# zumble-zay

A lean, secure web backend in Go. It authenticates users via trusted OAuth2
identity providers (Google, GitHub, Microsoft) and provides the authenticated
foundation for retrieving, organizing, and persisting user-contextualized
GitHub data (issues, PRs, comments, reviews) enriched with Zumble-Zay metadata.

This is the first component of a larger project: the authentication and HTTP
foundation. Domain features (GitHub data retrieval, persistence, and
Zumble-Zay metadata) build on top of it.

> **Project context & design:** [AGENTS.md](AGENTS.md) (canonical, tool-neutral
> context), [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), the decision records in
> [docs/adr/](docs/adr/), and a model-agnostic LLM
> [design-review harness](docs/DESIGN_REVIEW.md).

## Features

- **OAuth2 login** with Google, GitHub, and Microsoft (Azure AD).
- **PKCE + state** on every login to defend against code interception and CSRF.
- **Server-side sessions** with HMAC-signed, `HttpOnly`, `SameSite=Lax` cookies.
  Session fixation is prevented by rotating the session ID on login.
- **Auth-gated routing** via a unified `RequireAuth`/`RequireScope` middleware
  that resolves a principal from a session cookie or a workload bearer token.
- **Security hardening**: strict security headers, allow-list CORS, request body
  size limits, request timeouts, panic recovery, and graceful shutdown.
- **Zero third-party deps** beyond `golang.org/x/oauth2`.

## Project layout

```
cmd/server/          # main entrypoint
internal/config/     # environment configuration
internal/session/    # signed-cookie session manager
internal/auth/       # OAuth provider wiring + login/callback handlers
internal/authn/      # unified RequireAuth/RequireScope (cookie or bearer)
internal/principal/  # Principal abstraction (user | workload)
internal/worklist/   # WorkItem + ZZ metadata, store + sort + ingestion seam
internal/api/        # JSON HTTP handlers
internal/server/     # router + middleware
```

## Getting started

