<div align="center">

  <pre>
                                              
                                   █▄
       ▄▄                          ██
 ▄██▀█ ██ ▄█▀ ▄████ ██ ██ ▄▀▀█▄ ▄████
 ▀███▄ ████   ██ ██ ██ ██ ▄█▀██ ██ ██
█▄▄██▀▄██ ▀█▄▄▀████▄▀██▀█▄▀█▄██▄█▀███
                 ██                  
                                   ▀                                                  
  </pre>

  <div>
    <a href="https://github.com/rossbrigoli/skquad/actions/workflows/ci.yml">
      <img src="https://github.com/rossbrigoli/skquad/actions/workflows/ci.yml/badge.svg" alt="CI">
    </a>
    <a href="https://github.com/rossbrigoli/skquad/actions/workflows/images.yml">
      <img src="https://github.com/rossbrigoli/skquad/actions/workflows/images.yml/badge.svg" alt="Images">
    </a>
    <a href="https://github.com/rossbrigoli/skquad/blob/main/LICENSE">
      <img src="https://img.shields.io/badge/License-Apache%202.0-brightgreen.svg?style=flat" alt="License: Apache 2.0">
    </a>
    <img src="https://img.shields.io/badge/Status-Early%20vertical%20slice-yellow?style=flat" alt="Status: early vertical slice">
  </div>
</div>

---

> A Kubernetes-native control plane for building and operating squads of AI
> agents.

skquad models agent collaboration as squads, agents, Kanban tasks, messages,
resource grants, and task-scoped memory. It combines a Go control-plane API, a
Kubernetes operator, a Python agent runtime, and a central LiteLLM gateway so
that agent identity, permissions, model access, and lifecycle remain under
platform control.

