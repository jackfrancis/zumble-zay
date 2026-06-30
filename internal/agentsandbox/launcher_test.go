//go:build agent_sandbox

package agentsandbox

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/runtimespec"
)

func testLauncher() *Launcher {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{sandboxGVR: "SandboxList"})
	return &Launcher{
		client:    client,
		namespace: "zz",
		opts:      runtimespec.Options{Image: "img:1", ZZBaseURL: "http://zz:8080", ServiceAccount: "zz-runtime"},
	}
}

// TestSandboxEmbedsRuntimeContract is the cross-substrate regression check: the
// Sandbox carries the identical runtime container + ZZ_* injection contract as the
// Job and Pod launchers, inside spec.podTemplate (docs/adr/0012, 0026).
func TestSandboxEmbedsRuntimeContract(t *testing.T) {
	l := testLauncher()
	u := l.sandbox(orchestrator.JobSpec{
		JobID: "j1", Type: "github-enrich", Provider: "github", ActingUserID: "github:1494193",
	}, "tok-123", time.Time{})

	if u.GetAPIVersion() != sandboxAPIVersion || u.GetKind() != sandboxKind {
		t.Fatalf("gvk = %s/%s, want %s/%s", u.GetAPIVersion(), u.GetKind(), sandboxAPIVersion, sandboxKind)
	}
	if gn := u.GetGenerateName(); gn != "zz-github-enrich-" {
		t.Errorf("generateName = %q, want zz-github-enrich-", gn)
	}
	if got := u.GetLabels()["zumble-zay.dev/acting-user"]; got != "github-1494193" {
		t.Errorf("acting-user label = %q, want github-1494193 (sanitized)", got)
	}

	containers, found, err := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("podTemplate containers missing: found=%v err=%v n=%d", found, err, len(containers))
	}
	c := containers[0].(map[string]any)
	if c["image"] != "img:1" {
		t.Errorf("image = %v, want img:1", c["image"])
	}
	env := map[string]string{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		if v, ok := m["value"].(string); ok {
			env[m["name"].(string)] = v
		}
	}
	if env[agent.EnvJobType] != "github-enrich" || env[agent.EnvBaseURL] != "http://zz:8080" ||
		env[agent.EnvToken] != "tok-123" || env[agent.EnvProvider] != "github" {
		t.Errorf("injection env missing/incorrect: %v", env)
	}
}

// TestSandboxSelfReapsWhenDeadlineSet verifies the native scheduled-deletion is
// stamped from the job deadline so a finished Sandbox cleans itself up.
func TestSandboxSelfReapsWhenDeadlineSet(t *testing.T) {
	l := testLauncher()
	deadline := time.Now().Add(2 * time.Minute)
	u := l.sandbox(orchestrator.JobSpec{Type: "github-ingest"}, "tok", deadline.Add(shutdownGrace))

	policy, _, _ := unstructured.NestedString(u.Object, "spec", "shutdownPolicy")
	if policy != "Delete" {
		t.Errorf("shutdownPolicy = %q, want Delete", policy)
	}
	if when, found, _ := unstructured.NestedString(u.Object, "spec", "shutdownTime"); !found || when == "" {
		t.Errorf("shutdownTime not set")
	}
}

// TestDispatchCreatesSandbox proves Dispatch submits a Sandbox via the dynamic
// client and returns an agent-sandbox handle. The reactor short-circuits storage
// so the test does not depend on the fake's generateName handling.
func TestDispatchCreatesSandbox(t *testing.T) {
	l := testLauncher()
	fakeDyn := l.client.(*dynamicfake.FakeDynamicClient)
	var created *unstructured.Unstructured
	fakeDyn.PrependReactor("create", "sandboxes", func(a clienttesting.Action) (bool, runtime.Object, error) {
		created = a.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		return true, created, nil
	})

	h, err := l.Dispatch(context.Background(),
		orchestrator.JobSpec{JobID: "j1", Type: "github-ingest", Provider: "github", ActingUserID: "github:1"}, "tok")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if h.Kind != "agent-sandbox" {
		t.Fatalf("handle kind = %q, want agent-sandbox", h.Kind)
	}
	if created == nil || created.GetKind() != sandboxKind {
		t.Fatalf("Create did not receive a Sandbox: %+v", created)
	}
}
