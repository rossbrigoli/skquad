# WORKLOG

## 2026-08-28 11:04:05 ACST

- objective: Add README repository status header and ASCII skquad wordmark.
- files changed: `README.md`, `WORKLOG.md`.
- command/test run: `git diff --check -- README.md`.
- result: Replaced the plain top-level README heading with a centered HTML header containing an ASCII `skquad` wordmark, CI and Images workflow badges, Apache 2.0 badge, and early vertical-slice status badge, while preserving the existing project summary and production-readiness warning.

## 2026-08-28 10:57:52 ACST

- objective: Harden semantic memory with embeddings and trust boundaries.
- files changed: `control-plane/internal/domain/types.go`, `control-plane/internal/config/config.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/memory_test.go`, `control-plane/internal/storage/migrations/0003_agent_memory_trust.sql`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `charts/skquad/values.yaml`, `charts/skquad/templates/api-server-deployment.yaml`, `README.md`, `docs/agent-runtime.md`, `docs/data-model.md`, `docs/implementation-status.md`, `docs/plugin-architecture.md`, `docs/security-threat-model.md`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m unittest discover -s tests/integration`; `helm lint charts/skquad`; default and ingress/external-secret Kubernetes server dry-runs; `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `git diff --check`.
- result: Added memory trust/provenance/review metadata, raw-content separation, optional embedding vector persistence, and embedding-aware storage ranking when a query vector is supplied. Completion summaries now persist as bounded `raw_model_output` / `pending_review` memory, task-context limits expose whether embedding generation is enabled, Helm wires the disabled-by-default embedding config, and runtime prompts label memory as contextual evidence rather than executable instruction. Automatic embedding generation and approval/distillation workflow remain explicit follow-ups.

## 2026-08-28 02:50:06 ACST

- objective: Version database migrations with ledger and lock.
- files changed: `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/postgres_test.go`, `control-plane/internal/storage/migrations/0002_schema_migrations.sql`, `control-plane/README.md`, `docs/data-model.md`, `docs/implementation-status.md`, `docs/operator-runbook.md`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m unittest discover -s tests/integration`; `helm lint charts/skquad`; default and ingress/external-secret Kubernetes server dry-runs; `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `git diff --check`.
- result: Replaced repeated untracked startup migration execution with a Postgres advisory-lock-protected runner. Embedded migration files are sorted, checksummed with SHA-256, skipped when already recorded in `schema_migrations`, and rejected on checksum mismatch. Added `0002_schema_migrations.sql`, storage tests for embedded migration ordering/checksums, and docs for immutable migration history plus manual restore/forward-fix rollback boundaries.

## 2026-08-28 02:38:19 ACST

- objective: Reduce API Secret RBAC and agent network-policy blast radius.
- files changed: `operator/cmd/manager/main.go`, `operator/internal/controller/squad_controller.go`, `operator/internal/controller/squad_controller_test.go`, `operator/README.md`, `charts/skquad/templates/api-server-deployment.yaml`, `charts/skquad/templates/api-server-rbac.yaml`, `charts/skquad/templates/llm-gateway-deployment.yaml`, `charts/skquad/templates/operator-deployment.yaml`, `charts/skquad/templates/operator-rbac.yaml`, `charts/skquad/templates/web-deployment.yaml`, `charts/skquad/README.md`, `docs/deployment-operator.md`, `docs/operator-runbook.md`, `docs/security-threat-model.md`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m unittest discover -s tests/integration`; `helm lint charts/skquad`; rendered RBAC shape check; default and ingress/external-secret Kubernetes server dry-runs; `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `git diff --check`.
- result: Removed the chart-level API server ClusterRole/ClusterRoleBinding for generated agent Secrets. The operator now creates a per-squad namespace Role/RoleBinding that grants the API server ServiceAccount only generated Secret get/create/patch/update/delete authority in that squad namespace. Squad egress is tightened from broad control-plane namespace ports to pods labeled `api-server` or `llm-gateway` on the named `http` port. Chart-internal API URLs now target the API Service port instead of the pod port. Docs and operator tests now cover the new boundary; registry-derived external egress remains a documented follow-up.

## 2026-08-28 02:24:37 ACST

- objective: Harden OIDC identity, grant scopes, and audit guarantees.
- files changed: `control-plane/internal/auth/oidc.go`, `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `control-plane/README.md`, `docs/api-design.md`, `docs/data-model.md`, `docs/domain-model.md`, `docs/identity-security.md`, `docs/implementation-status.md`, `docs/security-threat-model.md`, `docs/web-app-ux.md`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m unittest discover -s tests/integration`; `helm lint charts/skquad`; default and ingress/external-secret Kubernetes server dry-runs; `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `git diff --check`.
- result: OIDC users are keyed by issuer + subject with email/email_verified/name as profile data; explicitly unverified email claims are rejected. User and agent access grants now enforce scoped permissions (`read`, `talk`, `ping`, `add_task`, `admin`, `*`) on the relevant paths, cross-squad denials are audited, and access-grant/agent-permission mutations fail closed if their required pre-mutation audit event cannot be recorded. Docs now describe the shipped boundaries and remaining broader audit-hardening gap.

## 2026-08-28 02:10:48 ACST

- objective: Add message retry, expiry, and dead-letter handling.
- files changed: `control-plane/internal/domain/types.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `README.md`, `control-plane/README.md`, `agent-runtime/README.md`, `docs/api-design.md`, `docs/collaboration-messaging.md`, `docs/agent-runtime.md`, `docs/data-model.md`, `docs/implementation-status.md`, `docs/operator-runbook.md`, `docs/testing-strategy.md`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m unittest discover -s tests/integration`; `helm lint charts/skquad`; default and ingress/external-secret Kubernetes server dry-runs; `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `git diff --check`.
- result: Added message attempt counts, max attempts, retry scheduling, expiry timestamps, and terminal reasons; runtime failure reporting via `/api/v1/agents/me/messages/{id}/fail`; retry-due inbox listing; pending-message wake logic that ignores delivered, expired, and dead messages while preserving scheduled retries; and docs describing remaining delegate/handoff materialization scope.

## 2026-08-28 01:02:44 ACST

- objective: Add CI-runnable integration and end-to-end smoke coverage.
- files changed: `.github/workflows/ci.yml`, `tests/integration/test_smoke.py`, `docs/testing-strategy.md`, `docs/ci-cd.md`, `README.md`, `WORKLOG.md`.
- command/test run: `python3 -m unittest discover -s tests/integration`.
- result: Added a root integration smoke suite that exercises representative control-plane runtime endpoints, Kubernetes outbox and CR-writer paths, operator reconciler contracts, runtime tests, and Helm render wiring. Wired the suite into GitHub Actions as `Integration smoke` and documented fast CI versus cluster-required verification boundaries. Local integration smoke suite passed.

## 2026-08-28 01:09:04 ACST

