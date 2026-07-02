# Architecture Decision Records

Short records of significant decisions: the context, the decision, and its
consequences. Newest decisions supersede older ones explicitly.

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-zz-as-authorization-server.md) | ZZ is the authorization server | Accepted |
| [0002](0002-agents-as-ephemeral-workloads.md) | Agents are ephemeral spawned workloads | Accepted |
| [0003](0003-kubernetes-substrate.md) | Kubernetes is the runtime substrate | Accepted |
| [0004](0004-render-reads-persisted-zz.md) | Landing page reads persisted ZZ data | Accepted |
| [0005](0005-in-memory-first.md) | In-memory stores first, behind interfaces | Accepted |
| [0006](0006-credential-broker-not-data-broker.md) | ZZ is a credential broker, not a data broker | Accepted |
| [0007](0007-orchestrator-colocated-until-spawn.md) | Orchestrator co-located until it spawns real workloads | Accepted |
| [0008](0008-worklist-ranking-signals-vs-scores.md) | Worklist ranking separates observed signals from computed scores | Accepted |
| [0009](0009-agent-runtime-contract-boundary.md) | The agent runtime contract is the substrate-neutral boundary | Accepted |
| [0010](0010-agent-runtimes-capability-scoped.md) | Agent runtimes are capability-scoped, not per-call | Accepted |
| [0011](0011-llm-produced-axes-zz-blends.md) | The four ranking axes are LLM-produced; ZZ blends and ratifies | Accepted |
| [0012](0012-kubernetes-native-substrates-swappable.md) | Kubernetes-native agent substrates are swappable behind a runtime artifact | Accepted |
| [0013](0013-stay-on-github-oauth-app.md) | ZZ stays on the GitHub OAuth App (GitHub Apps can't serve a cross-org radar) | Accepted |
| [0014](0014-axis-ranker-openai-compatible-client.md) | The AxisRanker uses an OpenAI-compatible chat-completions client (Copilot default) | Accepted |
| [0015](0015-llm-axes-authoritative.md) | LLM axis proposals are authoritative (retire the ratify clamp and confidence floor) | Accepted |
| [0016](0016-server-rendered-landing-page.md) | A lean server-rendered landing page styled with vendored Primer | Accepted |
| [0017](0017-hide-user-metadata-agents-unhide.md) | Hide is user-set metadata; agents auto-unhide on change | Accepted |
| [0018](0018-per-item-assistive-conversation.md) | Per-item assistive conversation (read-only) | Accepted (synchronous turn superseded by 0019) |
| [0019](0019-conversation-as-ephemeral-agent.md) | The assistive conversation is an ephemeral agent (async, live GitHub context) | Accepted |
| [0020](0020-conversation-github-tool-use.md) | Read-only GitHub tool-use in the conversation (file@ref, PR/issue, search) | Accepted |
| [0021](0021-render-assistant-markdown.md) | Render assistant Markdown to sanitized HTML (goldmark + bluemonday) | Accepted |
| [0022](0022-discussion-research-axis-reweighting.md) | Discussion-derived "research" re-weighting of the ranking axes (per-axis multipliers) | Accepted |
| [0023](0023-orchestrator-own-runtime-and-identity.md) | Extract the orchestrator into its own runtime and identity (asymmetric mint, control API) | Accepted |
| [0024](0024-agent-runtime-portability.md) | Agent-runtime portability: launcher registry, async dispatch, and token exchange | Accepted |
| [0025](0025-callback-driven-completion.md) | Callback-driven job completion (the runtime reports; the orchestrator races it against the watch) | Accepted |
| [0026](0026-agent-sandbox-substrate.md) | agent-sandbox as an optional, build-tagged substrate (dynamic-client Sandbox CR, detached completion) | Accepted |
| [0027](0027-opensandbox-substrate.md) | OpenSandbox as a remote-control-plane substrate (hand-rolled HTTP, detached completion) | Accepted |
| [0029](0029-kagent-durable-runtime-substrate.md) | kagent as a durable BYO-A2A runtime substrate (per-job params in message metadata, detached callback completion) | Accepted |
| [0030](0030-job-token-pull-path.md) | Job-token pull-path: single-use ticket redemption keeps the live token out of a durable runtime's persisted task history | Accepted |
