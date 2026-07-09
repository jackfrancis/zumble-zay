# ADR 0033: Transport isolation for the control plane (NetworkPolicy)

Status: Accepted — 2026-07-09. Implemented as `deploy/k8s/base/networkpolicy.yaml`
(shipped in the base kustomization, so it applies in every environment) plus a
`make dev-up` step that installs the kube-network-policies controller to enforce
it on kind. No application code changed.

Builds on [0031](0031-control-plane-caller-identity.md) (per-service caller
identity on the control API via TokenReview) and
[0032](0032-agent-plane-token-audience.md) (audience-binding the job token to the
agent plane). Those bind *who* may authenticate; this binds *who may open a
socket at all*. It is the L3/L4 layer of the "transport isolation (mTLS /
NetworkPolicy)" follow-up; mTLS remains separate (see Consequences).

## Context

The web→orchestrator control API (`internal/controlplane`, the orchestrator's
`:8090`) triggers the privileged operations of the system: it spawns agent
runtimes and mints job tokens. It is already hardened at the application layer —
it is `ClusterIP`-only, never exposed through the Ingress, fail-closed, and since
ADR 0031 authenticates every caller per-service via TokenReview (chained over a
shared bearer). ADR 0032 gave the runtime→web plane its audience binding.

What none of that provides is a network-layer control. Every pod in the cluster —
including an ephemeral, attacker-influenceable *runtime* pod — can still open a
TCP connection to the orchestrator's control port and attempt to authenticate.
The auth controls are the gate, but a compromised or hostile pod should not even
be able to reach the socket to knock on it. That is a textbook case for a
default-deny `NetworkPolicy`: reduce the reachable surface to the one legitimate
client (the web tier) before authentication is ever considered.

The reason this layer had been deferred is a dev-substrate limitation: kind's
default CNI (**kindnet**) does not enforce `NetworkPolicy`. A policy applied on
kindnet is silently inert, so it could neither be exercised nor regression-tested
in the `make dev-up` loop — shipping an unenforced, untested security manifest is
how such controls bitrot. Three ways to get enforcement in dev were weighed:

- **(A) Swap the dev CNI** to a policy-enforcing one (Calico/Cilium). Enforces
  natively, but replaces the entire dev network substrate, diverges from the kind
  default every other contributor runs, and is a heavy dependency for one policy.
- **(B) Ship the policy prod-facing / apply-only**, never enforced in dev. Zero
  dev cost, but the manifest is never exercised in our own loop — precisely the
  bitrot failure mode above.
- **(C) Install the kube-network-policies controller** (kubernetes-sigs) *alongside*
  kindnet. It enforces standard `NetworkPolicy` via nftables/NFQUEUE without
  replacing the CNI, is purpose-built for exactly this kind gap, and permits
  node-sourced kubelet health probes out of the box.

## Decision

Add the network-layer control as a base manifest and enforce it in dev with a
controller install.

- **Default-deny Ingress on the orchestrator, one allowed source.**
  `deploy/k8s/base/networkpolicy.yaml` selects the orchestrator pod
  (`app.kubernetes.io/component: orchestrator`), which flips it to default-deny
  for Ingress, and permits only the web tier
  (`app.kubernetes.io/component: backend`) to reach `TCP 8090`. A runtime pod in
  any namespace is denied at the network layer. The policy governs Ingress only —
  egress is unrestricted, and node-sourced kubelet probes are permitted by the
  enforcing controller.
- **Ship it in the base kustomization**, so it applies in every environment. It is
  declarative and CNI-agnostic: where the CNI enforces `NetworkPolicy` (a
  production cluster), it is live with no extra moving parts; where it does not
  (bare kindnet), it is inert but harmless.
- **`make dev-up` installs kube-network-policies** (pinned
  `KUBE_NETWORK_POLICIES_VERSION`, default `v1.1.0`) so the base policy is actually
  enforced on kind. The step is best-effort and skippable (empty version):
  because the policy is defense-in-depth and fail-open, a transient install
  failure degrades to the pre-0033 kindnet behavior rather than blocking the dev
  loop.

## Consequences

- **The control API's reachable surface shrinks to one client.** A compromised
  pod — including a runtime, the most exposed identity in the system — can no
  longer open a socket to the orchestrator's control port; only the web tier can.
  This sits *underneath* the ADR 0031/0032 auth controls, not in place of them:
  authentication remains the primary gate.
- **Defense-in-depth, never the sole control.** On a CNI that ignores
  `NetworkPolicy` (bare kindnet, or if the dev controller install fails) the
  manifest is fail-open. It is therefore always layered over the auth controls and
  is never relied on alone — the same posture the file header states.
- **Dev now live-validates the policy.** Enforcing it on kind is the specific thing
  that unblocks this follow-up: the control can be exercised end-to-end in the
  standard loop rather than shipped on faith.
- **Scope is Ingress L3/L4 for the control plane only.** This does not encrypt
  traffic in transit and does not stop replay of a *stolen* token from within an
  allowed source. In-transit confidentiality/integrity and within-plane replay are
  the province of mTLS plus the short token TTL, which remain the open half of the
  transport follow-up. The runtime→web `/agent/*` plane is out of scope here (its
  within-plane replay is likewise transport + TTL).
- **The selectors are label-coupled.** The policy's `from`/`podSelector` must track
  the deployment labels (`backend` for the web tier, `orchestrator` for the
  control plane); a label rename that misses this manifest would silently
  over-deny (break the control plane) or under-select. The current labels were
  verified against the deployments.
- **One new dev dependency.** The kube-network-policies DaemonSet in `kube-system`.
  It is pinned and overridable, and installing it is the only change to the bare
  `make dev-up` baseline.

## Alternatives considered

- **Calico/Cilium CNI swap (A)** — rejected: replaces the whole dev network
  substrate and diverges from the kind default for a single policy.
- **Apply-only / prod-facing, unenforced in dev (B)** — rejected: never exercised
  in our own loop; the manifest would bitrot.
- **mTLS first** — deferred, not rejected. A `NetworkPolicy` is the cheaper,
  declarative first layer and needs no certificate-issuance story; mTLS (a mesh or
  SPIRE-style identity) is a larger design that also addresses within-plane replay
  and is tracked as the remaining transport work.
