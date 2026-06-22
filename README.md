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
