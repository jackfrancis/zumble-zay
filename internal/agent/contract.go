package agent

import (
	"fmt"
	"strconv"
)

// Injection contract (docs/adr/0012): the environment variables a runtime reads
// and every launcher fills. Env (encode) and ParamsFromEnv (decode) are inverse
// and live together so the two halves cannot drift. The acting user and job id
// are deliberately absent — they ride inside the signed job token.
const (
	EnvJobType       = "ZZ_JOB_TYPE"
	EnvBaseURL       = "ZZ_BASE_URL"
	EnvToken         = "ZZ_JOB_TOKEN"
	EnvProvider      = "ZZ_PROVIDER"
	EnvGitHubBaseURL = "ZZ_GITHUB_BASE_URL"
	EnvEnrichLimit   = "ZZ_ENRICH_LIMIT"
)

// Env encodes the serializable parameters of a runtime invocation into the
// injection-contract environment. A launcher sets these on the workload it
// creates; the runtime reconstructs them with ParamsFromEnv. Non-serializable
// fields (the HTTP client and ranker) are constructed by the runtime, not
// injected.
func Env(p RunParams) map[string]string {
	env := map[string]string{
		EnvJobType: p.JobType,
		EnvBaseURL: p.BaseURL,
		EnvToken:   p.Token,
	}
	if p.Provider != "" {
		env[EnvProvider] = p.Provider
	}
	if p.GitHubBaseURL != "" {
		env[EnvGitHubBaseURL] = p.GitHubBaseURL
	}
	if p.EnrichLimit > 0 {
		env[EnvEnrichLimit] = strconv.Itoa(p.EnrichLimit)
	}
	return env
}

// ParamsFromEnv reconstructs RunParams from the injection contract; getenv is
// usually os.Getenv. The HTTP client and ranker are left nil for the runtime to
// default. It returns an error if a required variable is missing or malformed.
func ParamsFromEnv(getenv func(string) string) (RunParams, error) {
	p := RunParams{
		JobType:       getenv(EnvJobType),
		BaseURL:       getenv(EnvBaseURL),
		Token:         getenv(EnvToken),
		Provider:      getenv(EnvProvider),
		GitHubBaseURL: getenv(EnvGitHubBaseURL),
	}
	if v := getenv(EnvEnrichLimit); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return RunParams{}, fmt.Errorf("agent: invalid %s %q: %w", EnvEnrichLimit, v, err)
		}
		p.EnrichLimit = n
	}
	if p.BaseURL == "" || p.Token == "" || p.JobType == "" {
		return RunParams{}, fmt.Errorf("agent: missing required env (%s, %s, %s)", EnvBaseURL, EnvToken, EnvJobType)
	}
	return p, nil
}