- objective: Create practical operator install and operations runbook.
- files changed: `docs/operator-runbook.md`, `docs/deployment-operator.md`, `charts/skquad/README.md`, `README.md`, `WORKLOG.md`.
- command/test run: `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system --include-crds`; production-oriented Helm render with external Postgres, OIDC, LiteLLM master-key, LiteLLM database, and provider API-key Secret refs; `git diff --check`.
- result: Added a Kubernetes operations runbook covering Helm install/upgrade, production-oriented values, namespace model, generated Secrets, ingress, troubleshooting, scale-to-zero, runtime readiness, RBAC/security notes, and uninstall guidance. Linked it from deployment, chart, and root documentation. Chart lint/render and whitespace checks passed.

## 2026-08-28 01:16:09 ACST

- objective: Reconcile documentation with the current implementation status.
- files changed: `docs/implementation-status.md`, `README.md`, major `docs/*.md` design documents, `WORKLOG.md`; Kanbunny board updated with new follow-up cards.
- command/test run: `rg` scan for stale status/implementation language; `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system --include-crds`; `python3 -m unittest discover -s tests/integration`; `git diff --check`; Kanbunny board count check.
- result: Added an implementation-status ledger that distinguishes implemented slices from deferred hardening work. Linked major design docs to the ledger, corrected stale overclaims around API Kubernetes writes, audit guarantees, LLM gateway enforcement, and metering, and created focused Kanbunny TODO cards for unresolved review findings. Remaining `rg` hits are intentional boundary/status language.

## 2026-08-27 16:33:03 ACST

- objective: Continue Skquad control-plane implementation.
- files changed: `control-plane/cmd/api/main.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/go.mod`, `control-plane/go.sum`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18080 go run ./cmd/api` with curl smoke checks for `/api/v1/auth/me` and `/healthz`.
- result: Added runnable dev API server with dev auth, squad/agent/board/task REST slice, in-memory development store, and handler tests. Tests and smoke check passed.

## 2026-08-27 16:36:07 ACST

- objective: Continue Skquad control-plane implementation with persistent storage.
- files changed: `control-plane/cmd/api/main.go`, `control-plane/internal/storage/postgres.go`, `control-plane/go.mod`, `control-plane/go.sum`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18082 go run ./cmd/api` with curl smoke checks for squad, agent, task creation and task move.
- result: Added Postgres store selected by `SKQUAD_DATABASE_URL`, embedded idempotent migration execution, and Postgres-backed user/squad/agent/identity/board/task methods. Dev fallback remains working.

## 2026-08-27 16:42:01 ACST

- objective: Continue Skquad control-plane implementation with OIDC authentication.
- files changed: `control-plane/internal/auth/oidc.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/config/config.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/cmd/api/main.go`, `control-plane/go.mod`, `control-plane/go.sum`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18083 go run ./cmd/api` with curl smoke check for `/api/v1/auth/me`.
- result: Added OIDC bearer JWT authenticator, OIDC config validation, first-login user provisioning, role preservation on existing users, and tests for OIDC success/failure paths. Dev auth still returns `platform_admin`.

## 2026-08-27 16:45:37 ACST

- objective: Continue Skquad control-plane implementation with access grants and read authorization.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18084 go run ./cmd/api` with curl smoke check for access-grant routes.
- result: Added access grant create/list/delete endpoints, in-memory and Postgres grant storage methods, granted-user read access checks for squad resources, and tests proving grants allow reads without allowing writes.

## 2026-08-27 16:52:45 ACST

- objective: Continue Skquad control-plane implementation with registry endpoints.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18085 go run ./cmd/api` with curl smoke checks for LLM provider and skill registry routes.
- result: Added admin-only registry write endpoints, authenticated registry reads, in-memory and Postgres persistence for LLM providers and generic resources, and tests for registry CRUD/deprecation and non-admin write rejection.

## 2026-08-27 16:56:15 ACST

- objective: Continue Skquad control-plane implementation with audit and metering reads.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18086 go run ./cmd/api` with curl smoke checks for health, squad create, agent create, squad audit, agent metering, and platform metering summary.
- result: Added squad/agent/platform metering read endpoints, squad/admin audit read endpoints, best-effort audit recording for control-plane mutations, and in-memory/Postgres audit + metering persistence. Tests and smoke check passed.

## 2026-08-27 16:58:39 ACST

- objective: Continue Skquad control-plane implementation with Kubernetes Squad/Agent CR writers.
- files changed: `control-plane/cmd/api/main.go`, `control-plane/internal/config/config.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/kube/cr_writer.go`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18087 go run ./cmd/api` with curl smoke checks for health, squad create, agent create, squad audit, and platform metering summary.
- result: Added optional `SKQUAD_K8S_ENABLED` CR writer startup path, server-side apply/delete writer for documented `skquad.io/v1` Squad and Agent resources, config for CR namespace/token/group/image, and fake-writer handler tests for squad/agent create/update/delete mirroring. Tests and smoke check passed.

## 2026-08-27 17:05:11 ACST

- objective: Continue Skquad control-plane implementation with agent identity and registry permissions.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...`; `SKQUAD_ADDR=127.0.0.1:18088 go run ./cmd/api` with curl smoke checks for health, squad create, agent create, identity create/rotate, LLM provider create, permission set, and squad audit.
- result: Added owner-only agent identity create/rotate endpoints with generated K8s credential and LLM gateway virtual-key refs, in-memory identity persistence, owner-only agent permission list/set endpoints, in-memory/Postgres permission storage, registry-resource validation, audit entries, and handler tests. Tests and smoke check passed.

## 2026-08-27 17:07:15 ACST

- objective: Continue Skquad operator implementation with Kubernetes API types.
- files changed: `operator/internal/api/v1/types.go`, `operator/go.mod`, `operator/go.sum`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `go mod tidy`; `go test ./...` in `operator/`.
- result: Added `skquad.io/v1` Squad and Agent Kubernetes API structs, list types, status structs, JSON extension fields for operating model/permissions, and manual `runtime.Object` deep-copy support. Operator module compiles.

## 2026-08-27 19:26:50 ACST

- objective: Continue Skquad operator implementation with scheme registration and the first Squad reconciler.
- files changed: `control-plane/internal/kube/cr_writer.go`, `control-plane/README.md`, `operator/cmd/manager/main.go`, `operator/internal/api/v1/types.go`, `operator/internal/controller/squad_controller.go`, `operator/internal/controller/squad_controller_test.go`, `operator/go.mod`, `operator/go.sum`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `go get sigs.k8s.io/controller-runtime@latest`; `go mod tidy`; `go test ./...` in `operator/`.
- result: Added `skquad.io/v1` scheme registration, controller-runtime manager startup with health/readiness probes, a first Squad reconciler that creates and labels the configured squad namespace and writes ready status, and fake-client reconciler tests. Updated the control-plane CR writer to include `Squad.spec.namespace`.

## 2026-08-27 19:29:45 ACST

- objective: Continue Skquad operator implementation with squad namespace base resources.
- files changed: `operator/internal/controller/squad_controller.go`, `operator/internal/controller/squad_controller_test.go`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `operator/`.
- result: Extended the Squad reconciler to create the `skquad-agent` ServiceAccount, default-deny NetworkPolicy, and starter ResourceQuota in each squad namespace. Expanded fake-client tests to verify all base resources.

