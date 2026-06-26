# ADR 0013: ZZ stays on the GitHub OAuth App (GitHub Apps cannot serve a cross-org radar)

Status: Accepted — 2026-06-26

Supersedes the GitHub-App migration anticipated in
[0006](0006-credential-broker-not-data-broker.md) and noted in
[0002](0002-agents-as-ephemeral-workloads.md).

## Context

ZZ's worklist is a **cross-organization** radar. The GitHub client finds a
user's work with the search API across every org they contribute to
(`author:@me`, `assignee:@me`, `review-requested:@me`) — in practice public CNCF
repos like `kubernetes/autoscaler` and `kubernetes-sigs/cluster-api-provider-azure`.

Earlier ADRs flagged the GitHub **OAuth App**'s long-lived user token as a
security limitation and anticipated migrating to a **GitHub App** with expiring
user-to-server tokens (8-hour access token + refresh token) so each agent job
would receive a genuinely short-lived credential.

Investigation of GitHub's documentation showed that migration is **fundamentally
incompatible with a cross-org radar**. A GitHub App user-to-server token is
**installation-scoped**: per GitHub, "the app can only access resources in an
account where it is installed … it cannot access resources in an organization
that the user is a member of unless the app is also installed on that
organization." The search API enforces the same boundary (it can fail with
`no_accessible_repos`). Installing a GitHub App on `kubernetes`, `kubernetes-sigs`,
etc. requires org-owner rights the user does not have. So a GitHub App would
return items only from the user's own account and orgs they administer —
**emptying most of the worklist**.

This is a structural property of the two app models, not a configuration detail:

| | Cross-org read (public repos) | Credential lifetime |
| --- | --- | --- |
| **OAuth App** | broad — any repo the user can see, no install needed | long-lived, no expiry |
| **GitHub App** | installation-scoped — breaks the radar | 8h token + refresh |

GitHub offers **short-lived OR broad**, not both, through these models.

## Decision

**Stay on the GitHub OAuth App.** Remove the GitHub-App migration from the
roadmap and design docs entirely; it is not a deferred improvement but a
ruled-out direction for this product.

Reframe the security goal accordingly. The OAuth App token ZZ holds is
**read-only and public-scoped** (`read:user`, `user:email`); a leaked copy reads
only public data the user can already see — low severity. The short-lived
**authorization** boundary that matters for agents is already provided by ZZ's
own job tokens (`internal/mint`), independent of the provider token's lifetime.

Harden the OAuth App credential **in place** rather than by switching providers:

- Revoke the stored credential on logout (delete from the vault; optionally call
  GitHub `DELETE /applications/{client_id}/token`).
- Encrypt the vault at rest — but only meaningfully once the vault is persisted
  (it is in-memory today), so this lands with cloud persistence, not before.
- Keep source scopes minimal; treat adding `repo` (private repos) as a distinct
  decision, since it widens the blast radius of a long-lived token.

The generic **refresh-on-vend** mechanism (`auth.Handler.Credential`) stays — it
is a no-op without a refresh token and remains useful for any future provider
(e.g. Microsoft Graph) that issues expiring user tokens. It is simply never
exercised by GitHub.

## Consequences

- The cross-org radar keeps working; no risk of an empty worklist from an
  access-model change.
- ZZ accepts a long-lived GitHub credential as a deliberate tradeoff, mitigated
  by minimal read-only public scope, vault encryption (once persisted), and
  revocation on logout — not by provider migration.
- A future feature that is *naturally* installation-scoped (e.g. writing back to
  repos the user owns) could justify a GitHub App **for that surface only**,
  alongside — not replacing — the OAuth App used for the radar.
- Amends ADR 0002 and ADR 0006: their "tracked limitation / planned GitHub-App
  migration" notes are withdrawn in favor of this decision.
