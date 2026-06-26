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

// KubernetesPodLauncher runs each agent job as a bare core/v1 Pod and watches
// it to a terminal phase. Compared with the Job launcher it is the minimal
// substrate: no retry/backoff and no ttlSecondsAfterFinished GC (a bare Pod is
// not owned by a controller), and status is read straight from the Pod phase.
// That lifecycle contrast — the launcher itself must reap terminal Pods — is
// exactly what makes it a useful comparison point (docs/adr/0012).
type KubernetesPodLauncher struct {
	client       kubernetes.Interface
	cfg          Config
	pollInterval time.Duration
}

var _ orchestrator.Launcher = (*KubernetesPodLauncher)(nil)

// NewPodLauncher builds a KubernetesPodLauncher over the given client.
func NewPodLauncher(client kubernetes.Interface, cfg Config) *KubernetesPodLauncher {
	return &KubernetesPodLauncher{client: client, cfg: cfg, pollInterval: 2 * time.Second}
}

// Launch creates a Pod running the runtime image with the injection contract,
// watches it to a terminal phase, and returns a Handle naming the Pod.
func (l *KubernetesPodLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	created, err := l.client.CoreV1().Pods(l.cfg.Namespace).Create(ctx, l.podSpec(spec, token), metav1.CreateOptions{})
	if err != nil {
		return orchestrator.Handle{Kind: "k8s-pod"}, fmt.Errorf("create pod: %w", err)
	}
	return orchestrator.Handle{Kind: "k8s-pod", Ref: created.Name}, l.waitForCompletion(ctx, created.Name)
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
