# skquad Helm Chart

Packages and deploys the skquad control plane + operator on vanilla Kubernetes
(no OLM). Squad/agent resources are created via the API (not by hand).

- Design: [`docs/deployment-operator.md`](../../docs/deployment-operator.md) §6
- Operations runbook:
  [`docs/operator-runbook.md`](../../docs/operator-runbook.md)

## Install
```
helm install skquad charts/skquad -n skquad-system --create-namespace
```

## Images

The chart defaults to GHCR images published by the repository workflows:

- `ghcr.io/rossbrigoli/skquad-api-server`
- `ghcr.io/rossbrigoli/skquad-operator`
- `ghcr.io/rossbrigoli/skquad-llm-gateway`
- `ghcr.io/rossbrigoli/skquad-agent-runtime`
- `ghcr.io/rossbrigoli/skquad-web`

Override `image.*.repository` and `image.*.tag` for local builds, private
registries, or a specific `sha-<short-sha>` image.

CRDs for `squads.skquad.io` and `agents.skquad.io` are installed from
`charts/skquad/crds` before the rest of the chart.

The current chart installs:

- CRDs for `Squad` and `Agent`
- Operator namespace, ServiceAccount, ClusterRole, ClusterRoleBinding, and
  Deployment. The ClusterRole includes CR finalizer update permissions and
  delete permissions for managed Namespaces, Deployments, ServiceAccounts,
  ResourceQuotas, NetworkPolicies, and per-squad RBAC so operator finalizers can
  clean up cross-namespace resources.
- API server ServiceAccount, namespaced CR-writer RBAC, Deployment, and Service.
  Generated agent credential and virtual-key Secret write/delete access is
  granted by operator-created RoleBindings in each reconciled squad namespace,
  not by a chart-level cluster-wide Secret writer role.
- LLM gateway master-key Secret, ConfigMap, Deployment, and Service
- Web Deployment and Service
- Postgres Secret, Service, and StatefulSet for local/dev installs
- Optional Kubernetes Ingress or Traefik IngressRoute routing `/api` to the API
  server and `/` to the web app
- Runtime URL defaults so generated Agent CRs point pods back at the chart's API
  server and LLM gateway services
- Agent runtime defaults for idle timeout, task/inbox fallback poll intervals,
  work-wait timeout, and inbox batch size.

For production installs, set `postgres.existingSecret` and
`apiServer.databaseUrlSecret.name` so database credentials and the API database
URL come from a pre-created Secret instead of chart values.

Override `apiServer.runtimeControlPlaneUrl` or
`apiServer.runtimeLlmGatewayUrl` when agent pods must call non-chart service
endpoints.

The chart starts the gateway as a LiteLLM proxy using
`llmGateway.config`. By default that config enables virtual-key auth through
`LITELLM_MASTER_KEY`, uses the chart/Postgres database with a separate
`litellm` schema, and has an empty `model_list` so the default render is valid
without real provider credentials.
Set `llmGateway.masterKeySecret.name` to use a pre-created Secret instead of
the development `llmGateway.masterKey` value. Set `llmGateway.databaseUrl` or
`llmGateway.databaseUrlSecret` to use an external LiteLLM database. Put provider
definitions in `llmGateway.config` and inject provider API keys with
`llmGateway.extraEnv` or `llmGateway.extraEnvFrom`; do not place real API keys
in the config map.
The gateway image includes LiteLLM proxy dependencies, generated Prisma client
artifacts, and a fetched Prisma query-engine binary for persistent virtual-key
storage.
The chart also wires the same master-key Secret into the API server and gateway
as `SKQUAD_GATEWAY_CALLBACK_TOKEN`. The gateway's Skquad LiteLLM callback uses
that token to post successful usage and failure audit events back to the API
server's internal `/api/v1/gateway/metering` endpoint.

`llmGateway.probes.startup` gives LiteLLM time to prepare database-backed proxy
state before liveness checks can restart the container. Tune it upward if a
large migration or cold image pull makes startup exceed the default window.
`llmGateway.migrationDir` sets `LITELLM_MIGRATION_DIR` to a writable path so
LiteLLM can create baseline migration state if the target schema is not empty.

Runtime fallback polling defaults are configured through `agent.idleTimeout`,
`agent.taskPollIntervalSeconds`, `agent.inboxPollIntervalSeconds`, and
`agent.inboxBatchSize`. Idle runtimes long-poll the control plane for task or
inbox wake-ups before using these fallback intervals. Runtime execution limits
are configured through `agent.taskTimeoutSeconds`, `agent.maxLLMSteps`, and
`agent.taskSummaryMaxChars`. The chart passes those values to the operator, and
the operator injects the corresponding `SKQUAD_*` env vars into generated agent
pods.

### Built-in platform tools (S-152/BT-5a, ADR-0012)

- `agent.builtinToolsEnabled` (default `"true"`): default for the agent-pod
  kill switch `SKQUAD_BUILTIN_TOOLS_ENABLED`, passed to the operator as
  `SKQUAD_AGENT_BUILTIN_TOOLS_ENABLED` and injected into every agent pod.
  Set `"false"` to disable exec/web_fetch/web_search fleet-wide regardless of
  control-plane config (runtime treats `false`/`0`/`no` as disabled).
- `apiServer.search.secretName` (default `skquad-search-keys`),
  `apiServer.search.braveApiKey`, `apiServer.search.perplexityApiKey`
  (default `""`): web search provider keys for the control-plane search
  proxy (`SKQUAD_SEARCH_BRAVE_API_KEY` /
  `SKQUAD_SEARCH_PERPLEXITY_API_KEY` on the api-server only — never agent
  pods). Inline values render a chart-managed Secret (dev only); for GitOps,
  leave them empty and manage a SealedSecret named `search.secretName` with
  keys `brave-api-key` and/or `perplexity-api-key`. The default provider
  (duckduckgo) needs no key, so both refs are `optional: true`.

`web.apiBaseUrl` configures `NEXT_PUBLIC_SKQUAD_API_BASE_URL` for browser API
calls. The default `/api/v1` matches the chart ingress routing where `/api`
goes to the API server and `/` goes to the web app.

Public DNS/TLS values and external database hardening are still upcoming slices.
