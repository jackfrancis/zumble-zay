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

	"github.com/jackfrancis/zumble-zay/internal/agent"
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
}

// KubernetesJobLauncher runs each agent job as a batch/v1 Job and watches it to
// completion. A Job is chosen for completion tracking, retry/backoff, and TTL
// cleanup, and is the Kueue-admissible unit (docs/adr/0012).
type KubernetesJobLauncher struct {
	client       kubernetes.Interface
	cfg          Config
	pollInterval time.Duration
}

var _ orchestrator.Launcher = (*KubernetesJobLauncher)(nil)

// New builds a KubernetesJobLauncher over the given client.
func New(client kubernetes.Interface, cfg Config) *KubernetesJobLauncher {
	if cfg.TTLAfterFinish == 0 {
		cfg.TTLAfterFinish = 300
	}
	return &KubernetesJobLauncher{client: client, cfg: cfg, pollInterval: 2 * time.Second}
}

// Launch creates a Job that runs the runtime image with the injection contract
// (docs/adr/0012), watches it to completion, and returns a Handle naming the Job.
func (l *KubernetesJobLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	created, err := l.client.BatchV1().Jobs(l.cfg.Namespace).Create(ctx, l.jobSpec(spec, token), metav1.CreateOptions{})
	if err != nil {
		return orchestrator.Handle{Kind: "k8s-job"}, fmt.Errorf("create job: %w", err)
	}
	return orchestrator.Handle{Kind: "k8s-job", Ref: created.Name}, l.waitForCompletion(ctx, created.Name)
}

func (l *KubernetesJobLauncher) jobSpec(spec orchestrator.JobSpec, token string) *batchv1.Job {
	env := agent.Env(agent.RunParams{
		JobType:       string(spec.Type),
		BaseURL:       l.cfg.ZZBaseURL,
		Token:         token,
		Provider:      spec.Provider,
		GitHubBaseURL: l.cfg.GitHubBaseURL,
	})
	envVars := make([]corev1.EnvVar, 0, len(env))
	for k, v := range env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}
	backoff := int32(0)
	ttl := l.cfg.TTLAfterFinish
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zz-" + string(spec.Type) + "-",
			Namespace:    l.cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "zumble-zay-runtime",
				"zumble-zay.dev/job-type":    string(spec.Type),
				"zumble-zay.dev/acting-user": sanitizeLabel(spec.ActingUserID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: l.cfg.ServiceAccount,
					Containers: []corev1.Container{{
						Name:            "runtime",
						Image:           l.cfg.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env:             envVars,
					}},
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