## 2026-08-27 19:31:29 ACST

- objective: Continue Skquad operator implementation with the first Agent reconciler.
- files changed: `operator/cmd/manager/main.go`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `operator/`.
- result: Added an Agent reconciler that resolves the owning squad namespace, creates/updates the agent Deployment, applies scale-to-zero replicas from `spec.desiredActive`, uses the squad agent ServiceAccount, sets runtime env vars, and records ready status. Added fake-client tests for active and inactive agents.

## 2026-08-27 19:44:21 ACST

- objective: Continue Skquad operator implementation with agent Secret mounts and chart CRDs.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/kube/cr_writer.go`, `control-plane/internal/kube/cr_writer_test.go`, `operator/internal/api/v1/types.go`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `operator/README.md`, `docs/deployment-operator.md`, `charts/skquad/README.md`, `charts/skquad/crds/skquad.io_agents.yaml`, `charts/skquad/crds/skquad.io_squads.yaml`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; `kubectl apply --dry-run=server` for both CRDs with lab kubeconfig.
- result: Added optional `credentialSecret` and `virtualKeySecret` fields to Agent CRs, mounted those Secrets read-only into agent Deployments, threaded agent identity refs into control-plane CR writes, added CRD YAML for Squad and Agent resources, and validated tests/chart/CRDs.

## 2026-08-27 19:51:35 ACST

- objective: Continue Skquad chart implementation with operator RBAC and Deployment templates.
- files changed: `charts/skquad/templates/_helpers.tpl`, `charts/skquad/templates/namespace.yaml`, `charts/skquad/templates/operator-serviceaccount.yaml`, `charts/skquad/templates/operator-rbac.yaml`, `charts/skquad/templates/operator-deployment.yaml`, `charts/skquad/values.yaml`, `charts/skquad/README.md`, `docs/deployment-operator.md`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; `helm template skquad charts/skquad -n default --include-crds --set namespace.create=false | kubectl apply --dry-run=server -f -`.
- result: Added Helm helpers, optional namespace creation, operator ServiceAccount, ClusterRole/Binding, hardened operator Deployment with probes and leader-election args, and validation against the lab Kubernetes API via server dry-run.

## 2026-08-27 19:56:32 ACST

- objective: Continue Skquad operator implementation with starter egress allow policies.
- files changed: `operator/internal/controller/squad_controller.go`, `operator/internal/controller/squad_controller_test.go`, `operator/README.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; `helm template skquad charts/skquad -n default --include-crds --set namespace.create=false | kubectl apply --dry-run=server -f -`.
- result: Extended the Squad reconciler to keep default-deny isolation while adding DNS egress to `kube-system` and platform egress back to the control-plane namespace on HTTP/gateway/Postgres ports. Updated fake-client tests and docs.

## 2026-08-27 20:04:07 ACST

- objective: Continue Skquad chart implementation with control-plane/runtime workload templates.
- files changed: `charts/skquad/templates/_helpers.tpl`, `charts/skquad/templates/api-server-deployment.yaml`, `charts/skquad/templates/api-server-rbac.yaml`, `charts/skquad/templates/api-server-service.yaml`, `charts/skquad/templates/api-server-serviceaccount.yaml`, `charts/skquad/templates/llm-gateway-configmap.yaml`, `charts/skquad/templates/llm-gateway-deployment.yaml`, `charts/skquad/templates/llm-gateway-service.yaml`, `charts/skquad/templates/postgres-secret.yaml`, `charts/skquad/templates/postgres-service.yaml`, `charts/skquad/templates/postgres-statefulset.yaml`, `charts/skquad/templates/web-deployment.yaml`, `charts/skquad/templates/web-service.yaml`, `charts/skquad/values.yaml`, `charts/skquad/README.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; `helm template skquad charts/skquad -n default --include-crds --set namespace.create=false | kubectl apply --dry-run=server -f -`.
- result: Added chart-managed API server, LLM gateway, web, and Postgres resources. API server now gets namespaced CR-writer RBAC and a Postgres URL from the chart Secret by default; all rendered resources validate via Kubernetes server dry-run.

## 2026-08-27 20:10:36 ACST

- objective: Continue Skquad chart implementation with ingress and production secret knobs.
- files changed: `charts/skquad/templates/_helpers.tpl`, `charts/skquad/templates/api-server-deployment.yaml`, `charts/skquad/templates/ingress.yaml`, `charts/skquad/templates/postgres-secret.yaml`, `charts/skquad/templates/postgres-statefulset.yaml`, `charts/skquad/values.yaml`, `charts/skquad/README.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; Kubernetes server dry-runs for enabled ingress and external Postgres Secret rendering.
- result: Added optional Kubernetes Ingress routing `/api` to the API server and `/` to the web app. Added `postgres.existingSecret`, configurable secret keys, and `apiServer.databaseUrlSecret` so production installs can use pre-created database Secrets instead of chart-managed dev credentials.

## 2026-08-27 20:18:44 ACST

- objective: Continue Skquad agent runtime implementation with the bootstrap contract.
- files changed: `agent-runtime/pyproject.toml`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `agent-runtime/README.md`, `docs/agent-runtime.md`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `python3 -m unittest discover -s tests` in `agent-runtime/`; `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py`; `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; rendered chart server dry-run with ingress enabled.
- result: Replaced the placeholder agent runtime with bootstrap config loading, mounted Secret directory reading, FastAPI health/readiness app factory, and a console script. The Agent reconciler now passes runtime credential path env vars and configures `/healthz` and `/readyz` probes. Runtime tests pass locally with the FastAPI endpoint test skipped because FastAPI is not installed in the ambient Python.

## 2026-08-27 20:18:57 ACST

- objective: Continue Skquad control-plane/runtime implementation with agent-facing task status endpoints.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/config/config.go`, `control-plane/internal/kube/cr_writer.go`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `operator/internal/api/v1/types.go`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `charts/skquad/crds/skquad.io_agents.yaml`, `charts/skquad/templates/api-server-deployment.yaml`, `charts/skquad/values.yaml`, `control-plane/README.md`, `agent-runtime/README.md`, `operator/README.md`, `charts/skquad/README.md`, `docs/api-design.md`, `docs/agent-runtime.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Added agent-authenticated `/api/v1/agents/me` task list/claim/start/complete/block and heartbeat endpoints, with in-memory and Postgres task claim support. Added the runtime control-plane client and `poll_once` idle/busy reporting primitive. Threaded control-plane and LLM gateway URLs through chart values, API server config, Agent CRs, and operator deployment env injection. All checks passed.

## 2026-08-27 20:25:11 ACST

- objective: Continue Skquad control-plane implementation with real agent credential material.
- files changed: `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/kube/cr_writer.go`, `control-plane/internal/kube/cr_writer_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `charts/skquad/templates/api-server-rbac.yaml`, `control-plane/README.md`, `charts/skquad/README.md`, `docs/api-design.md`, `docs/data-model.md`, `docs/identity-security.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Agent identity create/rotate now generates random runtime credential material, stores only a SHA-256 verifier hash, writes the raw token to the generated Kubernetes Secret when the CR writer is enabled, and cleans up newly written Secrets on storage failure. Runtime auth now accepts hashed credentials and rejects public credential refs for new identities. The chart grants the API server Secret write/delete RBAC for generated agent credentials. All checks passed.

## 2026-08-27 20:30:20 ACST

- objective: Continue Skquad control-plane/operator scale-up behavior for pending assigned tasks.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/README.md`, `operator/README.md`, `docs/deployment-operator.md`, `docs/kanban-task-lifecycle.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Task assignment, claim, completion, block, move, update, delete, and heartbeat paths now resync the affected agent status from assigned `todo`/`in-progress` work and mirror the Agent CR, setting `desiredActive` true while work remains and false once the agent is idle with no pending assigned work. Added tests for wake-on-assignment and staying busy while another assigned task remains. Updated Kanbunny Control plane implementation card with progress.

## 2026-08-27 20:32:57 ACST

- objective: Continue Skquad operator implementation with idle-timeout-aware scale-down.
- files changed: `operator/internal/api/v1/types.go`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `charts/skquad/crds/skquad.io_agents.yaml`, `operator/README.md`, `docs/deployment-operator.md`, `docs/kanban-task-lifecycle.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Added `Agent.status.idleSince` and operator logic that keeps an inactive agent Deployment at one replica until `spec.idleTimeout` elapses, then scales to zero. The reconciler clears idle tracking when `desiredActive` becomes true, requeues for the remaining timeout while waiting, and records status reasons for ready/waiting/scaled-to-zero. Added controller tests for waiting and expired idle timeout behavior. Did not edit the Kanbunny operator card because it is already in review.

