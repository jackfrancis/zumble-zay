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
	// execdPort is the in-sandbox port of OpenSandbox's execd agent, through which
	// the runtime command is started (docs/adr/0027).
	execdPort = 44772
	// runtimeCommand is the shell command execd runs inside the keep-alive sandbox
	// to start the agent runtime. The sandbox image must therefore carry a shell
	// and the /runtime binary — the distroless ZZ runtime image has neither, so a
	// shell-bearing variant is required (see OPENSANDBOX_RUNTIME_IMAGE).
	runtimeCommand = "/runtime"
	// shutdownGrace is added past a job's deadline for the sandbox's self-reap
	// timeout, so a finished sandbox cleans itself up shortly after the job's worst
	// case even if the prompt delete is missed — the sandbox equivalent of a Job's
	// TTL.
	shutdownGrace = 5 * time.Minute
	// readyTimeout bounds how long Dispatch waits for a freshly created sandbox to
	// reach Running before it can exec the runtime into it.
	readyTimeout = 90 * time.Second

	defaultCPU    = "500m"
	defaultMemory = "512Mi"
)

// keepAliveEntrypoint keeps the sandbox container alive so the runtime can be
// exec'd into it; OpenSandbox runs its execd agent alongside this. It is the
// platform's own default, set explicitly for clarity (docs/adr/0027).
var keepAliveEntrypoint = []string{"tail", "-f", "/dev/null"}

// init registers the substrate so LAUNCHER=opensandbox selects it (docs/adr/0024,
// 0027). The package self-registers and is activated by a blank import in
// cmd/orchestrator; it pulls no third-party module, so it needs no build tag.
func init() {
	launcher.Register(launcherName, build)
}

// Options are the runtime-shaping settings this launcher forwards to each sandbox.
// Image is the SANDBOX image: because OpenSandbox runs the runtime via `sh -c`
// inside a keep-alive container, it must carry a shell and the /runtime binary
// (not the distroless ZZ runtime image) — see OPENSANDBOX_RUNTIME_IMAGE. The
// ranking-model token is held as a value, not a Secret reference: OpenSandbox is
// remote, so the in-cluster secretKeyRef path does not apply.
// TODO(adr-0027): vend it through OpenSandbox's Credential Vault instead.
type Options struct {
	Image          string
	ZZBaseURL      string
	AIEndpoint     string
	AIModel        string
	AIToken        string
	CPU            string
	Memory         string
	UseServerProxy bool
}

// Launcher runs each agent job as an OpenSandbox sandbox via the lifecycle API.
// Because OpenSandbox sandboxes are long-lived, exec-into environments (not
// one-shot workloads), the runtime is not the container entrypoint: Dispatch
// creates a keep-alive sandbox, waits for it to be Running, then execs the runtime
// into it through execd (docs/adr/0027). Completion is detached: the runtime
// reports it via callback (docs/adr/0025) and the per-job deadline is the
// backstop. It needs no Kubernetes client and no ZZ RBAC — OpenSandbox's own
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
		// The sandbox image must be shell-bearing (OpenSandbox execs via sh -c), so
		// it is read separately and falls back to the configured runtime image only
		// when not overridden — the distroless default will not run here.
		Image:          getenvOr("OPENSANDBOX_RUNTIME_IMAGE", cfg.Runtime.Image),
		ZZBaseURL:      cfg.Runtime.ZZBaseURL,
		AIEndpoint:     cfg.AI.Endpoint,
		AIModel:        cfg.AI.Model,
		AIToken:        cfg.AI.Token,
		CPU:            getenvOr("OPENSANDBOX_CPU", defaultCPU),
		Memory:         getenvOr("OPENSANDBOX_MEMORY", defaultMemory),
		UseServerProxy: os.Getenv("OPENSANDBOX_USE_SERVER_PROXY") == "true",
	}
	if log != nil {
		log.Info("using opensandbox launcher (remote control plane; create keep-alive sandbox + exec runtime; detached completion via callback)",
			"endpoint", endpoint, "image", opts.Image, "zz_base_url", opts.ZZBaseURL, "use_server_proxy", opts.UseServerProxy)
	}
	return &Launcher{
		client: newClient(endpoint, apiKey, &http.Client{Timeout: 30 * time.Second}),
		opts:   opts,
		log:    log,
	}, nil
}

