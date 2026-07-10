package substrate

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

const (
	launcherName = "substrate"
	// handleKind labels the workload's location on the Handle (docs/adr/0012).
	handleKind = "substrate"
	// stateFailed is the A2A task state the actor returns when it rejects a job
	// synchronously — before running it — e.g. for malformed parameters. Any other
	// state means the job was accepted and now runs detached (docs/adr/0025).
	stateFailed = "failed"

	// defaultRouterURL is the in-cluster Agent Substrate atenet-router root. It is
	// an FQDN because the orchestrator and ate-system run in different namespaces
	// (the cross-namespace DNS lesson of docs/adr/0027). The router listens on :80.
	defaultRouterURL = "http://atenet-router.ate-system.svc.cluster.local"
	// defaultAtespace / defaultActor identify the standing ZZ runtime actor,
	// provisioned out-of-band (deploy/k8s/substrate + `kubectl ate`), and
	// defaultDNSSuffix is Substrate's actor DNS zone. The actor host the router
	// routes on is "<actor>.<atespace>.<suffix>"; a full SUBSTRATE_ACTOR_HOST
	// override wins over the composed value, as the zone is alpha and may drift.
	defaultAtespace  = "zumble-zay"
	defaultActor     = "zz-runtime"
	defaultDNSSuffix = "actors.resources.substrate.ate.dev"
)

// init registers the substrate so LAUNCHER=substrate selects it (docs/adr/0024,
// 0035). The package self-registers and is activated by a blank import in
// cmd/orchestrator; it pulls no third-party module (net/http only), so it needs no
// build tag — the actor-lifecycle gRPC is deliberately off this path (docs/adr/0035).
func init() {
	launcher.Register(launcherName, build)
}

// Launcher dispatches each agent job to a durable Agent Substrate actor over HTTP
// through the atenet-router rather than spawning a workload per job (docs/adr/0035).
// The actor is a long-lived runtime (the cmd/runtime-a2a image) that Substrate
// multiplexes and suspends/resumes; this launcher POSTs one A2A message/send per
// job through the router, which auto-resumes the actor and proxies to it. It holds
// no Kubernetes client and needs no ZZ RBAC — Substrate owns the actor lifecycle.
type Launcher struct {
	client    *client
	actorHost string
	log       *slog.Logger
}

var (
	_ orchestrator.Launcher          = (*Launcher)(nil)
	_ orchestrator.AsyncLauncher     = (*Launcher)(nil)
	_ orchestrator.PullTokenLauncher = (*Launcher)(nil)
)

// build constructs the launcher from SUBSTRATE_* environment, read here (not from
// the shared config) so this substrate adds nothing to internal/config and stays
// merge-clean with other concurrent substrates (docs/adr/0024, 0027, 0035). The
// defaults target a standard ate-system install with the ZZ actor in the
// "zumble-zay" atespace, so LAUNCHER=substrate works out of the box; each is
// overridable. The static runtime configuration (ZZ base URL, model
// endpoint/token) is deliberately not read here — it lives on the actor's
// ActorTemplate (inside its golden snapshot), not on each dispatch.
func build(_ *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	routerURL := getenvOr("SUBSTRATE_ROUTER_URL", defaultRouterURL)
	host := os.Getenv("SUBSTRATE_ACTOR_HOST")
	if host == "" {
		atespace := getenvOr("SUBSTRATE_ATESPACE", defaultAtespace)
		actor := getenvOr("SUBSTRATE_ACTOR", defaultActor)
		suffix := getenvOr("SUBSTRATE_DNS_SUFFIX", defaultDNSSuffix)
		host = strings.Join([]string{actor, atespace, suffix}, ".")
	}
	if log != nil {
		log.Info("using substrate launcher (dispatch to a durable actor through the atenet-router; job params ride the task metadata)",
			"router", strings.TrimRight(routerURL, "/"), "actor", host)
	}
	return &Launcher{
		client:    newClient(routerURL, &http.Client{}), // no client timeout: bounded by the per-job ctx
		actorHost: host,
		log:       log,
	}, nil
}

