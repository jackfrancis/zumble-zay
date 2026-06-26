// The runtime workload is identical across substrate launchers: the same image
// and the same injection-contract environment (docs/adr/0012). Only the
// workload wrapper differs (Job, bare Pod, ...). These helpers keep that shared
// shape in one place so the launchers stay directly comparable.
package k8slauncher

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// runtimeEnvVars builds the injection-contract environment for the runtime
// container from the job spec and its minted token.
func runtimeEnvVars(cfg Config, spec orchestrator.JobSpec, token string) []corev1.EnvVar {
	env := agent.Env(agent.RunParams{
		JobType:       string(spec.Type),
		BaseURL:       cfg.ZZBaseURL,
		Token:         token,
		Provider:      spec.Provider,
		GitHubBaseURL: cfg.GitHubBaseURL,
		AIEndpoint:    cfg.AIEndpoint,
		AIModel:       cfg.AIModel,
	})
	envVars := make([]corev1.EnvVar, 0, len(env))
	for k, v := range env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}
	return envVars
}

// runtimeContainer is the single runtime container every substrate launcher
// runs; only the surrounding workload (Job, Pod, ...) differs. The ranking
// model token, when configured, is injected by Secret reference so it never
// appears as a plain value in the workload spec.
func runtimeContainer(cfg Config, spec orchestrator.JobSpec, token string) corev1.Container {
	env := runtimeEnvVars(cfg, spec, token)
	if cfg.AITokenSecretName != "" {
		optional := true
		env = append(env, corev1.EnvVar{
			Name: agent.EnvAIToken,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.AITokenSecretName},
					Key:                  cfg.AITokenSecretKey,
					Optional:             &optional,
				},
			},
		})
	}
	return corev1.Container{
		Name:            "runtime",
		Image:           cfg.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             env,
	}
}

// runtimeLabels are the observability labels shared by runtime workloads, so
// jobs and pods are selectable the same way (e.g. by job-type or acting user).
func runtimeLabels(spec orchestrator.JobSpec) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "zumble-zay-runtime",
		"zumble-zay.dev/job-type":    string(spec.Type),
		"zumble-zay.dev/acting-user": sanitizeLabel(spec.ActingUserID),
	}
}
