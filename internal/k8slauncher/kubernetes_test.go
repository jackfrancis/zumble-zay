package k8slauncher

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

func TestJobSpecEncodesInjectionContract(t *testing.T) {
	l := New(fake.NewSimpleClientset(), Config{
		Namespace: "zz", Image: "img:1", ZZBaseURL: "http://zz:8080", ServiceAccount: "zz-runtime",
	})
	job := l.jobSpec(orchestrator.JobSpec{
		JobID: "j1", Type: "github-enrich", Provider: "github", ActingUserID: "github:1494193",
	}, "tok-123")

	c := job.Spec.Template.Spec.Containers[0]
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
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit not 0")
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Errorf("ttlSecondsAfterFinished not set")
	}
	if job.Spec.Template.Spec.ServiceAccountName != "zz-runtime" {
		t.Errorf("serviceAccount = %q", job.Spec.Template.Spec.ServiceAccountName)
	}
	if got := job.Labels["zumble-zay.dev/acting-user"]; got != "github-1494193" {
		t.Errorf("acting-user label = %q, want github-1494193 (sanitized)", got)
	}
}

func TestLaunchCreatesJobAndWaitsForCompletion(t *testing.T) {
	cs := fake.NewSimpleClientset()
	l := New(cs, Config{Namespace: "zz", Image: "img", ZZBaseURL: "http://zz:8080"})
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

	name := waitForJob(t, cs, "zz")
	markSucceeded(t, cs, "zz", name)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Launch: %v", r.err)
		}
		if r.h.Kind != "k8s-job" || r.h.Ref != name {
			t.Fatalf("handle = %+v, want kind=k8s-job ref=%s", r.h, name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Launch did not return after the Job succeeded")
	}
}

func waitForJob(t *testing.T, cs *fake.Clientset, ns string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		jobs, _ := cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
		if len(jobs.Items) > 0 {
			return jobs.Items[0].Name
		}
		select {
		case <-deadline:
			t.Fatal("Job was not created")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func markSucceeded(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	j, err := cs.BatchV1().Jobs(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	j.Status.Succeeded = 1
	if _, err := cs.BatchV1().Jobs(ns).UpdateStatus(context.Background(), j, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update job status: %v", err)
	}
}