// Dispatch creates a keep-alive sandbox, waits for it to be Running, and execs the
// runtime into it (docs/adr/0027). The exec must happen here, not in Await,
// because only Dispatch receives the job spec and token the runtime needs; this
// means Dispatch holds a dispatch worker through sandbox provisioning. On any
// failure after creation it best-effort deletes the sandbox so none leaks.
func (l *Launcher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	info, err := l.client.createSandbox(ctx, l.createRequest(ctx, spec))
	if err != nil {
		return orchestrator.Handle{Kind: handleKind}, fmt.Errorf("create sandbox: %w", err)
	}
	handle := orchestrator.Handle{Kind: handleKind, Ref: info.ID}
	if err := l.startRuntime(ctx, info.ID, spec, token); err != nil {
		l.bestEffortDelete(info.ID)
		return handle, fmt.Errorf("start runtime: %w", err)
	}
	return handle, nil
}

// startRuntime waits for the sandbox to be Running, resolves its execd endpoint,
// and execs the runtime into it as a detached (background) command carrying the
// shared ZZ_* injection contract — identical to every other substrate, just
// delivered through execd rather than container env (docs/adr/0027). The
// ranking-model token, absent from the shared map because it is a secret, is added
// as a value (OpenSandbox is remote; the in-cluster Secret reference does not
// apply).
func (l *Launcher) startRuntime(ctx context.Context, id string, spec orchestrator.JobSpec, token string) error {
	if err := l.waitForRunning(ctx, id); err != nil {
		return err
	}
	ep, err := l.client.resolveEndpoint(ctx, id, execdPort, l.opts.UseServerProxy)
	if err != nil {
		return fmt.Errorf("resolve execd endpoint: %w", err)
	}
	env := runtimespec.Env(runtimespec.Options{
		Image:      l.opts.Image,
		ZZBaseURL:  l.opts.ZZBaseURL,
		AIEndpoint: l.opts.AIEndpoint,
		AIModel:    l.opts.AIModel,
	}, spec, token)
	if l.opts.AIToken != "" {
		env[agent.EnvAIToken] = l.opts.AIToken
	}
	return l.client.execCommand(ctx, execdURL(ep.Endpoint), ep.Headers, runCommandRequest{
		Command:    runtimeCommand,
		Background: true,
		Envs:       env,
	})
}

// waitForRunning polls the lifecycle API until the sandbox reaches Running, fails
// if it reaches a terminal state, or returns when readyTimeout (or the job
// deadline, whichever is sooner) elapses.
func (l *Launcher) waitForRunning(ctx context.Context, id string) error {
	wctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if info, err := l.client.getSandbox(wctx, id); err == nil {
			switch info.Status.State {
			case "Running":
				return nil
			case "Failed", "Terminated", "Stopping":
				return fmt.Errorf("sandbox %s entered terminal state %q", id, info.Status.State)
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("sandbox %s not Running before timeout: %w", id, wctx.Err())
		case <-ticker.C:
		}
	}
}

// Await is detached: the runtime runs as an exec inside the sandbox and reports
// completion via callback (docs/adr/0025), so Await has nothing to watch and waits
// for the per-job deadline (the backstop). On return — the job finished (callback)
// or timed out — it best-effort deletes the sandbox so it does not linger until
// its self-reap timeout. It keys off the Handle alone, so the orchestrator can
// call it on its own goroutine (docs/adr/0024).
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

// createRequest builds the POST /sandboxes body for a keep-alive sandbox: the
// shell-bearing runtime image, the keep-alive entrypoint (so the container stays
// up for the exec), resource limits, the observability labels as metadata, and a
// self-reap timeout from the job deadline. The ZZ_* injection is NOT set here — it
// rides the exec command in startRuntime, since the work runs as an exec, not the
// container entrypoint (docs/adr/0027).
func (l *Launcher) createRequest(ctx context.Context, spec orchestrator.JobSpec) createSandboxRequest {
	req := createSandboxRequest{
		Image:          &imageSpec{URI: l.opts.Image},
		Entrypoint:     keepAliveEntrypoint,
		ResourceLimits: map[string]string{"cpu": l.opts.CPU, "memory": l.opts.Memory},
		Metadata:       runtimespec.Labels(spec),
	}
	if secs := timeoutSeconds(ctx); secs > 0 {
		req.Timeout = &secs
	}
	return req
}

// bestEffortDelete schedules the sandbox for termination, logging (not failing) on
// error since the sandbox self-reaps via its timeout regardless. It uses a fresh
// short context because the caller's context is often already done.
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

// execdURL normalizes a resolved endpoint address into a base URL: the lifecycle
// API may return a bare host:port (or a server-proxy path) with no scheme, so it
// prepends http:// when absent (in-cluster execd is plain HTTP).
func execdURL(endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	return "http://" + endpoint
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
