package opensandbox

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/runtimespec"
)

const (
	launcherName = "opensandbox"
	// handleKind labels the workload's location on the Handle (docs/adr/0012).
	handleKind = "opensandbox"
	// runtimeEntrypoint is the runtime image's command. OpenSandbox requires an
	// explicit entrypoint when creating from an image; it matches the runtime
	// Dockerfile's ENTRYPOINT ["/runtime"].
	runtimeEntrypoint = "/runtime"
	// shutdownGrace is added past a job's deadline for the sandbox's self-reap
	// timeout, so a finished sandbox cleans itself up shortly after the job's
	// worst case even if the prompt delete is missed — the sandbox equivalent of
	// a Job's TTL.
	shutdownGrace = 5 * time.Minute

	defaultCPU    = "500m"
	defaultMemory = "512Mi"
)

// init registers the substrate so LAUNCHER=opensandbox selects it (docs/adr/0024,
// 0027). The package self-registers and is activated by a blank import in
// cmd/orchestrator; it pulls no third-party module, so it needs no build tag.
func init() {
	launcher.Register(launcherName, build)
}

// Options are the runtime-shaping settings this launcher forwards to each
// sandbox. They are the substrate-neutral subset (mapped onto runtimespec) plus
// the sandbox resource limits. The ranking-model token is held as a value, not a
// Secret reference: OpenSandbox is remote, so the in-cluster secretKeyRef path
// does not apply. TODO(adr-0027): vend it through OpenSandbox's Credential Vault
// instead of forwarding the value.
type Options struct {
	Image      string
	ZZBaseURL  string
	AIEndpoint string
	AIModel    string
	AIToken    string
	CPU        string
	Memory     string
}

// Launcher runs each agent job as an OpenSandbox sandbox via the lifecycle API.
// It is detached: a sandbox has no batch-style completion to watch, so completion
// arrives from the runtime's callback (docs/adr/0025) and the per-job deadline is
// the backstop. It needs no Kubernetes client and no ZZ RBAC — OpenSandbox's own
// identity schedules the workload.
type Launcher struct {
	client *client
	opts   Options
	log    *slog.Logger
}

var (
	_ orchestrator.Launcher      = (*Launcher)(nil)
	_ orchestrator.AsyncLauncher = (*Launcher)(nil)
)

// build constructs the launcher from the OpenSandbox endpoint and API key, read
// here (not from the shared config) so this substrate adds nothing to
// internal/config and stays merge-clean with other concurrent substrates
// (docs/adr/0027). A missing endpoint or key fails fast, mirroring how the
// in-cluster launchers fail outside a cluster (docs/adr/0012).
func build(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	endpoint := strings.TrimRight(os.Getenv("OPENSANDBOX_ENDPOINT"), "/")
	apiKey := os.Getenv("OPENSANDBOX_API_KEY")
	if endpoint == "" || apiKey == "" {
		return nil, fmt.Errorf("opensandbox: OPENSANDBOX_ENDPOINT and OPENSANDBOX_API_KEY must be set")
	}
	opts := Options{
		Image:      cfg.Runtime.Image,
		ZZBaseURL:  cfg.Runtime.ZZBaseURL,
		AIEndpoint: cfg.AI.Endpoint,
		AIModel:    cfg.AI.Model,
		AIToken:    cfg.AI.Token,
		CPU:        getenvOr("OPENSANDBOX_CPU", defaultCPU),
		Memory:     getenvOr("OPENSANDBOX_MEMORY", defaultMemory),
	}
	if log != nil {
		log.Info("using opensandbox launcher (remote control plane; detached: completion via callback only)",
			"endpoint", endpoint, "image", opts.Image, "zz_base_url", opts.ZZBaseURL)
	}
	return &Launcher{
		client: newClient(endpoint, apiKey, &http.Client{Timeout: 30 * time.Second}),
		opts:   opts,
		log:    log,
	}, nil
}

// Dispatch creates a sandbox running the runtime image with the injection
// contract and returns a Handle naming it, without waiting (docs/adr/0024).
func (l *Launcher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	info, err := l.client.createSandbox(ctx, l.createRequest(ctx, spec, token))
	if err != nil {
		return orchestrator.Handle{Kind: handleKind}, fmt.Errorf("create sandbox: %w", err)
	}
	return orchestrator.Handle{Kind: handleKind, Ref: info.ID}, nil
}

// Await is detached: a sandbox has no batch-style completion, so it does not poll
// and instead waits for the per-job deadline (docs/adr/0025). Completion arrives
// via the runtime's callback, which the orchestrator races against this; reaching
// the deadline here means no report arrived in time (a timeout). On return — the
// job finished (callback) or timed out — it best-effort deletes the sandbox so it
// does not linger until its self-reap timeout. It keys off the Handle alone, so
// the orchestrator can call it on its own goroutine (docs/adr/0024).
func (l *Launcher) Await(ctx context.Context, handle orchestrator.Handle) error {
	<-ctx.Done()
	l.bestEffortDelete(handle.Ref)
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

// createRequest builds the POST /sandboxes body: the runtime image and its
// entrypoint, the shared ZZ_* injection map as the sandbox env (the cross-
// substrate contract — identical to every other substrate), the observability
// labels as metadata, and a self-reap timeout derived from the job deadline. The
// ranking-model token, absent from the shared map because it is a secret, is
// added here as a value (OpenSandbox is remote; the in-cluster Secret reference
// does not apply).
func (l *Launcher) createRequest(ctx context.Context, spec orchestrator.JobSpec, token string) createSandboxRequest {
	env := runtimespec.Env(runtimespec.Options{
		Image:      l.opts.Image,
		ZZBaseURL:  l.opts.ZZBaseURL,
		AIEndpoint: l.opts.AIEndpoint,
		AIModel:    l.opts.AIModel,
	}, spec, token)
	if l.opts.AIToken != "" {
		env[agent.EnvAIToken] = l.opts.AIToken
	}

	req := createSandboxRequest{
		Image:          &imageSpec{URI: l.opts.Image},
		Entrypoint:     []string{runtimeEntrypoint},
		ResourceLimits: map[string]string{"cpu": l.opts.CPU, "memory": l.opts.Memory},
		Env:            env,
		Metadata:       runtimespec.Labels(spec),
	}
	if secs := timeoutSeconds(ctx); secs > 0 {
		req.Timeout = &secs
	}
	return req
}

// bestEffortDelete schedules the sandbox for termination, logging (not failing)
// on error since the sandbox self-reaps via its timeout regardless. It uses a
// fresh short context because the caller's context is already done.
func (l *Launcher) bestEffortDelete(id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.client.deleteSandbox(ctx, id); err != nil && l.log != nil {
		l.log.Warn("opensandbox: best-effort delete failed", "sandbox", id, "err", err)
	}
}

// timeoutSeconds is the sandbox's self-reap TTL: the time left on the job's
// deadline plus a grace, so a finished sandbox cleans itself up shortly past the
// job's worst case. Zero when the context carries no deadline (then the sandbox
// has no TTL and persists until externally cleaned up).
func timeoutSeconds(ctx context.Context) int {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	secs := int((time.Until(dl) + shutdownGrace).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
