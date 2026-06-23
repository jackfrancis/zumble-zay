# Copilot instructions

The canonical, tool-neutral context for this repository is **[AGENTS.md](../AGENTS.md)**.
Always read it before making changes; it covers architecture, security
invariants, conventions, build/dev commands, and the roadmap.

Deeper references:

- [docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md) — system overview + diagrams
- [docs/adr/](../docs/adr/) — architecture decision records (the "why")
- [docs/DESIGN_REVIEW.md](../docs/DESIGN_REVIEW.md) — LLM stress-test harness

## Non-negotiables (summary — see AGENTS.md for detail)

- Each `.go` file has exactly one `package` clause; remove any duplicate a
  formatter injects above the doc comment.
- Do not raise `replicas` above 1 (sessions/worklist are in-memory).
- Use the image name `localhost/zumble-zay:dev` everywhere (podman/docker + kind
  parity).
- A presented-but-invalid bearer token must return 401, never fall through to
  the cookie session.
- Source scopes are read-only; only ZZ metadata is writable. Preserve PKCE,
  state, session-ID rotation, and HttpOnly/Secure/SameSite cookies.
- After any `.go` change run `make test` and `go vet ./...`.