## 2026-08-27 20:35:38 ACST

- objective: Continue Skquad agent runtime implementation with handler-driven task execution primitives.
- files changed: `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `docs/agent-runtime.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Added `TaskResult`, `TaskHandler`, `run_task_once`, and `run_task_loop` primitives. The runtime can now claim a task, report busy, invoke an injected handler, complete to `in-review` or `done`, block on handler failure/invalid status, then report idle. Added runtime tests for success, handler failure, invalid status, and no-task heartbeat behavior. Updated Kanbunny Agent runtime implementation card with progress.

## 2026-08-27 20:43:52 ACST

- objective: Continue Skquad agent runtime implementation with the default LiteLLM/plugin task handler.
- files changed: `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `operator/README.md`, `docs/agent-runtime.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Added `LiteLLMTaskHandler`, plugin tool schemas/invocation, status marker parsing, and runtime process task-loop startup behind `SKQUAD_TASK_LOOP_ENABLED`. Operator now sets the task-loop env var for agent pods. Full verification passed.

## 2026-08-27 20:50:04 ACST

- objective: Continue Skquad agent runtime implementation with permission-scoped runtime resource discovery.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/README.md`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `docs/api-design.md`, `docs/agent-runtime.md`, `docs/resource-registry.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default chart rendering and ingress/external-secret rendering.
- result: Added agent-authenticated `/api/v1/agents/me/resources` for active granted resource descriptors without provider API-key refs or resource auth refs. Runtime client can list descriptors and the default LiteLLM handler includes them in task context while keeping executable plugin invocation limited to loaded plugins. Full verification passed.

## 2026-08-27 21:08:00 ACST

- objective: Split epic-sized Skquad Kanbunny work into concrete execution cards.
- files changed: `WORKLOG.md`; Kanbunny board `skquad` updated out-of-band.
- command/test run: `curl -s http://kanbunny.lab/api/boards/e5b3865d-1d64-482c-b153-411b84a9b132/cards`; Kanbunny card create/update calls through the local API; board priority recompute.
- result: Created 19 detailed `todo` cards covering control-plane gaps, runtime gaps, LLM gateway, web app, CI/CD, testing, and documentation work. Moved the broad epic cards (`Control plane implementation`, `Agent runtime implementation`, `LLM gateway implementation`, `Web app implementation`, `CI/CD pipeline`, `Testing strategy & implementation`, and `User & operator documentation`) to `in-review` as superseded parent trackers with child-card references. Verified the board now has 19 `done`, 8 `in-review`, 19 `todo`, and 0 `in-progress` cards.

## 2026-08-27 21:09:46 ACST

- objective: Fix runtime virtual-key delivery and readiness semantics.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/kube/cr_writer_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/README.md`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `operator/README.md`, `docs/identity-security.md`, `docs/agent-runtime.md`, `docs/deployment-operator.md`, `docs/api-design.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering.
- result: Agent identity create/rotate now generates and writes separate Kubernetes Secrets for the runtime credential and LLM gateway virtual key, stores mountable `k8s://...` refs for both, rotates both refs together, and deletes superseded Secrets best-effort. Runtime readiness now requires control-plane/gateway config plus both mounted Secret values when the task loop is enabled, while allowing a reduced readiness path when the task loop is disabled.

## 2026-08-27 21:12:01 ACST

- objective: Align Postgres migrations with the documented data model.
- files changed: `control-plane/internal/storage/migrations/0001_init.sql`, `control-plane/README.md`, `docs/data-model.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering.
- result: Added the pgvector extension plus durable `messages` and `agent_memory` schema foundations to the embedded migration. Updated data-model docs to use the same `to_agent_id` inbox field and to mark message APIs/runtime memory integration as follow-up implementation slices.

## 2026-08-27 21:16:44 ACST

- objective: Implement agent messaging and inbox APIs in the control plane.
- files changed: `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/README.md`, `docs/api-design.md`, `docs/collaboration-messaging.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering.
- result: Added durable message domain/storage support, agent-authenticated inbox list/send/ack endpoints, user-facing agent chat enqueue/history endpoints, same-squad messaging, grant-checked cross-squad messaging, audit recording, and pending-message wake/idle mirroring through Agent CR desired activity. Docs now distinguish implemented queue behavior from later runtime inbox draining and delegate/handoff task materialization.

## 2026-08-27 21:25:17 ACST

- objective: Normalize Skquad provider, model, and gateway contracts.
- files changed: `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/kube/cr_writer.go`, `control-plane/internal/kube/cr_writer_test.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `operator/internal/api/v1/types.go`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `charts/skquad/crds/skquad.io_agents.yaml`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `operator/README.md`, `docs/agent-runtime.md`, `docs/api-design.md`, `docs/data-model.md`, `docs/deployment-operator.md`, `docs/domain-model.md`, `docs/llm-gateway.md`, `docs/resource-registry.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering; `git diff --check`.
- result: Added explicit `default_model`/`defaultModel` contract alongside existing provider IDs, persisted it in Postgres, surfaced it through Agent CRs, passed `SKQUAD_DEFAULT_MODEL` into agent pods, and updated the runtime LiteLLM handler to use the model alias while retaining the old provider env as a legacy fallback. LLM provider registry responses now include `default_model`, and runtime resource descriptors include sanitized provider routing metadata without secret refs. Docs now distinguish registry provider IDs from LiteLLM/gateway model aliases.

