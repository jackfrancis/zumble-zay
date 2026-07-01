//go:build ray

// This file is compiled only with `-tags ray`, so the Ray/KubeRay substrate —
// and the client-go dynamic client it uses — is absent from the default build
// (docs/adr/0024, 0028). The build tag is the Go-identifier form of the substrate
// name; everything user-facing (the LAUNCHER value, the Handle kind, logs) is
// "ray".
package raylauncher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/runtimespec"
)

const (
	rayAPIVersion = "ray.io/v1"
	rayKind       = "RayJob"
	// clusterSelectorKey is the label KubeRay matches a RayJob to a standing
	// RayCluster on (spec.clusterSelector).
	clusterSelectorKey = "ray.io/cluster"
	// defaultEntrypoint is the runtime binary the RayJob runs on the cluster; the
	// RayCluster image must provide it (docs/adr/0028).
	defaultEntrypoint = "/runtime"
	// actorsEntrypoint runs the Ray-actors llm-rank program instead of /runtime,
	// used when the launcher is in actor mode for llm-rank jobs (docs/adr/0029).
	// The RayCluster image must provide it (baked by deploy/ray/Dockerfile.ray).
	actorsEntrypoint = "python /llm_rank_ray.py"
	// llmRankJobType is the JobType whose scoring the actor path parallelizes
	// across the cluster (mirrors orchestrator.JobLLMRank).
	llmRankJobType    = "llm-rank"
	defaultTTLSeconds = int64(300)
)

// rayJobGVR is the KubeRay RayJob resource (ray.io/v1).
var rayJobGVR = schema.GroupVersionResource{Group: "ray.io", Version: "v1", Resource: "rayjobs"}

// init registers the substrate so LAUNCHER=ray selects it. It runs only when this
// package is blank-imported under the build tag (docs/adr/0024, 0028), so the
// registration — like the substrate — is absent from the default build.
func init() {
	launcher.Register("ray", build)
}

// build constructs the launcher from the pod's in-cluster ServiceAccount. Like
// the other Kubernetes substrates it only works inside a cluster, failing fast
// otherwise; it additionally requires RAY_CLUSTER to name the standing RayCluster
// (docs/adr/0028).
func build(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	cluster := strings.TrimSpace(os.Getenv("RAY_CLUSTER"))
	if cluster == "" {
		return nil, fmt.Errorf("ray: RAY_CLUSTER must name the standing RayCluster")
	}
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("ray: in-cluster config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("ray: dynamic client: %w", err)
	}

	namespace := strings.TrimSpace(os.Getenv("RAY_NAMESPACE"))
	if namespace == "" {
		namespace = cfg.Runtime.Namespace
	}
	entrypoint := strings.TrimSpace(os.Getenv("RAY_RUNTIME_ENTRYPOINT"))
	if entrypoint == "" {
		entrypoint = defaultEntrypoint
	}
	ttl := defaultTTLSeconds
	if v := strings.TrimSpace(os.Getenv("RAY_JOB_TTL_SECONDS")); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			ttl = n
		}
	}
	// Actor mode: run llm-rank as a Ray-actors Python program that parallelizes
	// scoring across the cluster, instead of the /runtime batch binary
	// (docs/adr/0029). Opt-in and scoped to llm-rank; every other job type still
	// runs /runtime.
	llmRankActors := strings.EqualFold(strings.TrimSpace(os.Getenv("RAY_LLM_RANK_ACTORS")), "true")

	opts := runtimespec.Options{
		Image:          cfg.Runtime.Image,
		ZZBaseURL:      cfg.Runtime.ZZBaseURL,
		ServiceAccount: cfg.Runtime.ServiceAccount,
		AIEndpoint:     cfg.AI.Endpoint,
		AIModel:        cfg.AI.Model,
		// AITokenSecretName/Key intentionally omitted: for the /runtime path the
		// ranking-model token is carried by the standing RayCluster's pods, never
		// placed in the plaintext runtimeEnvYAML of a per-job CR (docs/adr/0028).
	}
	// The actors path is the one exception (docs/adr/0029): Ray's runtime_env does
	// not reliably propagate the cluster pod's ZZ_AI_TOKEN into actor processes,
	// so for actor-mode llm-rank the launcher injects the token — which the
	// orchestrator already holds in its own env — into the RayJob runtime_env,
	// the only delivery Ray guarantees reaches actors. Scoped to that path only.
	aiToken := ""
	if llmRankActors {
		aiToken = cfg.AI.Token
	}
	if log != nil {
		log.Info("using ray launcher",
			"namespace", namespace, "cluster", cluster, "entrypoint", entrypoint,
			"llm_rank_actors", llmRankActors, "zz_base_url", opts.ZZBaseURL)
	}
	return &Launcher{
		client:        dyn,
		namespace:     namespace,
		cluster:       cluster,
		entrypoint:    entrypoint,
		llmRankActors: llmRankActors,
		aiToken:       aiToken,
		ttlSeconds:    ttl,
		opts:          opts,
		poll:          3 * time.Second,
	}, nil
}

