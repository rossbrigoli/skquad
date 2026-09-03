# skquad Agent Runtime

The thin custom agent harness (Python, LiteLLM + plugin interface). Runs inside
each agent pod. Owns the agent's lifecycle, task-scoped context, the core work
loop, and the plugin interface. Model-agnostic and extensible.

- Design: [`docs/agent-runtime.md`](../docs/agent-runtime.md)
- Decision: [`docs/adr/0001-agent-runtime.md`](../docs/adr/0001-agent-runtime.md)

## Layout
```
agent-runtime/
├── skquad_runtime/
│   ├── __init__.py
│   └── runtime.py         # bootstrap, health/readiness, core loop/handler
├── tests/
│   └── test_runtime.py
└── pyproject.toml
```

## Current implementation

- Loads bootstrap config from the operator-provided `SKQUAD_*` environment.
- Reads mounted Kubernetes Secret directories for the agent credential and LLM
  gateway virtual key without returning raw secret values.
- Exposes `/healthz`, `/readyz`, `/status`, and dependency-free Prometheus text
  at `/metrics` through FastAPI. Runtime status/metrics include counters and
  state only; task payloads, summaries, message payloads, and credentials are
  not emitted.
- Provides a small control-plane client for agent-authenticated task listing,
  resource discovery, inbox listing/acknowledgement, task-context loading,
  lease-backed claiming, fenced completion/blocking, and idle/busy/error
  heartbeats.
- Provides `poll_once`, `run_inbox_once`, and `run_task_once` primitives. The
  runtime loop drains a bounded inbox batch and then processes at most one task
  per iteration so messages and tasks do not starve each other. After an idle
  iteration it long-polls the control plane for assigned task or inbox wake-ups
  before falling back to the configured poll interval.
- Enforces configurable task execution limits: `SKQUAD_TASK_TIMEOUT_SECONDS`,
  `SKQUAD_MAX_LLM_STEPS`, and `SKQUAD_TASK_SUMMARY_MAX_CHARS`.
- Keeps the execution lease alive while a task handler runs: a background
  thread refreshes the lease every `SKQUAD_HEARTBEAT_INTERVAL_SECONDS`
  (default 40s, ≈ lease/3) so long-running tasks are not mistaken for dead
  workers by the control-plane reaper. Heartbeat failures are logged and
  retried on the next tick; the thread is joined before the terminal
  complete/block/idle call so it can never heartbeat after the execution has
  left the active state.
- Provides a default `LiteLLMTaskHandler` that reads the mounted LLM gateway
  virtual key, calls the OpenAI-compatible gateway through LiteLLM using the
  explicit `SKQUAD_DEFAULT_MODEL` model alias, discovers fresh task-scoped
  context for each task, exposes only currently granted plugin tool schemas, and
  invokes authorized loaded plugin tool calls. Calls include agent, squad, and
  task metadata so the gateway can attribute metering and failure audit events.
- Loads plugin modules with importlib from `SKQUAD_PLUGIN_MODULES`, optionally
  filters them with `SKQUAD_ENABLED_PLUGINS`, and blocks tasks predictably for
  unknown tool calls or plugin invocation failures.
- Starts the task loop in the runtime process when `SKQUAD_TASK_LOOP_ENABLED`
  is true (the operator sets it true for agent pods) while still serving
  `/healthz` and `/readyz`.
- Treats `/readyz` as an execution-readiness check: when the task loop is
  enabled, required control-plane/gateway config plus both mounted Secret values
  must be present.
- Fetches task-scoped context for the assigned task before LLM execution,
  including granted active resources and recent scoped memory rows. Context is
  not cached across tasks, so permission and memory changes take effect without
  a pod restart.
- Sends non-empty completion summaries back to the control plane for bounded
  task-result and optional per-agent memory persistence.
- Provides the `skquad-agent-runtime` console script.

Automatic registry package installation, semantic memory search, and artifact
persistence are still upcoming slices. Inbox handler failures are reported back
to the control plane so messages can retry, expire, or move to the dead-letter
state.
