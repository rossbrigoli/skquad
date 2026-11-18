# Architecture Decision Records (ADRs)

This directory records skquad's key architectural decisions. Each ADR follows
the format: **Context → Decision → Consequences → Alternatives Considered**.

## Index

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-agent-runtime.md) | Agent Runtime — Thin Custom Runtime + LiteLLM + Plugin Interface | Accepted |
| [0002](0002-llm-gateway.md) | Central LLM Gateway — LiteLLM Proxy | Accepted |
| [0003](0003-scale-to-zero.md) | Scale-to-Zero — Operator-Driven | Accepted |
| [0004](0004-message-bus.md) | Async Message Bus — Postgres-Backed Queue (v1) | Accepted |
| [0005](0005-persistence.md) | Data Persistence — Postgres + pgvector | Accepted |
| [0006](0006-deployment.md) | Deployment — Vanilla K8s, Custom Operator + Helm (No OLM) | Accepted |
| [0007](0007-agent-identity.md) | Agent Identity — Owner-Created, Platform-Facilitated | Accepted |
| [0008](0008-languages.md) | Language & Runtime Choices | Accepted |
| [0009](0009-workspace-as-git.md) | Workspace-as-Git | Accepted |
| [0010](0010-ai-model-binding.md) | AI Model Binding — grants → bindings → gateway allow-list | Accepted |
| [0011](0011-layered-prompt-system.md) | Layered Prompt System (Platform / Organization / Squad / Agent) | Accepted |
| [0012](0012-builtin-platform-tools.md) | Built-in Platform Tools (exec, web_fetch, web_search) | Accepted |
| [0013](0013-tool-gateway.md) | Dedicated Tool Gateway for Governed Agent Egress | Accepted |
| [0013](0013-embedder-runtime-selection.md) | Embedder Runtime Selection (CUDA / Vulkan / CPU) — shares 0013 with Tool Gateway (pre-existing numbering collision) | Accepted |
| [0014](0014-browser-as-a-service.md) | Browser-as-a-Service (quarantined Playwright MCP) | Accepted |
| [0015](0015-three-party-grant-model.md) | Three-Party Grant Model with No-Escalation Invariant | Accepted |
| [0016](0016-agent-driven-board-transitions.md) | Agent-Driven Board Transitions with Agent-Decided Review | Accepted |
| [0017](0017-budget-admission-control.md) | Budget Enforcement Is Admission Control, Not Interruption | Accepted |

> **ADR-0010 drill (2026-09-25, S-114):** live fallback drill confirmed
> primary→fallback failover and metering (`model_used`), and confirmed the L1
> caveat — a 400-class primary error still triggers the fallback once (see
> `docs/llm-gateway.md` §Fallback drill verification).

## How to add an ADR

1. Copy the format from an existing ADR.
2. Number it sequentially (`0009-...`).
3. Set **Status** to `Proposed`, then `Accepted` / `Rejected` / `Superseded`.
4. If a decision changes, mark the old ADR `Superseded by [NNNN]` and add the
   new one.
