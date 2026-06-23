# Design review / stress-test harness

A portable, model-agnostic way for a teammate to have **any** LLM
(Claude, GPT, Gemini, Llama, etc.) critique Zumble-Zay's design and
implementation, so the foundations get refined by diverse perspectives.

The goal is *adversarial review*, not agreement. Use a different model than the
one that wrote the code.

## How to run it

1. Give the model this repository as context. Either:
   - paste [../AGENTS.md](../AGENTS.md), [ARCHITECTURE.md](ARCHITECTURE.md), and
     [adr/](adr/) into the chat; or
   - point a repo-aware tool (Copilot, Cursor, Aider, Claude Code, etc.) at the
     repo root — it will pick up `AGENTS.md` automatically.
2. Paste the **Reviewer prompt** below.
3. Capture the findings (see "Recording results").

## Reviewer prompt (copy/paste)

> You are a skeptical staff-level engineer doing an adversarial design and
> security review of the Zumble-Zay (ZZ) backend. Do not be agreeable; your job
> is to find what is wrong, risky, underspecified, or over-engineered.
>
> Context to read first: `AGENTS.md`, `docs/ARCHITECTURE.md`, and every file in
> `docs/adr/`. Then inspect `internal/` and `deploy/k8s/`.
>
> Evaluate each area and give concrete, prioritized findings:
>
> 1. **Authentication & sessions** — PKCE/state usage, session-ID rotation,
>    cookie flags, the "invalid bearer never falls through to cookie" rule.
>    Identify bypasses or CSRF/fixation/replay gaps.
> 2. **Agent authorization model (ADR 0001/0002)** — the RFC 8693 token-exchange
>    shape, scope intersection at spawn, blast radius, revocation, token TTL and
>    leakage. Where does this break under a compromised orchestrator or runtime?
> 3. **Token vault & delegated provider tokens** — encryption-at-rest plan, key
>    management, least privilege, what happens on provider-side revocation.
> 4. **Worklist & data model** — ordering correctness, the empty→ingest trigger
>    as a side-effecting GET, idempotency, pagination, multi-tenant isolation via
>    `ActingUserID`.
> 5. **Persistence & scaling (ADR 0005)** — the in-memory→cloud path, the
>    `replicas: 1` constraint, session sharing, consistency.
> 6. **Kubernetes & supply chain (ADR 0003)** — securityContext, secrets
>    handling, image provenance, network policy gaps, multi-tenant concerns.
> 7. **API design** — REST shape, error contracts, versioning, abuse/rate limits.
>
> For each finding provide: severity (Critical/High/Medium/Low), the specific
> file or decision, the concrete risk, and a recommended change. End with the
> top 5 things to fix before building the next increment, and any decision in
> `docs/adr/` you would reverse or amend (with reasoning).

## Optional focused prompts

- **Threat model:** "Produce a STRIDE threat model for the agent→ZZ→provider
  trust boundaries described in ADR 0001 and 0002."
- **Red-team the auth:** "Enumerate concrete attack paths against the session
  and bearer-token handling in `internal/authn` and `internal/session`."
- **Scale review:** "Given ADR 0005, design the minimal change set to safely run
  `replicas > 1`, and list what would break if we did it today."

## Recording results

Keep reviews durable and comparable across models:

- Save each run as `docs/reviews/<date>-<model>.md` with the model name and the
  raw findings.
- Triage findings into issues; when a finding changes a decision, add or amend
  an ADR in `docs/adr/` so the rationale trail stays current.

## Guardrails for reviewers acting on the repo

If the reviewing tool can edit code, hold it to the invariants in `AGENTS.md`
(no `replicas` increase, image name `localhost/zumble-zay:dev`, one `package`
clause per file, read-only source scopes). Prefer proposals and ADR amendments
over silent rewrites.