// Launcher runs each agent job as a KubeRay RayJob on a standing RayCluster
// (docs/adr/0028). Only the thin CR envelope is unstructured; the ZZ_* injection
// contract is the same map every substrate uses, rendered into runtimeEnvYAML.
type Launcher struct {
	client        dynamic.Interface
	namespace     string
	cluster       string
	entrypoint    string
	llmRankActors bool
	aiToken       string
	ttlSeconds    int64
	opts          runtimespec.Options
	poll          time.Duration
}

var (
	_ orchestrator.Launcher      = (*Launcher)(nil)
	_ orchestrator.AsyncLauncher = (*Launcher)(nil)
)

// Dispatch creates a RayJob running the runtime entrypoint with the injection
// contract and returns a Handle naming it, without waiting (docs/adr/0024).
func (l *Launcher) Dispatch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	created, err := l.client.Resource(rayJobGVR).Namespace(l.namespace).
		Create(ctx, l.rayJob(spec, token), metav1.CreateOptions{})
	if err != nil {
		return orchestrator.Handle{Kind: "ray"}, fmt.Errorf("create rayjob: %w", err)
	}
	return orchestrator.Handle{Kind: "ray", Ref: created.GetName()}, nil
}

// Await polls the RayJob named by handle to a terminal job status. It keys off
// the Handle alone, so the orchestrator can call it on its own goroutine without
// shared launcher state (docs/adr/0024). The runtime's completion callback races
// this poll (docs/adr/0025).
func (l *Launcher) Await(ctx context.Context, handle orchestrator.Handle) error {
	ticker := time.NewTicker(l.poll)
	defer ticker.Stop()
	for {
		u, err := l.client.Resource(rayJobGVR).Namespace(l.namespace).Get(ctx, handle.Ref, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get rayjob %s: %w", handle.Ref, err)
		}
		status, _, _ := unstructured.NestedString(u.Object, "status", "jobStatus")
		switch strings.ToUpper(status) {
		case "SUCCEEDED":
			return nil
		case "FAILED", "STOPPED":
			return fmt.Errorf("rayjob %s ended in status %s", handle.Ref, status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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

// rayJob builds the RayJob CR: the shared ZZ_* injection contract rendered into
// runtimeEnvYAML, an entrypoint that runs the runtime binary, and a
// clusterSelector targeting the standing RayCluster. The ranking-model token is
// deliberately not included — the cluster carries it (docs/adr/0028).
func (l *Launcher) rayJob(spec orchestrator.JobSpec, token string) *unstructured.Unstructured {
	env := agent.Env(agent.RunParams{
		JobType:       string(spec.Type),
		BaseURL:       l.opts.ZZBaseURL,
		Token:         token,
		Provider:      spec.Provider,
		ItemID:        spec.ItemID,
		GitHubBaseURL: l.opts.GitHubBaseURL,
		AIEndpoint:    l.opts.AIEndpoint,
		AIModel:       l.opts.AIModel,
	})
	// Actor-mode llm-rank only: deliver the model token to the actors via the
	// RayJob runtime_env, the one channel Ray guarantees reaches actor processes
	// (docs/adr/0029). For /runtime jobs the token stays off the CR (docs/adr/0028).
	if l.llmRankActors && string(spec.Type) == llmRankJobType && l.aiToken != "" {
		env[agent.EnvAIToken] = l.aiToken
	}

	u := &unstructured.Unstructured{}
	u.SetAPIVersion(rayAPIVersion)
	u.SetKind(rayKind)
	u.SetGenerateName("zz-" + string(spec.Type) + "-")
	u.SetNamespace(l.namespace)
	u.SetLabels(runtimespec.Labels(spec))

	_ = unstructured.SetNestedField(u.Object, l.entrypointFor(spec), "spec", "entrypoint")
	_ = unstructured.SetNestedField(u.Object, runtimeEnvYAML(env), "spec", "runtimeEnvYAML")
	_ = unstructured.SetNestedField(u.Object, true, "spec", "shutdownAfterJobFinishes")
	_ = unstructured.SetNestedField(u.Object, l.ttlSeconds, "spec", "ttlSecondsAfterFinished")
	_ = unstructured.SetNestedStringMap(u.Object, map[string]string{clusterSelectorKey: l.cluster}, "spec", "clusterSelector")
	return u
}

// entrypointFor selects the RayJob entrypoint for a job. In actor mode, an
// llm-rank job runs the Ray-actors Python program that parallelizes scoring
// across the cluster (docs/adr/0029); every other job type — and all jobs when
// actor mode is off — runs the configured /runtime batch binary (docs/adr/0028).
func (l *Launcher) entrypointFor(spec orchestrator.JobSpec) string {
	if l.llmRankActors && string(spec.Type) == llmRankJobType {
		return actorsEntrypoint
	}
	return l.entrypoint
}

// runtimeEnvYAML renders the per-job environment as a Ray runtimeEnv document.
// JSON is a subset of YAML, so a JSON-encoded {"env_vars": {...}} is a valid
// runtimeEnvYAML string and needs no YAML dependency (docs/adr/0028).
func runtimeEnvYAML(env map[string]string) string {
	b, err := json.Marshal(map[string]any{"env_vars": env})
	if err != nil {
		return "{}"
	}
	return string(b)
}