## 2026-08-27 21:27:42 ACST

- objective: Harden API design cross-cutting behavior documentation and status validation coverage.
- files changed: `control-plane/internal/httpapi/server_test.go`, `docs/api-design.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering; `git diff --check`.
- result: Updated API docs to explicitly defer cursor pagination and persisted idempotency keys instead of claiming they are implemented. Removed the stale `/tasks/:id/assign` route from the documented current API, documented accepted task status semantics, and added handler coverage proving invalid user task moves, invalid agent completion statuses, and invalid heartbeat statuses return the standard error envelope.

## 2026-08-27 21:38:52 ACST

- objective: Add dynamic plugin discovery and loading to the agent runtime.
- files changed: `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `docs/agent-runtime.md`, `docs/plugin-architecture.md`, `WORKLOG.md`.
- command/test run: `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/__init__.py skquad_runtime/runtime.py tests/test_runtime.py` in `agent-runtime/`; `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering; `git diff --check`.
- result: Added importlib-based plugin loading from `SKQUAD_PLUGIN_MODULES`, optional `SKQUAD_ENABLED_PLUGINS` name filtering, plugin interface validation, startup use of loaded plugins for the LiteLLM task handler, and predictable blocked task results for unknown tool calls or plugin invocation failures. Docs now describe supported import specs and the remaining registry package-installation follow-up.

## 2026-08-27 21:47:38 ACST

- objective: Implement task-scoped context and memory access for runtime task execution.
- files changed: `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `README.md`, `control-plane/README.md`, `agent-runtime/README.md`, `docs/api-design.md`, `docs/agent-runtime.md`, `docs/data-model.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering.
- result: Added agent-memory domain/storage support in memory and Postgres stores, added agent-authenticated `/api/v1/agents/me/tasks/{taskID}/context` for assigned-task metadata, granted active resource descriptors, bounded recent scoped memory, and payload limits. Agent completion can now opt in to persisting a bounded summary as memory, and the runtime fetches task context before calling LiteLLM and persists non-empty completion summaries. Docs now describe implemented recent-memory behavior and leave semantic vector search/artifacts as follow-up work.

## 2026-08-27 21:55:05 ACST

- objective: Add runtime inbox draining for agent collaboration.
- files changed: `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `operator/README.md`, `charts/skquad/values.yaml`, `charts/skquad/templates/operator-deployment.yaml`, `charts/skquad/README.md`, `README.md`, `docs/agent-runtime.md`, `docs/collaboration-messaging.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs for default rendering and ingress/external-secret rendering; `git diff --check`.
- result: Added runtime inbox message models, control-plane client methods for pending inbox reads and message acknowledgements, `MessageHandler`/`MessageResult`, `DefaultMessageHandler`, and `run_inbox_once`. The runtime loop now drains a bounded inbox batch and processes at most one task per iteration for fairness. Operator/chart wiring now injects task poll interval, inbox poll interval, and inbox batch size env vars into agent pods. Docs describe at-least-once retry-by-not-acknowledging and leave durable failure counters/dead-letter transitions as follow-up work.

## 2026-08-27 22:06:30 ACST

- objective: Address critical review findings before continuing feature work: operator finalizer cleanup and runtime per-task permission/context refresh.
- files changed: `operator/internal/controller/squad_controller.go`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/squad_controller_test.go`, `operator/internal/controller/agent_controller_test.go`, `operator/README.md`, `charts/skquad/templates/operator-rbac.yaml`, `charts/skquad/README.md`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `docs/deployment-operator.md`, `docs/agent-runtime.md`, `docs/plugin-architecture.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/runtime.py` in `agent-runtime/`; full project verification still pending for this slice.
- result: Added `skquad.io/squad-cleanup` and `skquad.io/agent-cleanup` finalizers plus the operator RBAC needed to update CR finalizers and delete managed resources. Squad deletion now explicitly removes managed base resources and the managed namespace; Agent deletion now explicitly removes the cross-namespace Deployment. Runtime `LiteLLMTaskHandler` no longer caches discovered resources on the long-lived handler and refreshes task context for each task. Loaded plugin modules are now filtered against current task grants before tool schemas are exposed or tool calls are invoked. Docs call out that finalizers are implemented but the transactional Kubernetes outbox is still a required follow-up. Kanbunny corrective card moved to `in-review`; new `Implement transactional Kubernetes outbox worker` follow-up card created in `todo`.

## 2026-08-27 22:20:43 ACST

- objective: Implement the transactional Kubernetes outbox worker for Squad/Agent CR convergence.
- files changed: `README.md`, `control-plane/README.md`, `control-plane/cmd/api/main.go`, `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/kube/outbox_worker.go`, `control-plane/internal/kube/outbox_worker_test.go`, `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `docs/api-design.md`, `docs/data-model.md`, `docs/deployment-operator.md`, `docs/identity-security.md`, `operator/README.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; Kubernetes server dry-runs from the `default` namespace for default rendering and ingress/external-secret rendering; `git diff --check`.
- result: Added durable Kubernetes outbox domain/storage models, Postgres table/indexes, in-memory test semantics, and a control-plane worker that leases pending/failed events, applies Squad/Agent CR writes/deletes idempotently, and records applied/failed state with retry scheduling. Squad/Agent create/update/delete/status and identity mutations now enqueue non-secret CR intents with the store mutation, while HTTP handlers no longer fail accepted domain mutations because a CR write is temporarily unavailable. Delete events preserve non-secret payload after rows are removed. Docs now describe accepted-intent semantics and the remaining synchronous boundary for raw credential/virtual-key Secret writes.

## 2026-08-27 22:33:35 ACST