The primary dev loop runs zumble-zay the **same way it runs in production —
inside Kubernetes** — using a local [kind](https://kind.sigs.k8s.io) cluster, so
you catch container, manifest, and config issues immediately. One command stands
it up:

```sh
make dev-up        # build image → create kind cluster → load → apply → ready
make dev-forward   # port-forward the service to localhost:8080  (blocks)
```

Then, in another shell:

```sh
curl localhost:8080/healthz          # {"status":"ok"}
curl localhost:8080/auth/providers   # enabled OAuth providers
```

Full details — prerequisites, iterating, enabling OAuth, teardown — are in
[Develop on Kubernetes (kind)](#develop-on-kubernetes-kind). Prefer a bare
process for a quick check? See [Run as a plain Go process](#run-as-a-plain-go-process-alternative).

## Develop on Kubernetes (kind)

A hardened container ([Dockerfile](Dockerfile), distroless/non-root/static) and
kustomize manifests under [deploy/k8s](deploy/k8s) deploy the backend. The
Dockerfile uses no BuildKit-only features, so it builds identically with Docker
or Podman, and the `make` targets auto-detect the engine (Podman preferred on
macOS, Docker otherwise — override with `CONTAINER_ENGINE=docker`).

### Prerequisites

- `kubectl`, `kind`, and a container engine (`podman` or `docker`).
  On macOS: `brew install kind kubectl podman`.
- With Podman, start the VM once: `podman machine start`.

### Stand it up

```sh
make dev-up        # build image from source → create kind cluster →
                   # load image → apply manifests → wait until ready
make dev-forward   # port-forward the service to localhost:8080  (blocks)
```

Then, in another shell:

```sh
curl localhost:8080/healthz          # {"status":"ok"}
curl localhost:8080/auth/providers   # enabled OAuth providers
```

`make dev-up` is idempotent — it auto-detects podman/docker, sets the kind
podman provider when needed, and creates a random `SESSION_SECRET` only if the
Secret does not already exist (so re-runs never rotate your session secret). It
also seeds `AI_TOKEN` from your environment when exported, so the in-cluster LLM
gateway comes up — see [Enable model ranking](#enable-model-ranking-optional).

### Iterate

After editing source, re-run a single command to rebuild and redeploy:

```sh
make dev-up        # rebuilds the image, reloads it, re-applies, rolls out
make dev-logs      # tail application logs
```

### Enable OAuth login (optional)

The app starts without provider credentials (health and auth-listing endpoints
work; login does not). To enable a provider, add its credentials to the Secret
and restart the rollout:

```sh
kubectl -n zumble-zay create secret generic zumble-zay-secrets \
  --from-literal=SESSION_SECRET="$(openssl rand -base64 48)" \
  --from-literal=GITHUB_CLIENT_ID=... --from-literal=GITHUB_CLIENT_SECRET=... \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n zumble-zay rollout restart deploy/zumble-zay
```

Set each provider's OAuth redirect URI to `${BASE_URL}/auth/<provider>/callback`
(for the dev overlay, `http://localhost:8080/auth/github/callback`).

### Enable model ranking (optional)

The dev overlay runs an in-cluster [agentgateway](deploy/k8s/overlays/dev/agentgateway.yaml)
as the agents' LLM egress proxy: every runtime's chat-completions call goes
through it, and the provider key lives only in the gateway (sourced from
`zumble-zay-secrets/AI_TOKEN`). Provide that key to enable LLM ranking:

```sh
export AI_TOKEN=<provider key>   # e.g. a GitHub Copilot token
make dev-up                      # seeds AI_TOKEN into the Secret, then rolls out
```

Without `AI_TOKEN` the gateway pod stays in `CreateContainerConfigError` and
runtimes fall back to the deterministic stub ranker — health, auth, and worklist
rendering still work. To add the key to an already-running cluster without a full
`make dev-up`:

```sh
kubectl -n zumble-zay patch secret zumble-zay-secrets --type merge \
  -p "{\"stringData\":{\"AI_TOKEN\":\"$AI_TOKEN\"}}"
kubectl -n zumble-zay rollout restart deploy/zumble-zay-agentgateway
```

### Tear down

```sh
make dev-down      # delete the kind cluster
```

### Run as a plain Go process (alternative)

For a quick, dependency-light check that skips containers and Kubernetes:

1. Copy the environment template and fill in your values: `cp .env.example .env`.
2. Generate a session secret (`openssl rand -base64 48`) into `SESSION_SECRET`.
3. Register OAuth apps with redirect URI `${BASE_URL}/auth/<provider>/callback`
   (e.g. `http://localhost:8080/auth/github/callback`); only configured
   providers are enabled.
4. Load the environment and run: `set -a; source .env; set +a && make run`.

### Notes

- State (sessions, worklist) is in-memory today, so the Deployment runs
  `replicas: 1`. Horizontal scaling waits on a shared session store and the
  cloud worklist store.
- The dev overlay drops the public Ingress and serves over plain HTTP via
  port-forward (`COOKIE_SECURE=false`). The `base` is production-shaped (TLS
  Ingress, `COOKIE_SECURE=true`); override `BASE_URL`, the Ingress host, and the
  TLS secret per environment.

## API

| Method | Path                       | Auth | Description                  |
| ------ | -------------------------- | ---- | ---------------------------- |
| GET    | `/healthz`                 | no   | Health check                 |
| GET    | `/auth/providers`          | no   | List enabled providers       |
| GET    | `/auth/{provider}/login`   | no   | Begin OAuth login            |
| GET    | `/auth/{provider}/callback`| no   | OAuth redirect target        |
| POST   | `/auth/logout`             | no   | Destroy the current session  |
| GET    | `/api/me`                  | yes  | Current authenticated user   |
| GET    | `/api/worklist`            | yes  | Ordered set of work items    |

Domain endpoints (GitHub data retrieval and Zumble-Zay metadata) are layered
on this foundation as they are built.

## Development

```sh
make test   # run tests
make vet    # static analysis
make build  # compile to bin/server
```

## Roadmap

Upcoming work builds on this foundation:

1. Retrieve user-contextualized GitHub data (issues, PRs, comments, reviews).
2. Persist organized GitHub data via a cloud storage API for fast retrieval.
3. Attach Zumble-Zay metadata (priority, relevance, impact) per data point.

These require retaining the OAuth access token in the session, a GitHub client
layer, and a persistence interface — none of which exist yet.
