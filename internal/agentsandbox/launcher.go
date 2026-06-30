//go:build agent_sandbox

// This file is compiled only with `-tags agent_sandbox`, so the agent-sandbox
// substrate — and the client-go dynamic client it uses — is absent from the
// default build (docs/adr/0024, 0026). The build tag is the Go-identifier form of
// the substrate name (hyphens are not valid in build constraints); everything
// user-facing (the LAUNCHER value, the Handle kind, logs) is "agent-sandbox".
package agentsandbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/runtimespec"
)

const (
	sandboxAPIVersion = "agents.x-k8s.io/v1beta1"
	sandboxKind       = "Sandbox"
	// shutdownGrace is added past a job's deadline for the Sandbox's native
	// scheduled deletion, so a finished Sandbox self-reaps shortly after the job's
	// worst case rather than lingering — the Sandbox equivalent of a Job's TTL.
	shutdownGrace = 5 * time.Minute
)

// sandboxGVR is the agent-sandbox Sandbox resource (agents.x-k8s.io/v1beta1).
var sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// init registers the substrate so LAUNCHER=agent-sandbox selects it. It runs only
// when this package is blank-imported under the build tag (docs/adr/0024, 0026),
// so the registration — like the substrate — is absent from the default build.
func init() {
	launcher.Register("agent-sandbox", build)
}

// build constructs the launcher from the pod's in-cluster ServiceAccount. Like
// the other Kubernetes substrates it only works inside a cluster, failing fast
// otherwise (docs/adr/0012).
func build(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("agent-sandbox: in-cluster config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("agent-sandbox: dynamic client: %w", err)
	}
	opts := runtimespec.Options{
		Image:             cfg.Runtime.Image,
		ZZBaseURL:         cfg.Runtime.ZZBaseURL,
		ServiceAccount:    cfg.Runtime.ServiceAccount,
		AIEndpoint:        cfg.AI.Endpoint,
		AIModel:           cfg.AI.Model,
		AITokenSecretName: cfg.AI.TokenSecretName,
		AITokenSecretKey:  cfg.AI.TokenSecretKey,
	}
	if log != nil {
		log.Info("using agent-sandbox launcher (detached: completion via callback only)",
			"namespace", cfg.Runtime.Namespace, "image", opts.Image, "zz_base_url", opts.ZZBaseURL)
	}
	return &Launcher{client: dyn, namespace: cfg.Runtime.Namespace, opts: opts}, nil
}

// Launcher runs each agent job as an agent-sandbox Sandbox. It is detached: a
// Sandbox has no batch-style completion, so completion arrives from the runtime's
// callback (docs/adr/0025), and it needs only create RBAC on sandboxes — the
// Sandbox self-reaps via its scheduled deletion.
type Launcher struct {
	client    dynamic.Interface
	namespace string
	opts      runtimespec.Options
}

var (
	_ orchestrator.Launcher      = (*Launcher)(nil)
	_ orchestrator.AsyncLauncher = (*Launcher)(nil)
)

// Dispatch creates a Sandbox running the runtime image with the injection
// contract and returns a Handle naming it, without waiting (docs/adr/0024).
func (l *Launcher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	created, err := l.client.Resource(sandboxGVR).Namespace(l.namespace).
		Create(ctx, l.sandbox(spec, token, sandboxShutdown(ctx)), metav1.CreateOptions{})
	if err != nil {
		return orchestrator.Handle{Kind: "agent-sandbox"}, fmt.Errorf("create sandbox: %w", err)
	}
	return orchestrator.Handle{Kind: "agent-sandbox", Ref: created.GetName()}, nil
}

// Await is detached: a Sandbox has no batch-style completion, so it does not watch
// and instead waits for the per-job deadline (docs/adr/0025). Completion arrives
// via the runtime's callback, which the orchestrator races against this; reaching
// the deadline here means no report arrived in time (a timeout). It keys off the
// Handle alone, so the orchestrator can call it on its own goroutine (docs/adr/0024).
func (l *Launcher) Await(ctx context.Context, _ orchestrator.Handle) error {
	<-ctx.Done()
	return ctx.Err()
}

// Launch composes Dispatch and Await so a direct blocking call is available
// (docs/adr/0009); the orchestrator prefers the split async path (docs/adr/0024).
func (l *Launcher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	handle, err := l.Dispatch(ctx, spec, token)
	if err != nil {
		return handle, err
	}
	return handle, l.Await(ctx, handle)
}

// sandbox builds the Sandbox: the shared runtime PodSpec embedded in
// spec.podTemplate, plus a native scheduled-deletion so it self-reaps. Only the
// thin envelope is unstructured; the PodSpec is built typed by runtimespec, so the
// runtime container and injection contract are identical to every other substrate.
func (l *Launcher) sandbox(spec orchestrator.JobSpec, token string, shutdownAt time.Time) *unstructured.Unstructured {
	podSpec := runtimespec.PodSpec(l.opts, spec, token)
	podSpecMap, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&podSpec)

	u := &unstructured.Unstructured{}
	u.SetAPIVersion(sandboxAPIVersion)
	u.SetKind(sandboxKind)
	u.SetGenerateName("zz-" + string(spec.Type) + "-")
	u.SetNamespace(l.namespace)
	u.SetLabels(runtimespec.Labels(spec))
	_ = unstructured.SetNestedMap(u.Object, podSpecMap, "spec", "podTemplate", "spec")
	if !shutdownAt.IsZero() {
		_ = unstructured.SetNestedField(u.Object, shutdownAt.UTC().Format(time.RFC3339), "spec", "shutdownTime")
		_ = unstructured.SetNestedField(u.Object, "Delete", "spec", "shutdownPolicy")
	}
	return u
}

// sandboxShutdown returns when the Sandbox should self-delete: shortly past the
// job's deadline, so a finished Sandbox self-reaps. Zero when the context carries
// no deadline (then the Sandbox persists until externally cleaned up).
func sandboxShutdown(ctx context.Context) time.Time {
	dl, ok := ctx.Deadline()
	if !ok {
		return time.Time{}
	}
	return dl.Add(shutdownGrace)
}
