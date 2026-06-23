# zumble-zay

A lean, secure web backend in Go. It authenticates users via trusted OAuth2
identity providers (Google, GitHub, Microsoft) and provides the authenticated
foundation for retrieving, organizing, and persisting user-contextualized
GitHub data (issues, PRs, comments, reviews) enriched with Zumble-Zay metadata.

This is the first component of a larger project: the authentication and HTTP
foundation. Domain features (GitHub data retrieval, persistence, and
Zumble-Zay metadata) build on top of it.

## Features

- **OAuth2 login** with Google, GitHub, and Microsoft (Azure AD).
- **PKCE + state** on every login to defend against code interception and CSRF.
- **Server-side sessions** with HMAC-signed, `HttpOnly`, `SameSite=Lax` cookies.
  Session fixation is prevented by rotating the session ID on login.
- **Auth-gated routing** via `requireAuth` middleware for protected endpoints.
- **Security hardening**: strict security headers, allow-list CORS, request body
  size limits, request timeouts, panic recovery, and graceful shutdown.
- **Zero third-party deps** beyond `golang.org/x/oauth2`.

## Project layout

```
cmd/server/          # main entrypoint
internal/config/     # environment configuration
internal/session/    # signed-cookie session manager
internal/auth/       # OAuth provider wiring + login/callback handlers
internal/server/     # router + middleware
```

## Getting started

There are two ways to run zumble-zay locally:

- **On Kubernetes (recommended)** — mirrors how it runs in production; one
  command: `make dev-up`. See [Develop on Kubernetes (kind)](#develop-on-kubernetes-kind).
- **As a plain Go process** — the lightweight loop described below.

### Run as a Go process

1. Copy the environment template and fill in your values:

   ```sh
   cp .env.example .env
   ```

2. Generate a session secret:

   ```sh
   openssl rand -base64 48
   ```

   Put the result in `SESSION_SECRET`.

3. Register OAuth applications and set the redirect URI for each provider to
   `${BASE_URL}/auth/<provider>/callback`, e.g.
   `http://localhost:8080/auth/github/callback`. Only providers you configure
   are enabled.

4. Load the environment and run:

   ```sh
   set -a; source .env; set +a
   make run
   ```

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

## Develop on Kubernetes (kind)

The primary dev loop runs the app the same way it runs in production — in
Kubernetes — using a local [kind](https://kind.sigs.k8s.io) cluster. A hardened
container ([Dockerfile](Dockerfile), distroless/non-root/static) and kustomize
manifests under [deploy/k8s](deploy/k8s) deploy the backend. The Dockerfile uses
no BuildKit-only features, so it builds identically with Docker or Podman, and
the `make` targets auto-detect the engine (Podman preferred on macOS, Docker
otherwise — override with `CONTAINER_ENGINE=docker`).

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
Secret does not already exist (so re-runs never rotate your session secret).

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

### Tear down

```sh
make dev-down      # delete the kind cluster
```

### Notes

- State (sessions, worklist) is in-memory today, so the Deployment runs
  `replicas: 1`. Horizontal scaling waits on a shared session store and the
  cloud worklist store.
- The dev overlay drops the public Ingress and serves over plain HTTP via
  port-forward (`COOKIE_SECURE=false`). The `base` is production-shaped (TLS
  Ingress, `COOKIE_SECURE=true`); override `BASE_URL`, the Ingress host, and the
  TLS secret per environment.

## Roadmap

Upcoming work builds on this foundation:

1. Retrieve user-contextualized GitHub data (issues, PRs, comments, reviews).
2. Persist organized GitHub data via a cloud storage API for fast retrieval.
3. Attach Zumble-Zay metadata (priority, relevance, impact) per data point.

These require retaining the OAuth access token in the session, a GitHub client
layer, and a persistence interface — none of which exist yet.
