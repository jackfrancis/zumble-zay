// Package k8slauncher runs each agent job as a Kubernetes batch/v1 Job and
// watches it to completion (docs/adr/0012). It is the reference per-substrate
// launcher, named for the resource it creates; sibling launchers (Pod, kagent,
// Kueue, sandbox) target other substrates behind the same orchestrator.Launcher
// interface. The agent runtime itself never imports a Kubernetes client — only
// this launcher (compiled into the server) does.
package k8slauncher

import (
	"context"
	"fmt"
	"regexp"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// Config configures the Kubernetes Job launcher.
type Config struct {
	Namespace      string // namespace the runtime Jobs are created in
	Image          string // runtime container image
	ZZBaseURL      string // in-cluster URL the runtime calls back (Service DNS)
	GitHubBaseURL  string // optional GitHub API base override
	ServiceAccount string // optional ServiceAccount for the runtime pod
	TTLAfterFinish int32  // ttlSecondsAfterFinished for completed Jobs
	// Ranking model wiring forwarded to the runtime (docs/adr/0011). Endpoint and
	// model are non-secret and ride the plain injection contract; the token is a
	// secret, injected by reference (AITokenSecretName/Key) rather than as a plain
	// env value, so it never appears in the Job spec.
	AIEndpoint        string
	AIModel           string
	AITokenSecretName string
	AITokenSecretKey  string
}

// KubernetesJobLauncher runs each agent job as a batch/v1 Job and watches it to
// completion. A Job is chosen for completion tracking, retry/backoff, and TTL
// cleanup, and is the Kueue-admissible unit (docs/adr/0012).
type KubernetesJobLauncher struct {
	client       kubernetes.Interface
	cfg          Config
	pollInterval time.Duration
}

var (
	_ orchestrator.Launcher      = (*KubernetesJobLauncher)(nil)
	_ orchestrator.AsyncLauncher = (*KubernetesJobLauncher)(nil)
)

// New builds a KubernetesJobLauncher over the given client.
func New(client kubernetes.Interface, cfg Config) *KubernetesJobLauncher {
	if cfg.TTLAfterFinish == 0 {
		cfg.TTLAfterFinish = 300
	}
	return &KubernetesJobLauncher{client: client, cfg: cfg, pollInterval: 2 * time.Second}
}

// Dispatch creates a Job that runs the runtime image with the injection contract
// (docs/adr/0012) and returns a Handle naming it, without waiting for it to
// finish (docs/adr/0024).
func (l *KubernetesJobLauncher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	created, err := l.client.BatchV1().Jobs(l.cfg.Namespace).Create(ctx, l.jobSpec(spec, token), metav1.CreateOptions{})
	if err != nil {
		return orchestrator.Handle{Kind: "k8s-job"}, fmt.Errorf("create job: %w", err)
	}
	return orchestrator.Handle{Kind: "k8s-job", Ref: created.Name}, nil
}

// Await watches the Job named by handle to completion. It keys off the Handle
// alone, so the orchestrator can call it on its own goroutine without shared
// launcher state (docs/adr/0024).
func (l *KubernetesJobLauncher) Await(ctx context.Context, handle orchestrator.Handle) error {
	return l.waitForCompletion(ctx, handle.Ref)
}

// Launch creates the Job and watches it to completion, composing Dispatch and
// Await so a direct blocking call is still available (docs/adr/0009); the
// orchestrator prefers the split async path (docs/adr/0024).
func (l *KubernetesJobLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	handle, err := l.Dispatch(ctx, spec, token)
	if err != nil {
		return handle, err
	}
	return handle, l.Await(ctx, handle)
}

func (l *KubernetesJobLauncher) jobSpec(spec orchestrator.JobSpec, token string) *batchv1.Job {
	backoff := int32(0)
	ttl := l.cfg.TTLAfterFinish
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zz-" + string(spec.Type) + "-",
			Namespace:    l.cfg.Namespace,
			Labels:       runtimeLabels(spec),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: l.cfg.ServiceAccount,
					Containers:         []corev1.Container{runtimeContainer(l.cfg, spec, token)},
				},
			},
		},
	}
}

// waitForCompletion polls the Job until it succeeds or fails (watch-to-completion).
func (l *KubernetesJobLauncher) waitForCompletion(ctx context.Context, name string) error {
	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()
	for {
		j, err := l.client.BatchV1().Jobs(l.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get job %s: %w", name, err)
		}
		switch {
		case j.Status.Succeeded > 0:
			return nil
		case j.Status.Failed > 0:
			return fmt.Errorf("job %s failed", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

var labelInvalid = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// sanitizeLabel makes s a valid Kubernetes label value (<=63 chars, limited
// charset), for observability labels like the acting user.
func sanitizeLabel(s string) string {
	s = labelInvalid.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}