- objective: Implement LiteLLM gateway bootstrap and agent virtual-key provisioning.
- files changed: `README.md`, `control-plane/internal/config/config.go`, `control-plane/internal/httpapi/litellm_client.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/README.md`, `llm-gateway/config.yaml`, `llm-gateway/README.md`, `charts/skquad/values.yaml`, `charts/skquad/templates/_helpers.tpl`, `charts/skquad/templates/api-server-deployment.yaml`, `charts/skquad/templates/llm-gateway-deployment.yaml`, `charts/skquad/templates/llm-gateway-secret.yaml`, `charts/skquad/templates/llm-gateway-service.yaml`, `charts/skquad/README.md`, `docs/deployment-operator.md`, `docs/llm-gateway.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; `helm template` default render; Kubernetes server dry-runs in the existing `default` namespace for default and provider-configured gateway renders; `git diff --check`.
- result: Added LiteLLM admin config/env support, chart-managed or external LiteLLM master-key Secret wiring, authenticated LiteLLM proxy bootstrap config, gateway DATABASE_URL wiring, health probes, corrected gateway Service port, and control-plane key generation through LiteLLM `/key/generate` using currently granted active provider model aliases. Raw generated virtual keys are written only to the agent Secret; docs call out remaining metering callback and grant-change key update/revocation follow-ups. Full verification passed.

## 2026-08-27 22:42:40 ACST

- objective: Replace placeholder GitHub Actions docs-listing CI with real validation jobs.
- files changed: `.github/workflows/ci.yml`, `.gitignore`, `README.md`, `docs/ci-cd.md`, `agent-runtime/skquad_runtime/runtime.py`, `web/package.json`, `web/package-lock.json`, `web/next-env.d.ts`, `web/tsconfig.json`, `web/src/app/layout.tsx`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; default and provider-configured `helm template`; `npm ci && NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; GitHub Actions run `https://github.com/rossbrigoli/skquad/actions/runs/33075942532`.
- result: Replaced the docs-only CI workflow with component jobs for Go control-plane, Go operator, Python agent runtime, LLM gateway metadata/config validation, Helm lint/render, and deterministic Next.js web build. The first CI run caught a real FastAPI readiness failure that was hidden locally because FastAPI was not installed; fixed `/readyz` to return explicit `JSONResponse` status codes. Added a minimal valid Next.js root layout, committed web lockfile/TypeScript metadata, documented CI/CD boundaries, and left image build/publish as the separate `Add container build and publish pipeline` follow-up. Final CI passed.

## 2026-08-27 23:00:35 ACST

- objective: Add container build and publish pipeline for Skquad deployable components.
- files changed: `.github/workflows/images.yml`, `control-plane/Dockerfile`, `control-plane/.dockerignore`, `operator/Dockerfile`, `operator/.dockerignore`, `agent-runtime/Dockerfile`, `agent-runtime/.dockerignore`, `llm-gateway/Dockerfile`, `llm-gateway/.dockerignore`, `web/Dockerfile`, `web/.dockerignore`, `charts/skquad/values.yaml`, `charts/skquad/README.md`, `docs/ci-cd.md`, `README.md`, `WORKLOG.md`.
- command/test run: local `podman build` for API server, operator, agent runtime, LLM gateway, and web images; `go vet ./...` and `go test ./...` in `control-plane/`; `go vet ./...` and `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `npm run build` in `web/`; workflow YAML parse check; `helm lint charts/skquad`; default/provider `helm template`; Kubernetes server-side dry-runs for default and ingress/external-secret/provider renders.
- result: Added deployable image definitions for all charted services and an Images GitHub Actions workflow. Pull requests build all images; pushes to `main` and `v*.*.*` tags publish GHCR images with `sha-*`, `latest`, and semver tags. Docker Hub mirroring is included when repository secrets are configured, but the local GitHub token could not set those secrets (`HTTP 403` on Actions secrets API), so GHCR is the guaranteed publish target. Helm chart defaults now reference the GHCR image family. Local image builds and validation checks passed.

## 2026-08-27 23:11:00 ACST

- objective: Document initial lab GitOps deployment automation for Skquad.
- files changed: `docs/ci-cd.md`, `docs/deployment-operator.md`, `WORKLOG.md`; related GitOps repo changes in `/home/ross/projects/k3s-cluster`.
- command/test run: Kubernetes server-side dry-run of `/home/ross/projects/k3s-cluster/apps/app-skquad.yaml`; documentation diff check pending before commit.
- result: Documented the new lab ArgoCD Application target, its GHCR `latest` image-tag strategy, and the explicit remaining gap: immutable image-tag promotion through GitOps updates or ArgoCD Image Updater.

## 2026-08-27 23:15:00 ACST

- objective: Fix LLM gateway image crash found by the first GitOps deployment.
- files changed: `llm-gateway/pyproject.toml`, `llm-gateway/README.md`, `.github/workflows/ci.yml`, `WORKLOG.md`.
- command/test run: `podman build -t localhost/skquad-llm-gateway:prisma-test ./llm-gateway`; `podman run --rm --entrypoint python localhost/skquad-llm-gateway:prisma-test -c 'import prisma.engine.errors; print(...)'`; LLM gateway metadata validation.
- result: Added explicit `prisma>=0.15` dependency because LiteLLM imports Prisma database error modules during proxy startup with persistent key state. CI metadata validation now requires the dependency so the crash is caught before deployment.

## 2026-08-28 00:25:00 ACST

- objective: Generate LiteLLM Prisma client artifacts in the gateway image and strengthen CI so the lab startup failure is caught before deployment.
- files changed: `.github/workflows/ci.yml`, `llm-gateway/Dockerfile`, `llm-gateway/README.md`, `docs/ci-cd.md`, `WORKLOG.md`.
- command/test run: `podman build -t localhost/skquad-llm-gateway:prisma-generated ./llm-gateway`; `podman run --rm --entrypoint python localhost/skquad-llm-gateway:prisma-generated -c "..."`; workflow YAML parse check; `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system`.
- result: Gateway image now installs `libatomic1`, runs `prisma generate` against LiteLLM's packaged Prisma schema during image build, and CI builds/smoke-tests the gateway image with a real `python -c` check for generated Prisma client artifacts.

## 2026-08-28 00:39:00 ACST

- objective: Add a LiteLLM gateway startup probe window after the lab deployment showed runtime database/bootstrap preparation exceeded the zero-delay liveness probe.
- files changed: `charts/skquad/values.yaml`, `charts/skquad/templates/llm-gateway-deployment.yaml`, `charts/skquad/README.md`, `WORKLOG.md`.
- command/test run: `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system`; `helm template skquad charts/skquad --namespace skquad-system | kubectl apply --dry-run=server -n skquad-system -f -`.
- result: Added configurable `llmGateway.probes` values with a default startup probe that gives LiteLLM up to 10 minutes to finish persistent proxy startup before liveness restarts are allowed.

## 2026-08-28 00:45:00 ACST

- objective: Separate LiteLLM persistent state from the Skquad control-plane schema after the lab deployment exposed Prisma P3005 on the non-empty `public` schema.
- files changed: `charts/skquad/values.yaml`, `charts/skquad/templates/llm-gateway-deployment.yaml`, `charts/skquad/README.md`, `llm-gateway/README.md`, `WORKLOG.md`.
- command/test run: rendered gateway env inspection; `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system`; `helm template skquad charts/skquad --namespace skquad-system | kubectl apply --server-side --field-manager=argocd-controller --force-conflicts --dry-run=server -n skquad-system -f -`.
- result: Chart-managed Postgres now gives LiteLLM a `DATABASE_URL` targeting `schema=litellm`, external LiteLLM DB URLs can be provided separately from the API server DB URL, and the gateway gets writable `LITELLM_MIGRATION_DIR=/tmp/litellm-migrations`.

## 2026-08-28 01:12:00 ACST

- objective: Fix the LiteLLM gateway image/runtime smoke coverage after lab still exposed a missing Prisma query-engine boundary.
- files changed: `.github/workflows/ci.yml`, `llm-gateway/Dockerfile`, `llm-gateway/README.md`, `charts/skquad/README.md`, `docs/ci-cd.md`, `WORKLOG.md`.
- command/test run: `podman build -t localhost/skquad-llm-gateway:prisma-fetch ./llm-gateway`; disposable Postgres-backed Prisma connection smoke test; disposable Postgres-backed LiteLLM startup/readiness smoke test.
- result: Gateway image now fetches Prisma query-engine binaries into the non-root runtime user's cache during build. CI now feeds Python heredocs with interactive stdin, verifies the generated Prisma client can connect to Postgres, and starts the actual LiteLLM proxy against Postgres until readiness passes.

## 2026-08-28 01:28:00 ACST

- objective: Remove ArgoCD StatefulSet drift for the bundled development Postgres chart.
- files changed: `charts/skquad/templates/postgres-statefulset.yaml`, `WORKLOG.md`.
- command/test run: `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system`; Kubernetes server-side dry-run against the lab API.
- result: The Postgres StatefulSet template now renders Kubernetes default pod fields, termination message fields, PVC template `apiVersion`/`kind`, and `volumeMode` so ArgoCD server-side apply comparison is stable.

## 2026-08-28 00:07:30 ACST

- objective: Add runtime execution observability and configurable execution limits.
- files changed: `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/skquad_runtime/__init__.py`, `agent-runtime/tests/test_runtime.py`, `agent-runtime/README.md`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `charts/skquad/values.yaml`, `charts/skquad/templates/operator-deployment.yaml`, `charts/skquad/README.md`, `operator/README.md`, `docs/agent-runtime.md`, `docs/observability-metering.md`, `docs/deployment-operator.md`, `WORKLOG.md`.
- command/test run: `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; default Helm render; Kubernetes server-side dry-run against the lab API; `npm ci && NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`.
- result: Added in-process runtime counters, `/status`, dependency-free Prometheus text at `/metrics`, bounded handler execution via `SKQUAD_TASK_TIMEOUT_SECONDS`, LiteLLM step cap via `SKQUAD_MAX_LLM_STEPS`, completion summary cap via `SKQUAD_TASK_SUMMARY_MAX_CHARS`, structured runtime logs, chart/operator env propagation, and docs describing the execution limits and metrics surface.

