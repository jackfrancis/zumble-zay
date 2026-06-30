// The runtime workload is identical across substrate launchers: the same image
// and the same injection-contract environment (docs/adr/0012). Only the workload
// wrapper differs (Job, bare Pod, ...). These thin wrappers map the launcher
// Config onto the substrate-neutral runtimespec helpers, so the Job and Pod
// launchers emit the identical runtime container and labels as any other
// substrate (the cross-substrate non-regression check).
package k8slauncher

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/runtimespec"
)

// runtimeOptions maps the launcher Config onto the substrate-neutral runtime
// settings shared by every substrate (docs/adr/0012).
func (c Config) runtimeOptions() runtimespec.Options {
	return runtimespec.Options{
		Image:             c.Image,
		ZZBaseURL:         c.ZZBaseURL,
		GitHubBaseURL:     c.GitHubBaseURL,
		ServiceAccount:    c.ServiceAccount,
		AIEndpoint:        c.AIEndpoint,
		AIModel:           c.AIModel,
		AITokenSecretName: c.AITokenSecretName,
		AITokenSecretKey:  c.AITokenSecretKey,
	}
}

// runtimeContainer is the runtime container the Job and Pod launchers run,
// identical to any other substrate (docs/adr/0012).
func runtimeContainer(cfg Config, spec orchestrator.JobSpec, token string) corev1.Container {
	return runtimespec.Container(cfg.runtimeOptions(), spec, token)
}

// runtimeLabels are the observability labels shared by runtime workloads, so
// jobs and pods are selectable the same way (e.g. by job-type or acting user).
func runtimeLabels(spec orchestrator.JobSpec) map[string]string {
	return runtimespec.Labels(spec)
}