// Dispatch sends the job to the durable actor as one A2A message/send through the
// router and returns as soon as the actor accepts it. The router auto-resumes a
// suspended actor on this traffic and proxies to it; the actor runs the job
// detached and reports its terminal outcome to ZZ through the completion callback,
// which the orchestrator races against Await (docs/adr/0025, 0035). The send is
// non-blocking, so the actor acknowledges immediately instead of holding the
// connection until the job finishes. The job parameters — job type, provider, any
// item id, and a single-use redemption ticket in place of the token
// (docs/adr/0030) — ride the message metadata: the actor's env is frozen in its
// golden snapshot, so per-job values cannot ride env, and carrying a ticket rather
// than the token keeps the live credential out of any snapshot of the actor's RAM.
func (l *Launcher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, ticket string) (orchestrator.Handle, error) {
	if l.log != nil {
		l.log.Info("substrate dispatch", "job", spec.JobID, "type", spec.Type, "actor", l.actorHost)
	}
	prompt := "Run Zumble-Zay job " + string(spec.Type)
	res, err := l.client.sendTask(ctx, l.actorHost, prompt, taskMetadata(spec, ticket))
	handle := orchestrator.Handle{Kind: handleKind, Ref: res.TaskID}
	if err != nil {
		return handle, fmt.Errorf("substrate dispatch %s: %w", spec.Type, err)
	}
	// The actor rejects a job synchronously only before it starts running (a bad
	// request), returning a failed task; any other state means it was accepted and
	// is now running detached, so completion arrives via the callback.
	if res.State == stateFailed {
		return handle, fmt.Errorf("substrate actor rejected %s: %s", spec.Type, res.Message)
	}
	return handle, nil
}

// Await backstops completion with the per-job deadline: the real outcome arrives
// via the runtime's completion callback, which the orchestrator races against
// this (docs/adr/0025, 0035). It keys off ctx alone — nothing to reap, since the
// actor is durable and Substrate owns its suspend/resume lifecycle — so the
// orchestrator can run it on its own goroutine.
func (l *Launcher) Await(ctx context.Context, _ orchestrator.Handle) error {
	<-ctx.Done()
	return ctx.Err()
}

// Launch composes Dispatch and Await so a direct blocking call is available
// (docs/adr/0009); the orchestrator prefers the split async path so completion is
// driven by the callback rather than by the deadline (docs/adr/0024).
func (l *Launcher) Launch(ctx context.Context, spec orchestrator.JobSpec, ticket string) (orchestrator.Handle, error) {
	handle, err := l.Dispatch(ctx, spec, ticket)
	if err != nil {
		return handle, err
	}
	return handle, l.Await(ctx, handle)
}

// PullsToken marks substrate as a pull substrate (docs/adr/0030): its runtime
// redeems a single-use ticket for the job token, so the orchestrator hands
// Dispatch a ticket rather than the token. This matters more here than anywhere
// else — Substrate snapshots the actor's entire RAM to object storage on suspend,
// so a live token in the runtime's memory would persist at rest; a single-use,
// short-TTL ticket redeemed at the start of the active run does not.
func (l *Launcher) PullsToken() bool { return true }

// taskMetadata is the per-job subset of the injection contract carried in the A2A
// message metadata. Only per-job values travel here — job type, provider, item,
// and a single-use redemption ticket in place of the token — because the actor
// already holds the static config (ZZ base URL, model endpoint/token) in its
// ActorTemplate environment. The keys are the canonical agent.Env* names, so the
// actor's decoder (agent.ParamsFromEnv, via cmd/runtime-a2a) reconstructs
// RunParams without any drift; the runtime redeems the ticket for the job token
// first (docs/adr/0030). Empty optional values are omitted so they cannot shadow
// the actor's environment.
func taskMetadata(spec orchestrator.JobSpec, ticket string) map[string]string {
	m := map[string]string{
		agent.EnvJobType: string(spec.Type),
		agent.EnvTicket:  ticket,
	}
	if spec.Provider != "" {
		m[agent.EnvProvider] = spec.Provider
	}
	if spec.ItemID != "" {
		m[agent.EnvItemID] = spec.ItemID
	}
	if v := agent.DispatchedAtValue(spec.DispatchedAt); v != "" {
		m[agent.EnvDispatchedAt] = v
	}
	return m
}

func getenvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