## 2026-08-28 00:24:39 ACST

- objective: Integrate LiteLLM gateway metering callbacks and audit hooks.
- files changed: `.github/workflows/ci.yml`, `README.md`, `agent-runtime/README.md`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `charts/skquad/README.md`, `charts/skquad/templates/api-server-deployment.yaml`, `charts/skquad/templates/llm-gateway-deployment.yaml`, `charts/skquad/values.yaml`, `control-plane/README.md`, `control-plane/internal/config/config.go`, `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `control-plane/internal/storage/postgres.go`, `docs/agent-runtime.md`, `docs/api-design.md`, `docs/ci-cd.md`, `docs/data-model.md`, `docs/llm-gateway.md`, `docs/observability-metering.md`, `llm-gateway/Dockerfile`, `llm-gateway/README.md`, `llm-gateway/config.yaml`, `llm-gateway/skquad_litellm_callbacks.py`, `WORKLOG.md`.
- command/test run: `go test ./internal/httpapi ./internal/storage` and `go vet ./... && go test ./...` in `control-plane/`; `go vet ./... && go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m py_compile skquad_litellm_callbacks.py` in `llm-gateway/`; gateway metadata/config validation; `podman build -t localhost/skquad-llm-gateway:metering ./llm-gateway`; containerized callback import smoke test; `helm lint charts/skquad`; default Helm render; Kubernetes server-side dry-run against the lab API; `npm ci && NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `git diff --check`.
- result: Added an internal gateway callback token, `POST /api/v1/gateway/metering` ingestion, task-attributed metering rows, best-effort system audit entries for successful usage/failures, runtime LiteLLM metadata propagation, and a LiteLLM CustomLogger callback packaged in the gateway image. The chart wires callback token/control-plane URL into API and gateway pods, docs now describe implemented metering behavior and remaining key-refresh/revocation gaps, and CI now imports the Skquad LiteLLM callback inside the built gateway image.

## 2026-08-28 01:03:00 ACST

- objective: Build the first authenticated Skquad web app shell.
- files changed: `README.md`, `charts/skquad/README.md`, `charts/skquad/templates/web-deployment.yaml`, `charts/skquad/values.yaml`, `docs/ci-cd.md`, `web/.eslintrc.json`, `web/README.md`, `web/package.json`, `web/package-lock.json`, `web/src/app/globals.css`, `web/src/app/layout.tsx`, `web/src/app/page.tsx`, `web/src/lib/api.ts`, `WORKLOG.md`.
- command/test run: `npm run lint` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `helm lint charts/skquad`; default Helm render; Kubernetes server-side dry-run against the lab API; local `next start` on `127.0.0.1:3010`; `curl http://127.0.0.1:3010`; `npm audit --omit=dev --json`.
- result: Replaced the placeholder page with a responsive authenticated application shell, top-level navigation, dev/bearer auth state, configurable browser API base via `NEXT_PUBLIC_SKQUAD_API_BASE_URL`, an API client foundation, `/auth/me` and `/squads` loading, and bounded loading/empty/error states. Added deterministic Next ESLint configuration. Browser screenshot verification was not completed because Playwright was present without an installed browser binary and no system browser was available. Production dependency audit still reports two high-severity findings in the current Next/PostCSS tree; created Kanbunny follow-up `Upgrade web dependencies for Next/PostCSS audit findings`.

## 2026-08-28 00:45:24 ACST

- objective: Implement first-pass squad, agent, and task UI workflows.
- files changed: `README.md`, `docs/ci-cd.md`, `docs/web-app-ux.md`, `web/README.md`, `web/src/app/globals.css`, `web/src/app/page.tsx`, `web/src/lib/api.ts`, `WORKLOG.md`.
- command/test run: `npm run lint` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`.
- result: Added typed browser API mutation helpers and replaced the remaining squad/agent/task placeholders with first-pass operational workflows. The web app can now create squads, update squad mission text, add agents, create/rotate agent identities, view selected agent chat history, enqueue consult messages, create tasks, move tasks across Kanban statuses, reassign tasks, and delete tasks. Docs now distinguish implemented web workflows from remaining registry/grants/metering/audit/admin work.

## 2026-08-28 00:58:00 ACST

- objective: Implement first-pass registry, grants, and admin UI workflows.
- files changed: `README.md`, `docs/ci-cd.md`, `docs/web-app-ux.md`, `web/README.md`, `web/src/app/globals.css`, `web/src/app/page.tsx`, `web/src/lib/api.ts`, `WORKLOG.md`.
- command/test run: `npm run lint` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`.
- result: Added web UI for loading registry catalogs, registering/deprecating LLM providers and generic registry resources, granting/revoking registry resources for the selected agent, creating/revoking squad access grants, and showing platform audit plus metering summary in the admin view. Docs now describe the implemented first-pass registry/grant/admin surfaces and remaining richer UX work.

## 2026-08-28 01:24:40 ACST

