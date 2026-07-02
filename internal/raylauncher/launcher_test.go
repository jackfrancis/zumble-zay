//go:build ray

package raylauncher

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/runtimespec"
)

func testLauncher() *Launcher {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rayJobGVR: "RayJobList"})
	return &Launcher{
		client:     client,
		namespace:  "zz",
		cluster:    "zz-ray",
		entrypoint: defaultEntrypoint,
		ttlSeconds: defaultTTLSeconds,
		opts: runtimespec.Options{
			Image: "img:1", ZZBaseURL: "http://zz:8080", ServiceAccount: "zz-runtime",
			AIEndpoint: "https://api.githubcopilot.com/chat/completions", AIModel: "claude-opus-4.8",
		},
	}
}

// TestEntrypointForLLMRank verifies that llm-rank runs the Ray-actors program
// while every other job type runs /runtime, and that the CR carries the model
// token for the actors end-to-end (docs/adr/0028).
func TestEntrypointForLLMRank(t *testing.T) {
	l := testLauncher()

	// llm-rank uses the Python actors program.
	if got := l.entrypointFor(orchestrator.JobSpec{Type: "llm-rank"}); got != actorsEntrypoint {
		t.Errorf("llm-rank entrypoint = %q, want %q", got, actorsEntrypoint)
	}
	// Every other job type stays on /runtime.
	for _, jt := range []string{"github-ingest", "github-enrich", "github-converse"} {
		if got := l.entrypointFor(orchestrator.JobSpec{Type: orchestrator.JobType(jt)}); got != defaultEntrypoint {
			t.Errorf("%s entrypoint = %q, want %q (only llm-rank switches)", jt, got, defaultEntrypoint)
		}
	}

	// And the CR reflects it end-to-end, injecting the model token the actors need.
	l.aiToken = "ai-tok"
	u := l.rayJob(orchestrator.JobSpec{Type: "llm-rank", ActingUserID: "u1"}, "tok")
	ep, _, _ := unstructured.NestedString(u.Object, "spec", "entrypoint")
	if ep != actorsEntrypoint {
		t.Errorf("llm-rank rayJob entrypoint = %q, want %q", ep, actorsEntrypoint)
	}
	envYAML, _, _ := unstructured.NestedString(u.Object, "spec", "runtimeEnvYAML")
	if !strings.Contains(envYAML, "ZZ_AI_TOKEN") || !strings.Contains(envYAML, "ai-tok") {
		t.Errorf("llm-rank runtimeEnvYAML must carry ZZ_AI_TOKEN for the actors; got: %s", envYAML)
	}
}

// TestRayJobEmbedsRuntimeContract is the cross-substrate regression check: the
// RayJob carries the identical ZZ_* injection contract as the Job, Pod, and
// Sandbox launchers — here inside spec.runtimeEnvYAML rather than a pod, since a
// RayJob hosts no pod (docs/adr/0012, 0028). Uses a /runtime job type (llm-rank
// runs the actors program instead — see TestEntrypointForLLMRank).
func TestRayJobEmbedsRuntimeContract(t *testing.T) {
	l := testLauncher()
	u := l.rayJob(orchestrator.JobSpec{
		JobID: "j1", Type: "github-enrich", Provider: "github", ActingUserID: "github:1494193",
	}, "tok-123")

	if u.GetAPIVersion() != rayAPIVersion || u.GetKind() != rayKind {
		t.Fatalf("gvk = %s/%s, want %s/%s", u.GetAPIVersion(), u.GetKind(), rayAPIVersion, rayKind)
	}
	if gn := u.GetGenerateName(); gn != "zz-github-enrich-" {
		t.Errorf("generateName = %q, want zz-github-enrich-", gn)
	}
	if got := u.GetLabels()["zumble-zay.dev/acting-user"]; got != "github-1494193" {
		t.Errorf("acting-user label = %q, want github-1494193 (sanitized)", got)
	}

	entrypoint, _, _ := unstructured.NestedString(u.Object, "spec", "entrypoint")
	if entrypoint != defaultEntrypoint {
		t.Errorf("entrypoint = %q, want %q", entrypoint, defaultEntrypoint)
	}
	sel, _, _ := unstructured.NestedStringMap(u.Object, "spec", "clusterSelector")
	if sel[clusterSelectorKey] != "zz-ray" {
		t.Errorf("clusterSelector[%s] = %q, want zz-ray", clusterSelectorKey, sel[clusterSelectorKey])
	}
	shutdown, _, _ := unstructured.NestedBool(u.Object, "spec", "shutdownAfterJobFinishes")
	if !shutdown {
		t.Error("shutdownAfterJobFinishes = false, want true")
	}

	envYAML, _, _ := unstructured.NestedString(u.Object, "spec", "runtimeEnvYAML")
	for _, want := range []string{
		"ZZ_JOB_TYPE", "github-enrich",
		"ZZ_BASE_URL", "http://zz:8080",
		"ZZ_JOB_TOKEN", "tok-123",
		"ZZ_AI_ENDPOINT", "ZZ_AI_MODEL", "claude-opus-4.8",
	} {
		if !strings.Contains(envYAML, want) {
			t.Errorf("runtimeEnvYAML missing %q; got: %s", want, envYAML)
		}
	}
	// The ranking-model token must never appear in the plaintext CR (docs/adr/0028).
	if strings.Contains(envYAML, "ZZ_AI_TOKEN") {
		t.Error("runtimeEnvYAML contains ZZ_AI_TOKEN; the cluster must carry it, not the CR")
	}
}

// TestDispatchCreatesRayJob covers Dispatch: it creates exactly one RayJob and
// returns a ray-kind handle. (The fake client does not expand generateName, so
// the Ref is asserted by Await tests that seed a named object instead.)
func TestDispatchCreatesRayJob(t *testing.T) {
	l := testLauncher()
	handle, err := l.Dispatch(context.Background(), orchestrator.JobSpec{
		JobID: "j1", Type: "llm-rank", Provider: "github", ActingUserID: "u1",
	}, "tok-123")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if handle.Kind != "ray" {
		t.Fatalf("handle.Kind = %q, want ray", handle.Kind)
	}
	list, err := l.client.Resource(rayJobGVR).Namespace(l.namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("created %d rayjobs, want 1", len(list.Items))
	}
}

// TestAwaitTerminalStatus drives Await's status mapping by seeding a named RayJob
// in each terminal state.
func TestAwaitTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		status  string
		wantErr bool
	}{
		{"SUCCEEDED", false},
		{"FAILED", true},
		{"STOPPED", true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			l := testLauncher()
			l.poll = time.Millisecond
			job := &unstructured.Unstructured{}
			job.SetAPIVersion(rayAPIVersion)
			job.SetKind(rayKind)
			job.SetNamespace(l.namespace)
			job.SetName("zz-llm-rank-seed")
			_ = unstructured.SetNestedField(job.Object, tc.status, "status", "jobStatus")
			if _, err := l.client.Resource(rayJobGVR).Namespace(l.namespace).Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed create: %v", err)
			}
			err := l.Await(context.Background(), orchestrator.Handle{Kind: "ray", Ref: "zz-llm-rank-seed"})
			if tc.wantErr && err == nil {
				t.Errorf("Await(%s) = nil, want error", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Await(%s) = %v, want nil", tc.status, err)
			}
		})
	}
}
