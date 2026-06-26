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