> [!IMPORTANT]
> skquad is an early-stage project with a working vertical slice; it is not yet
> production-ready. The web application currently has an authenticated shell
> plus first-pass squad, agent, task, registry, grants, admin, identity, and
> chat workflows, but deeper product polish is still in progress, and the
> default Helm values intentionally use development authentication and
> credentials. See [Current status](#current-status) before deploying it.

## How it works

skquad separates management concerns from agent workloads:

- The **control plane** runs the API server, PostgreSQL, LiteLLM gateway, web
  application, and operator in `skquad-system`.
- The **data plane** runs each squad in its own Kubernetes namespace. Agent
  Deployments wake for assigned work and scale back to zero after becoming
  idle.

```text
Clients ───────► Control-plane API ───────► PostgreSQL
                      │
                      └── durable outbox ─► Squad and Agent CRs
                                                   │
                                               Operator
                                                   │
                                      per-squad namespaces and agent pods

Agent runtime ── tasks, messages, context ─────► Control-plane API
              └─ model requests ───────────────► LiteLLM gateway ─► providers
```

The control plane is the source of truth. Kubernetes custom resources are
derived execution state reconciled by the operator; agent runtimes do not
receive direct database access or provider credentials.

Read the [architecture overview](docs/ARCHITECTURE.md) and the
[runtime architecture ADR](docs/adr/0001-agent-runtime.md) for the design in
detail.

## Core concepts

| Concept | Meaning |
| --- | --- |
| **Squad** | A user-owned team of agents with a mission and operating model. Each squad is isolated in its own Kubernetes namespace. |
| **Agent** | A squad member with an independent runtime, identity, credentials, permissions, and bounded task context. |
| **Board and task** | Each squad has a Kanban board. Tasks are the durable unit of work assigned to agents. |
| **Resource registry** | The catalog of LLM providers, skills, tools, APIs, knowledge sources, and project workspaces available to the platform. |
| **Permission** | An explicit grant connecting an agent to a registry resource. |
| **Access grant** | Permission for another user to interact with a squad or one of its agents. |

## Components

| Component | Technology | Responsibility |
| --- | --- | --- |
| [`control-plane/`](control-plane/) | Go | REST API, human and agent authentication, domain state, authorization, audit events, and Kubernetes outbox. |
| [`operator/`](operator/) | Go, controller-runtime | Reconciles `Squad` and `Agent` resources into namespaces, workloads, and scale-to-zero lifecycle. |
| [`agent-runtime/`](agent-runtime/) | Python, FastAPI | Claims tasks, assembles context, loads granted plugins, calls models, drains messages, and reports outcomes. |
| [`llm-gateway/`](llm-gateway/) | LiteLLM | Central model proxy and per-agent virtual-key boundary. |
| [`web/`](web/) | Next.js | User interface with an authenticated shell plus squad, agent, task, registry, grant, admin, identity, and chat workflows. |
| [`charts/skquad/`](charts/skquad/) | Helm | Installs the control plane, CRDs, operator, gateway, web app, and optional PostgreSQL. |

## Current status

The repository currently implements:

- squad, agent, board, task, access-grant, resource-registry, permission,
  messaging, audit, and metering-read APIs;
- development authentication, OIDC token validation, and separate
  agent-service credentials;
- PostgreSQL persistence with a process-local in-memory option for API
  development and tests;
- durable outbox delivery of Squad and Agent custom-resource changes;
- operator reconciliation, deletion finalizers, workload wake-up, and
  idle scale-down;
- task claiming and completion, task-scoped context, bounded recent memory,
  lease-backed/fenced task execution, durable task-result summaries, inbox
  processing, and permission-filtered runtime plugins;
- LiteLLM gateway deployment and agent-scoped virtual-key provisioning; and
- Helm packaging plus CI validation and versioned container images.

Important gaps remain:

- the web application has initial squad, agent, task, registry, grant, admin,
  identity, and chat workflows, but rich detail views, charts, live updates,
  drag and drop, and guided onboarding are not complete;
- automatic LiteLLM key refresh or revocation after permission changes is not
  implemented;
- automatic memory embedding generation and durable artifact storage are planned;
- automatic task materialization for `delegate`/`handoff` messages is planned;
  and
- production ingress, TLS, external-database operations, observability, and
  network-policy hardening are not complete.

The detailed source-of-truth requirements are in
[`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md). The
[`implementation status ledger`](docs/implementation-status.md) reconciles the
design docs with what is implemented, what is in review, and what remains
deferred. Component READMEs describe the implemented slice and their next
steps.

## Development deployment

### Prerequisites

- a Kubernetes cluster and `kubectl` configured for it;
- Helm 3; and
- access to the container images configured in
  [`charts/skquad/values.yaml`](charts/skquad/values.yaml).

Install the default development stack:

```bash
helm upgrade --install skquad charts/skquad \
  --namespace skquad-system \
  --create-namespace

kubectl --namespace skquad-system get pods
kubectl --namespace skquad-system rollout status deployment/skquad-api-server
```

Verify the API from another terminal:

```bash
kubectl --namespace skquad-system port-forward service/skquad-api-server 8080:80
curl --fail http://127.0.0.1:8080/healthz
```

The default installation is suitable only for development. It enables a fixed
development admin identity, deploys an in-cluster PostgreSQL instance with a
development password, creates a development LiteLLM master key, and does not
enable ingress.

The default LiteLLM `model_list` is also empty. Before an agent can execute
model-backed work, configure at least one provider model and supply its
credentials through Kubernetes Secrets. The
[Helm chart guide](charts/skquad/README.md) covers provider configuration,
external secrets, image overrides, and production considerations.

## Local development

The toolchains currently used by CI are Go 1.26 (the control plane itself
declares Go 1.23), Python 3.11, Node.js 20, and Helm 3.

Run the API without Kubernetes or PostgreSQL:

```bash
cd control-plane
go test ./...
SKQUAD_ADDR=127.0.0.1:8080 go run ./cmd/api
```

This starts the API in development-auth mode with process-local storage; all
data is lost when the process exits. Set `SKQUAD_DATABASE_URL` for PostgreSQL,
or see the [control-plane guide](control-plane/README.md) for OIDC, Kubernetes,
runtime, and gateway configuration.

Run the main repository checks:

```bash
(cd control-plane && go vet ./... && go test ./...)
(cd operator && go vet ./... && go test ./...)
(cd agent-runtime && python -m pip install -e . && python -m unittest discover -s tests)
(cd web && npm ci && npm run build)
helm lint charts/skquad
helm template skquad charts/skquad --namespace skquad-system --include-crds >/dev/null
python3 -m unittest discover -s tests/integration
```

The CI workflow also builds and smoke-tests the LiteLLM gateway container. See
the [CI/CD guide](docs/ci-cd.md) for image names, tags, and publishing behavior,
and the [testing strategy](docs/testing-strategy.md) for the integration smoke
suite and cluster-required checks.

## Documentation

| Area | Documents |
| --- | --- |
| Product scope | [Requirements](docs/REQUIREMENTS.md), [domain model](docs/domain-model.md) |
| System design | [Architecture](docs/ARCHITECTURE.md), [data model](docs/data-model.md), [API design](docs/api-design.md), [implementation status](docs/implementation-status.md), [ADRs](docs/adr/) |
| Agent execution | [Runtime](docs/agent-runtime.md), [task lifecycle](docs/kanban-task-lifecycle.md), [messaging](docs/collaboration-messaging.md), [plugins](docs/plugin-architecture.md) |
| Platform services | [LLM gateway](docs/llm-gateway.md), [resource registry](docs/resource-registry.md), [operator and deployment](docs/deployment-operator.md), [operator runbook](docs/operator-runbook.md) |
| Operations and security | [Identity and security](docs/identity-security.md), [threat model](docs/security-threat-model.md), [observability and metering](docs/observability-metering.md), [CI/CD](docs/ci-cd.md), [testing strategy](docs/testing-strategy.md) |
| User experience | [Web application UX](docs/web-app-ux.md) |

## Security and production use

The security documentation describes the intended architecture as well as the
implemented controls; it should not be read as a certification of the current
release. At minimum, a non-development deployment must use OIDC, externally
managed secrets, a production PostgreSQL service, configured provider
credentials, TLS ingress, restrictive network policy, resource limits, and a
backup and monitoring strategy. Review the
[identity and security design](docs/identity-security.md),
[threat model](docs/security-threat-model.md), and
[chart guidance](charts/skquad/README.md) before exposing the system.

## License

Licensed under the [Apache License 2.0](LICENSE).
