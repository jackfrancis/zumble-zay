// Package k8slauncher's Pod launcher runs each agent job as a bare core/v1 Pod
// (docs/adr/0012).
package k8slauncher

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// KubernetesPodLauncher runs each agent job as a bare core/v1 Pod. By default it
// watches the Pod to a terminal phase; in detached mode it does not watch at all
// and relies on the runtime's completion callback (docs/adr/0025). Compared with
// the Job launcher it is the minimal substrate: no retry/backoff and no
// ttlSecondsAfterFinished GC (a bare Pod is not owned by a controller). That
// lifecycle contrast — the launcher itself must reap terminal Pods — is exactly
// what makes it a useful comparison point (docs/adr/0012).
type KubernetesPodLauncher struct {
	client       kubernetes.Interface
	cfg          Config
	pollInterval time.Duration
	// detached makes Await skip the Pod watch and wait for the per-job deadline,
	// so completion arrives solely from the runtime's callback (docs/adr/0025).
	detached bool
}

var (
	_ orchestrator.Launcher      = (*KubernetesPodLauncher)(nil)
	_ orchestrator.AsyncLauncher = (*KubernetesPodLauncher)(nil)
)

// NewPodLauncher builds a KubernetesPodLauncher over the given client.
func NewPodLauncher(client kubernetes.Interface, cfg Config) *KubernetesPodLauncher {
	return &KubernetesPodLauncher{client: client, cfg: cfg, pollInterval: 2 * time.Second}
}

// NewDetachedPodLauncher builds a Pod launcher that does NOT watch the Pod:
// completion comes solely from the runtime's callback (docs/adr/0025), with the
// orchestrator's per-job deadline as the only backstop. It is the runnable
// reference for a fully-detached substrate — dispatch and rely on the callback,
// the shape kagent / Ray / an external runtime would take — using a real
// in-cluster workload, and it needs only Pod-create RBAC, not get/list/watch.
func NewDetachedPodLauncher(client kubernetes.Interface, cfg Config) *KubernetesPodLauncher {
	l := NewPodLauncher(client, cfg)
	l.detached = true
	return l
}

// kind labels the Handle (and is the registry-facing substrate name) so a
// detached Pod is distinguishable from a watched one in logs and job handles.
func (l *KubernetesPodLauncher) kind() string {
	if l.detached {
		return "k8s-pod-detached"
	}
	return "k8s-pod"
}

// Dispatch creates a Pod running the runtime image with the injection contract
// and returns a Handle naming it, without waiting for it to finish
// (docs/adr/0024).
func (l *KubernetesPodLauncher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	created, err := l.client.CoreV1().Pods(l.cfg.Namespace).Create(ctx, l.podSpec(spec, token), metav1.CreateOptions{})
	if err != nil {
		return orchestrator.Handle{Kind: l.kind()}, fmt.Errorf("create pod: %w", err)
	}
	return orchestrator.Handle{Kind: l.kind(), Ref: created.Name}, nil
}

// Await watches the Pod named by handle to a terminal phase — unless the launcher
// is detached, in which case it does not watch at all and instead waits for the
// per-job deadline (docs/adr/0025): completion arrives via the runtime's
// callback, which the orchestrator races against this, so reaching the deadline
// here means no report arrived in time (a timeout). It keys off the Handle alone,
// so the orchestrator can call it on its own goroutine (docs/adr/0024).
func (l *KubernetesPodLauncher) Await(ctx context.Context, handle orchestrator.Handle) error {
	if l.detached {
		<-ctx.Done()
		return ctx.Err()
	}
	return l.waitForCompletion(ctx, handle.Ref)
}

// Launch creates the Pod and watches it to a terminal phase, composing Dispatch
// and Await so a direct blocking call is still available (docs/adr/0009); the
// orchestrator prefers the split async path (docs/adr/0024).
func (l *KubernetesPodLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	handle, err := l.Dispatch(ctx, spec, token)
	if err != nil {
		return handle, err
	}
	return handle, l.Await(ctx, handle)
}

func (l *KubernetesPodLauncher) podSpec(spec orchestrator.JobSpec, token string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zz-" + string(spec.Type) + "-",
			Namespace:    l.cfg.Namespace,
			Labels:       runtimeLabels(spec),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: l.cfg.ServiceAccount,
			Containers:         []corev1.Container{runtimeContainer(l.cfg, spec, token)},
		},
	}
}

// waitForCompletion polls the Pod until it reaches a terminal phase. Unlike a
// Job there is no controller to surface completion, so the phase is the source
// of truth.
func (l *KubernetesPodLauncher) waitForCompletion(ctx context.Context, name string) error {
	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()
	for {
		p, err := l.client.CoreV1().Pods(l.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get pod %s: %w", name, err)
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s failed: %s", name, p.Status.Reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
