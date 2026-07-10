package agent

import (
	"testing"
	"time"
)

func TestEnvParamsRoundTrip(t *testing.T) {
	dispatched := time.UnixMilli(1_700_000_000_123)
	in := RunParams{
		JobType:       JobEnrich,
		BaseURL:       "http://zz:8080",
		Token:         "tok",
		Provider:      "github",
		GitHubBaseURL: "http://gh.example",
		EnrichLimit:   25,
		DispatchedAt:  dispatched,
	}
	env := Env(in)
	out, err := ParamsFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("ParamsFromEnv: %v", err)
	}
	if out.JobType != in.JobType || out.BaseURL != in.BaseURL || out.Token != in.Token ||
		out.Provider != in.Provider || out.GitHubBaseURL != in.GitHubBaseURL || out.EnrichLimit != in.EnrichLimit {
		t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
	if !out.DispatchedAt.Equal(dispatched) {
		t.Fatalf("DispatchedAt round trip: got %v, want %v", out.DispatchedAt, dispatched)
	}
}

func TestParamsFromEnvRequiresCoreFields(t *testing.T) {
	if _, err := ParamsFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("expected an error when required env is missing")
	}
}

func TestParamsFromEnvRejectsBadEnrichLimit(t *testing.T) {
	get := func(k string) string {
		switch k {
		case EnvJobType:
			return JobIngest
		case EnvBaseURL:
			return "http://zz"
		case EnvToken:
			return "tok"
		case EnvEnrichLimit:
			return "not-a-number"
		}
		return ""
	}
	if _, err := ParamsFromEnv(get); err == nil {
		t.Fatal("expected an error for a malformed enrich limit")
	}
}

func TestEnvOmitsEmptyOptionalFields(t *testing.T) {
	env := Env(RunParams{JobType: JobIngest, BaseURL: "http://zz", Token: "tok"})
	for _, k := range []string{EnvProvider, EnvGitHubBaseURL, EnvEnrichLimit} {
		if _, ok := env[k]; ok {
			t.Errorf("expected %s to be omitted when empty", k)
		}
	}
}