- objective: Upgrade web dependencies for Next/PostCSS audit findings.
- files changed: `web/package.json`, `web/package-lock.json`, `web/eslint.config.mjs`, `web/.eslintrc.json`, `web/next-env.d.ts`, `web/tsconfig.json`, `web/README.md`, `docs/implementation-status.md`, `WORKLOG.md`.
- command/test run: `npm ci`; `npm audit --audit-level=moderate`; `npm run lint`; `NEXT_TELEMETRY_DISABLED=1 npm run build`; `git diff --check`.
- result: Upgraded the web app to Next.js 16.3.3 and ESLint 9, replaced the removed `next lint` path with ESLint flat config, accepted Next 16's generated TypeScript defaults, and cleared the Next/PostCSS audit findings.

## 2026-08-28 01:51:36 ACST

- objective: Add task execution leases and durable results.
- files changed: `README.md`, `agent-runtime/README.md`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `control-plane/README.md`, `control-plane/internal/domain/types.go`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/migrations/0001_init.sql`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/storage.go`, `docs/agent-runtime.md`, `docs/api-design.md`, `docs/data-model.md`, `docs/implementation-status.md`, `docs/kanban-task-lifecycle.md`, `docs/testing-strategy.md`, `WORKLOG.md`.
- command/test run: `go test ./...` in `control-plane/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py` in `agent-runtime/`.
- result: Added `task_executions` with worker IDs, active leases, fencing tokens, and terminal result summaries. Runtime claims now receive execution metadata, busy heartbeats can extend the lease, and complete/block calls require the execution fence. Stale completion fences are rejected with conflict. Durable task result summaries are stored atomically with task terminal status before optional best-effort memory persistence.

## 2026-09-02 (Christian/Claude)

- objective: Improve UI — enterprise white/orange theme and squad-centric navigation (Kanbunny card "Improve UI").
- files changed: `web/src/app/globals.css`, `web/src/app/page.tsx`, `web/src/components/TopNav.tsx`, `web/src/components/Sidebar.tsx`, `web/src/components/SquadsSection.tsx`, `web/src/components/RegistrySection.tsx`, `web/src/components/AdminSection.tsx`, `web/src/components/shared.tsx`, `docs/web-app-ux.md`, `WORKLOG.md`.
- command/test run: `npm run lint` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; visual smoke via local dev server (desktop and mobile viewports).
- result: Replaced the teal/dark-sidebar shell with a white/light-grey + orange theme. Added a top navbar with brand, open-task/mode/API-connectivity status pills, and a profile avatar dropdown (identity, role, token form). Main menu is now Squads, Registry (subsections per resource type: LLM Providers, Skills, Tools, APIs, Knowledge Bases, Project Workspaces), and Admin — Admin rendered only for `platform_admin` (dev mode auto-promotes, so visible in DEV). Removed the Squads/Agents count tiles and the Agents/Tasks top-level menus; agents, tasks, and access grants now live as tabs inside the selected squad's detail view, so agents are always created inside a squad. Split the monolithic page into presentational components under `web/src/components/`.

## 2026-09-02 (Christian/Claude) - agent visibility

- objective: Make agent work visible in the web UI without control-plane changes, using data the API already returns.
- files changed: `web/src/app/globals.css`, `web/src/app/page.tsx`, `web/src/components/SquadCockpit.tsx`, `web/src/components/SquadsSection.tsx`, `web/src/components/shared.tsx`, `web/src/lib/api.ts`, `WORKLOG.md`.
- command/test run: `npm run lint` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; scripted headless-Chrome verification against a mock control plane (filter persistence, delivery states, console/network errors, mobile overflow measurement).
- result: Added a squad cockpit on the Overview tab (agent states, work in flight, squad spend, tasks done, plus a recent-activity feed) wired to the previously unused `GET /squads/{id}/metering` and `GET /squads/{id}/audit`. Task cards now show live execution state derived from the existing lease fields (running vs stalled, naming the worker) and mark agent-created tasks. The agents table gained a cost column from the unused `GET /agents/{id}/metering` and status pills that distinguish idle/busy/paused/error. Chat now surfaces message delivery state (attempt counts, retry due/scheduled, expiry, dead-letter reason). The board gained assignee and in-flight filters with per-squad localStorage persistence. No API or control-plane changes.

## 2026-09-02 (Christian/Claude) - PR review fixes

- objective: Address Sherlock's two review findings on the web UI redesign PR.
- files changed: `web/src/app/globals.css`, `web/src/app/page.tsx`, `web/src/components/RegistrySection.tsx`, `WORKLOG.md`.
- command/test run: `npm run lint` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; scripted before/after regression checks against a mock control plane, plus a sweep of all five registry subsections.
- result: The registry create route is now derived from the visible subsection instead of `resourceForm.type`, and the `type` field was removed from the form state entirely so the two can no longer drift; the form also clears when the subsection changes. Reproduced the original defect first (heading "Register Tool" while posting to `/registry/skills`) and confirmed it is gone. The sidebar no longer uses a fixed `top: 56px` sticky offset below 900px, where the top nav wraps to 98px tall and caused a measured 42px overlap; it is now static there, matching desktop, where it was never sticky.

## 2026-09-12 (Christian/Claude) - squad LLM selection

- objective: Take LLM providers out of Resources and make the LLM part of creating a squad, per Ross's feedback.
- files changed: `web/src/app/globals.css`, `web/src/app/page.tsx`, `web/src/components/AdminSection.tsx`, `web/src/components/RegistrySection.tsx`, `web/src/components/Sidebar.tsx`, `web/src/components/SquadsSection.tsx`, `web/src/components/shared.tsx`, `web/src/components/shared.test.ts`, `web/src/lib/api.ts`, `docs/api-design.md`, `docs/web-app-ux.md`, `WORKLOG.md`.
- command/test run: `npm ci`, `npm run lint`, `npx tsc --noEmit`, `npm run test:coverage` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; a 37-check headless-Chrome run against a stateful mock control plane that asserts on the request bodies the UI sends, including an injected grant failure.
- result: LLM Providers is no longer a Resources subsection; provider registration and deprecation moved to Admin, whose catalog now lists each provider's models. Create Squad picks a provider and model (first active provider preselected, deprecated ones hidden), stored in the squad's `operating_model.llm` because the control plane has no squad-level LLM field; the Overview tab can set or change it later, merging so other operating-model keys survive. Add Agent inherits the squad's provider, offers only that provider's models (mirroring the gateway's allowed-model rule), and is blocked with a specific reason when the squad LLM is unset, unknown, deprecated or has no models. Because creating an agent does not write grants, the provider is granted in a second call; a failure there is reported without hiding the new agent. LLM Provider is gone from agent resource grants and existing LLM grants are read-only, so an "Apply squad LLM" action replaces the removed path for agents created earlier: it sets the agent's provider and a served model and swaps its LLM grant while keeping other grants, and tells the user to rotate an existing identity since grant changes reach the gateway only on key re-provisioning. Unit coverage rose to ~94.8% statements. No control-plane changes.
