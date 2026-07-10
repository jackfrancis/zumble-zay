// Package runtimespec builds the Kubernetes workload pieces every agent-runtime
// substrate shares: the runtime container, its injection-contract environment,
// and the observability labels (docs/adr/0012). It is a substrate-neutral package
// depended on by every launcher (k8slauncher, agentsandbox, …) so they all emit
// an identical runtime shape — the cross-substrate non-regression check — and so a
// substrate can live in its own package without importing a sibling launcher.
package runtimespec

import (
	"regexp"

	corev1 "k8s.io/api/core/v1"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// Options are the runtime-shaping settings a substrate forwards to the runtime
// container — the substrate-neutral subset of a launcher's configuration. A
// launcher maps its own config onto this so every substrate runs the same image
// with the same injection contract.
type Options struct {
	Image             string // runtime container image
	ZZBaseURL         string // in-cluster URL the runtime calls back (Service DNS)
	GitHubBaseURL     string // optional GitHub API base override
	ServiceAccount    string // optional ServiceAccount for the runtime pod
	AIEndpoint        string // chat-completions endpoint forwarded to the ranker
	AIModel           string // ranking model id
	AITokenSecretName string // Secret holding the ranking-model token (injected by reference)
	AITokenSecretKey  string // key within that Secret
}

// Env builds the substrate-neutral ZZ_* injection map from the job spec and its
// minted token (docs/adr/0012). It is the contract every substrate emits: the
// Kubernetes launchers wrap it as container env (EnvVars), while a remote-control-
// plane substrate (e.g. opensandbox, docs/adr/0027) forwards the map straight to
// its create API. The ranking-model token is deliberately absent — it is a secret
// a launcher injects out-of-band (a Secret reference in cluster; a credential
// path remotely), so it never rides this shared map.
func Env(opts Options, spec orchestrator.JobSpec, token string) map[string]string {
	return agent.Env(agent.RunParams{
		JobType:       string(spec.Type),
		BaseURL:       opts.ZZBaseURL,
		Token:         token,
		Provider:      spec.Provider,
		ItemID:        spec.ItemID,
		GitHubBaseURL: opts.GitHubBaseURL,
		AIEndpoint:    opts.AIEndpoint,
		AIModel:       opts.AIModel,
		DispatchedAt:  spec.DispatchedAt,
	})
}

// EnvVars builds the injection-contract environment for the runtime container
// from the job spec and its minted token (docs/adr/0012). The ranking-model token
// is a secret, so it is appended by Secret reference rather than as a plain value,
// so it never appears in the workload spec.
func EnvVars(opts Options, spec orchestrator.JobSpec, token string) []corev1.EnvVar {
	env := Env(opts, spec, token)
	envVars := make([]corev1.EnvVar, 0, len(env)+1)
	for k, v := range env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}
	if opts.AITokenSecretName != "" {
		optional := true
		envVars = append(envVars, corev1.EnvVar{
			Name: agent.EnvAIToken,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: opts.AITokenSecretName},
					Key:                  opts.AITokenSecretKey,
					Optional:             &optional,
				},
			},
		})
	}
	return envVars
}

// Container is the single runtime container every substrate runs; only the
// surrounding workload (Job, Pod, Sandbox, …) differs.
func Container(opts Options, spec orchestrator.JobSpec, token string) corev1.Container {
	return corev1.Container{
		Name:            "runtime",
		Image:           opts.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             EnvVars(opts, spec, token),
	}
}

// PodSpec is the runtime pod every substrate runs: one runtime container, no
// restart (a job runs once), under the configured ServiceAccount. The Job and Pod
// launchers wrap their own template around the container; a Sandbox embeds this
// PodSpec directly in spec.podTemplate.
func PodSpec(opts Options, spec orchestrator.JobSpec, token string) corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyNever,
		ServiceAccountName: opts.ServiceAccount,
		Containers:         []corev1.Container{Container(opts, spec, token)},
	}
}

// Labels are the observability labels shared by runtime workloads, so every
// substrate's workload is selectable the same way (by job-type or acting user).
func Labels(spec orchestrator.JobSpec) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "zumble-zay-runtime",
		"zumble-zay.dev/job-type":    string(spec.Type),
		"zumble-zay.dev/acting-user": sanitizeLabel(spec.ActingUserID),
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
