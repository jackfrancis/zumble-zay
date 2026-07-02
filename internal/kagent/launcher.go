package kagent

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
	launcherName = "kagent"
	// handleKind labels the workload's location on the Handle (docs/adr/0012).
	handleKind = "kagent"
	// stateFailed is the A2A task state the agent returns when it rejects a job
	// synchronously — before running it — e.g. for malformed parameters. Any other
	// state means the job was accepted and now runs detached (docs/adr/0025).
	stateFailed = "failed"

	// defaultEndpoint is the in-cluster kagent controller A2A address. It is an
	// FQDN because the orchestrator and the kagent controller run in different
	// namespaces, where a short "service.namespace" would resolve but the bare
	// service name would not (the cross-namespace DNS lesson of docs/adr/0027).
	defaultEndpoint  = "http://kagent-controller.kagent.svc.cluster.local:8083"
	defaultNamespace = "kagent"
	defaultAgentName = "zz-runtime"
)

// init registers the substrate so LAUNCHER=kagent selects it (docs/adr/0024). The
// package self-registers and is activated by a blank import in cmd/orchestrator;
// it pulls no third-party module, so it needs no build tag.
func init() {
	launcher.Register(launcherName, build)
}

// Launcher dispatches each agent job to a durable kagent Agent over A2A rather
// than spawning a workload per job (docs/adr/0024). The runtime is a long-lived
// BYO Agent served by cmd/runtime-a2a; this launcher POSTs one A2A message/send
// per job to the kagent controller, which routes it to that agent. It holds no
// Kubernetes client and needs no ZZ RBAC — kagent owns the workload's lifecycle.
type Launcher struct {
	client    *client
	namespace string
	agentName string
	log       *slog.Logger
}

var (
	_ orchestrator.Launcher          = (*Launcher)(nil)
	_ orchestrator.AsyncLauncher     = (*Launcher)(nil)
	_ orchestrator.PullTokenLauncher = (*Launcher)(nil)
)

// build constructs the launcher from KAGENT_* environment, read here (not from
// the shared config) so this substrate adds nothing to internal/config and stays
// merge-clean with other concurrent substrates (docs/adr/0024, 0027). All three
// settings default to a standard kagent install, so LAUNCHER=kagent works out of
// the box; each is overridable. The static runtime configuration (ZZ base URL,
// model endpoint/token) is deliberately not read here — it lives on the durable
// agent's Deployment, not on each dispatch.
func build(_ *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	endpoint := strings.TrimRight(getenvOr("KAGENT_ENDPOINT", defaultEndpoint), "/")
	namespace := getenvOr("KAGENT_AGENT_NAMESPACE", defaultNamespace)
	agentName := getenvOr("KAGENT_AGENT_NAME", defaultAgentName)
	if log != nil {
		log.Info("using kagent launcher (dispatch to a durable Agent over A2A; job params ride the task metadata)",
			"endpoint", endpoint, "agent", namespace+"/"+agentName)
	}
	return &Launcher{
		client:    newClient(endpoint, &http.Client{}), // no client timeout: bounded by the per-job ctx
		namespace: namespace,
		agentName: agentName,
		log:       log,
	}, nil
}

// Dispatch sends the job to the durable agent as one A2A message/send and returns
// as soon as the agent accepts it. The agent runs the job detached and reports
// its terminal outcome to ZZ through the completion callback, which the
// orchestrator races against Await (docs/adr/0024, 0025). The send is
// non-blocking, so the kagent controller acknowledges immediately instead of
// holding the connection until the job finishes — this is what keeps a long job
// (a real converse review) from tripping the controller's synchronous-proxy
// timeout. The job parameters — job type, provider, any item id, and a single-use
// redemption ticket in place of the token (docs/adr/0029) — ride the message
// metadata, the channel a kagent controller forwards intact to the agent (HTTP
// headers are stripped, so metadata is the only reliable carrier). Carrying a
// ticket, not the token, keeps the live credential out of the controller's
// persisted task history; the runtime redeems it for the token before running.
func (l *Launcher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, ticket string) (orchestrator.Handle, error) {
	if l.log != nil {
		l.log.Info("kagent dispatch", "job", spec.JobID, "type", spec.Type, "agent", l.namespace+"/"+l.agentName)
	}
	prompt := "Run Zumble-Zay job " + string(spec.Type)
	res, err := l.client.sendTask(ctx, l.namespace, l.agentName, prompt, taskMetadata(spec, ticket))
	handle := orchestrator.Handle{Kind: handleKind, Ref: res.TaskID}
	if err != nil {
		return handle, fmt.Errorf("kagent dispatch %s: %w", spec.Type, err)
	}
	// The agent rejects a job synchronously only before it starts running (a bad
	// request), returning a failed task; any other state means it was accepted and
	// is now running detached, so completion arrives via the callback.
	if res.State == stateFailed {
		return handle, fmt.Errorf("kagent rejected %s: %s", spec.Type, res.Message)
	}
	return handle, nil
}

// Await backstops completion with the per-job deadline: the real outcome arrives
// via the runtime's completion callback, which the orchestrator races against
// this (docs/adr/0024, 0025). It keys off ctx alone — nothing to reap, since the
// kagent Agent is durable and the A2A task is just a record in kagent's store —
// so the orchestrator can run it on its own goroutine.
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

// PullsToken marks kagent as a pull substrate (docs/adr/0029): its runtime
// redeems a single-use ticket for the job token, so the orchestrator hands
// Dispatch a ticket rather than the token. This is what keeps the live token out
// of kagent's persisted task history — the durable controller stores the task
// metadata, so a single-use, short-TTL ticket is the right thing to leave there.
func (l *Launcher) PullsToken() bool { return true }

// taskMetadata is the per-job subset of the injection contract carried in the A2A
// message metadata. Only per-job values travel here — job type, provider, item,
// and a single-use redemption ticket in place of the token — because the durable
// agent already holds the static config (ZZ base URL, model endpoint/token) in
// its Deployment environment. The keys are the canonical agent.Env* names, so the
// agent's decoder (agent.ParamsFromEnv, via cmd/runtime-a2a) reconstructs
// RunParams without any drift; the runtime redeems the ticket for the job token
// first (docs/adr/0029). Empty optional values are omitted so they cannot shadow
// the agent's environment.
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
	return m
}

func getenvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
