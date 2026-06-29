package k8slauncher

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

func TestPodSpecEncodesInjectionContract(t *testing.T) {
	l := NewPodLauncher(fake.NewSimpleClientset(), Config{
		Namespace: "zz", Image: "img:1", ZZBaseURL: "http://zz:8080", ServiceAccount: "zz-runtime",
	})
	pod := l.podSpec(orchestrator.JobSpec{
		JobID: "j1", Type: "github-enrich", Provider: "github", ActingUserID: "github:1494193",
	}, "tok-123")

	c := pod.Spec.Containers[0]
	if c.Image != "img:1" {
		t.Errorf("image = %q, want img:1", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env[agent.EnvJobType] != "github-enrich" || env[agent.EnvBaseURL] != "http://zz:8080" ||
		env[agent.EnvToken] != "tok-123" || env[agent.EnvProvider] != "github" {
		t.Errorf("injection env missing/incorrect: %v", env)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Spec.ServiceAccountName != "zz-runtime" {
		t.Errorf("serviceAccount = %q", pod.Spec.ServiceAccountName)
	}
	if got := pod.Labels["zumble-zay.dev/acting-user"]; got != "github-1494193" {
		t.Errorf("acting-user label = %q, want github-1494193 (sanitized)", got)
	}
}

func TestPodLaunchCreatesPodAndWaitsForCompletion(t *testing.T) {
	cs := fake.NewSimpleClientset()
	l := NewPodLauncher(cs, Config{Namespace: "zz", Image: "img", ZZBaseURL: "http://zz:8080"})
	l.pollInterval = 5 * time.Millisecond

	type result struct {
		h   orchestrator.Handle
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := l.Launch(context.Background(),
			orchestrator.JobSpec{JobID: "j1", Type: "github-ingest", Provider: "github", ActingUserID: "github:1"}, "tok")
		done <- result{h, err}
	}()

	name := waitForPod(t, cs, "zz")
	markPodPhase(t, cs, "zz", name, corev1.PodSucceeded)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Launch: %v", r.err)
		}
		if r.h.Kind != "k8s-pod" || r.h.Ref != name {
			t.Fatalf("handle = %+v, want kind=k8s-pod ref=%s", r.h, name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Launch did not return after the Pod succeeded")
	}
}

func TestPodLaunchReportsFailure(t *testing.T) {
	cs := fake.NewSimpleClientset()
	l := NewPodLauncher(cs, Config{Namespace: "zz", Image: "img", ZZBaseURL: "http://zz:8080"})
	l.pollInterval = 5 * time.Millisecond

	errc := make(chan error, 1)
	go func() {
		_, err := l.Launch(context.Background(),
			orchestrator.JobSpec{JobID: "j1", Type: "github-ingest", Provider: "github", ActingUserID: "github:1"}, "tok")
		errc <- err
	}()

	name := waitForPod(t, cs, "zz")
	markPodPhase(t, cs, "zz", name, corev1.PodFailed)

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected an error for a failed Pod, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Launch did not return after the Pod failed")
	}
}

func TestDetachedPodLauncherDispatchesWithoutWatching(t *testing.T) {
	cs := fake.NewSimpleClientset()
	l := NewDetachedPodLauncher(cs, Config{Namespace: "zz", Image: "img", ZZBaseURL: "http://zz:8080"})

	h, err := l.Dispatch(context.Background(),
		orchestrator.JobSpec{JobID: "j1", Type: "github-ingest", Provider: "github", ActingUserID: "github:1"}, "tok")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if h.Kind != "k8s-pod-detached" {
		t.Fatalf("handle kind = %q, want k8s-pod-detached", h.Kind)
	}
	// Dispatch created the Pod (the fake clientset does not populate GenerateName,
	// so verify via List rather than the returned name).
	if pods, _ := cs.CoreV1().Pods("zz").List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 1 {
		t.Fatalf("expected one Pod created, got %d", len(pods.Items))
	}

	// Await does not watch the Pod: the fake Pod never reaches a terminal phase,
	// yet Await returns at the deadline rather than polling forever — completion
	// would otherwise arrive via the runtime's callback (docs/adr/0025).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Await(ctx, h); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await = %v, want context.DeadlineExceeded", err)
	}
}

func waitForPod(t *testing.T, cs *fake.Clientset, ns string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		pods, _ := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
		if len(pods.Items) > 0 {
			return pods.Items[0].Name
		}
		select {
		case <-deadline:
			t.Fatal("Pod was not created")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func markPodPhase(t *testing.T, cs *fake.Clientset, ns, name string, phase corev1.PodPhase) {
	t.Helper()
	p, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	p.Status.Phase = phase
	if _, err := cs.CoreV1().Pods(ns).UpdateStatus(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
}
