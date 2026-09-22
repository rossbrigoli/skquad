# WORKLOG

## 2026-09-13 07:50:40 ACST

- objective: Fix the deployed Inbox page/API mismatch and tighten inbox listing semantics.
- files changed: `control-plane/internal/storage/postgres.go`, `control-plane/internal/httpapi/server_inbox_test.go`, `control-plane/internal/storage/postgres_parity_test.go`, `WORKLOG.md`; GitOps deployment override updated separately in `/home/ross/projects/k3s-cluster/apps/app-skquad.yaml`.
- command/test run: live `curl http://skquad.lab/api/v1/inbox` before/after API rollout; `go test ./internal/httpapi -run 'TestInbox' -count=1`; `go test ./internal/storage -run 'TestPostgresStoreInboxListIncludesReadByDefault' -count=1`; `go test ./... -count=1`; `go vet ./...`; `git diff --check`.
- result: Confirmed the original 404 came from the API server still running `sha-8375ebc` while the web UI was on `sha-61ce54c`; rolled the API server to `sha-61ce54c` via GitOps, making `/api/v1/inbox` return 200. Fixed the Postgres inbox query so default `/inbox` includes read messages, `?unread=true` filters unread only, and empty inbox lists encode as `[]` instead of `null`.

## 2026-09-08 14:03:08 ACST

- objective: Add web controls for deleting squads and agents, with backend cleanup for agent runtime resources.
- files changed: `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `web/src/app/page.tsx`, `web/src/components/SquadsSection.tsx`, `web/src/app/globals.css`, `WORKLOG.md`.
- command/test run: `npx tsc --noEmit`, `npx vitest run`, and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `go test ./internal/httpapi ./internal/storage` and `go test ./...` in `control-plane/`; `git diff --check`.
- result: Squad and agent tables now expose destructive delete actions with confirmation prompts. Agent deletion captures identity refs before deleting and removes generated credential/virtual-key Secrets best-effort. Squad deletion captures all agent identity refs, queues Agent CR deletions before the Squad CR delete, removes generated credentials, and tests cover agent/credential cleanup.

## 2026-09-03 21:21:58 ACST

- objective: Fix SKQuad web crash when an empty Postgres board response encodes `tasks` as `null`.
- files changed: `web/src/app/page.tsx`, `control-plane/internal/storage/postgres.go`, `WORKLOG.md`.
- command/test run: `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; `go test ./...` in `control-plane/`; `git diff -- web/src/app/page.tsx control-plane/internal/storage/postgres.go`.
- result: The page header open-task count now treats a null task list as empty, and the Postgres task listing initializes an empty slice so board JSON emits `tasks: []` for empty boards. Web production build and control-plane tests passed.

## 2026-08-31 11:36:13 ACST

- objective: Finish and publish Postgres LISTEN/NOTIFY wake-ups for assigned task and inbox changes.
- files changed: committed source, chart, CI, and docs changes in `af4f075 Add agent work notification waits`; updated `WORKLOG.md` locally only.
- command/test run: `go test ./...` and `go vet ./...` in `control-plane/`; `go test ./...` and `go vet ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `python3 -m unittest discover -s tests/integration`; `helm lint charts/skquad`; default and IngressRoute Helm renders; `NEXT_TELEMETRY_DISABLED=1 npm run build`; `git diff --check`; `git push`; GitHub Actions CI run `33349410809`; Images run `33349410711`; GHCR manifest checks for all five `sha-af4f075` component images.
- result: Pushed `af4f075` to `main`; CI and Images both passed. GHCR published `sha-af4f075` tags for api-server, operator, agent-runtime, llm-gateway, and web. Docker Hub mirrors for `sha-af4f075` were not present, matching the existing likely-missing GitHub Docker Hub secret condition. Kanbunny card `0bc897ae-bc0c-4403-aa67-fa64e1394e67` was already in review and was not modified.

## 2026-08-30 23:25:23 ACST

- objective: Add Postgres LISTEN/NOTIFY wake-ups for assigned task and inbox changes.
- files changed: `control-plane/internal/storage/storage.go`, `control-plane/internal/storage/memory.go`, `control-plane/internal/storage/postgres.go`, `control-plane/internal/storage/migrations/0004_agent_work_notify.sql`, `control-plane/internal/httpapi/server.go`, `control-plane/internal/httpapi/server_test.go`, `control-plane/internal/storage/postgres_test.go`, `agent-runtime/skquad_runtime/runtime.py`, `agent-runtime/tests/test_runtime.py`, `operator/internal/controller/agent_controller.go`, `operator/internal/controller/agent_controller_test.go`, `charts/skquad/values.yaml`, `charts/skquad/README.md`, `agent-runtime/README.md`, `control-plane/README.md`, `docs/agent-runtime.md`, `docs/api-design.md`, `docs/deployment-operator.md`, `docs/adr/0004-message-bus.md`, `docs/implementation-status.md`, `WORKLOG.md`; Kanbunny card `0bc897ae-bc0c-4403-aa67-fa64e1394e67`.
- command/test run: `go test ./...` in `control-plane/`; `go test ./...` in `operator/`; `python3 -m unittest discover -s tests` and `python3 -m py_compile skquad_runtime/*.py tests/*.py` in `agent-runtime/`; `helm lint charts/skquad`; default, provider-configured, and `ingressRoute.enabled=true` Helm renders.
- result: Added an agent-authenticated `/api/v1/agents/me/work/wait` long-poll endpoint, Postgres notification triggers on assigned task and ready inbox message changes, Postgres and in-memory store wait implementations, and runtime loop support that waits after idle iterations before falling back to sleep. Relaxed Helm/operator fallback poll intervals to 30 seconds and updated docs/tests.

## 2026-08-30 21:25:00 ACST

- objective: Add CI render coverage for the Traefik IngressRoute chart path.
- files changed: `.github/workflows/ci.yml`, `WORKLOG.md`; Kanbunny board updated out-of-band.
- command/test run: `helm template skquad charts/skquad --namespace skquad-system` for default values; provider-configured gateway values; and `ingressRoute.enabled=true` lab host values; created Kanbunny card `3c59ad44-b6ff-4290-8507-2c31ae86bedf`; `git diff --check`.
- result: The Helm CI job now renders the optional Traefik IngressRoute template with concrete `/api` and `/` routes, closing the render-coverage gap for the chart path added in commit `b6c5f9b`. Added a retroactive Done card for the original IngressRoute chart work.

## 2026-08-30 21:18:02 ACST

- objective: Close Skquad v1 NFR-5, buyer, deployment, scope, and license decisions.
- files changed: `docs/REQUIREMENTS.md`, `docs/ARCHITECTURE.md`, `docs/security-threat-model.md`, `WORKLOG.md`.
- command/test run: `rg` scan for stale quantified/scope/multi-tenant language; `git diff --check -- docs/REQUIREMENTS.md docs/security-threat-model.md docs/ARCHITECTURE.md`.
- result: Requirements now target about 100 squads with about 7 agents each, sub-1s control-plane HTTP responses under that scale, single-tenant Helm/operator Kubernetes installs for v1, explicit hosted multi-tenant/non-Kubernetes/federation/SLA/per-token-enforcement exclusions, and Apache 2.0 licensing. Architecture and threat-model wording now match the v1 single-tenant posture while preserving load-bearing squad/agent isolation.

## 2026-08-29 01:30:19 ACST

- objective: Add chart-managed Traefik routing for Skquad lab UI/API ingress.
- files changed: `charts/skquad/values.yaml`, `charts/skquad/templates/ingressroute.yaml`, `charts/skquad/README.md`, `docs/operator-runbook.md`, `WORKLOG.md`.
- command/test run: `helm lint charts/skquad`; `helm template skquad charts/skquad --namespace skquad-system --set ingressRoute.enabled=true ... --show-only templates/ingressroute.yaml`; rendered IngressRoute Kubernetes server dry-run; `git commit -m "Add Traefik IngressRoute chart option"`; `git push`.
- result: Added optional `ingressRoute.enabled` chart support so Traefik clusters can route `/api` to the API server and `/` to the web service without relying on untracked live IngressRoute objects. Chart lint, template render, and server dry-run passed. Commit `b6c5f9b` was pushed to `rossbrigoli/skquad`.

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

## 2026-09-12 (Christian/Claude) - create forms as dialogs

- objective: Act on Ross's feedback that create forms should be a button that opens a modal window rather than a permanently visible panel.
- files changed: `web/src/components/Modal.tsx` (new), `web/src/components/SquadsSection.tsx`, `web/src/components/RegistrySection.tsx`, `web/src/components/AdminSection.tsx`, `web/src/components/shared.tsx`, `web/src/components/shared.test.ts`, `web/src/app/page.tsx`, `web/src/app/globals.css`, `docs/web-app-ux.md`, `WORKLOG.md`.
- command/test run: `npm run lint`, `npx tsc --noEmit`, `npm run test:coverage` and `NEXT_TELEMETRY_DISABLED=1 npm run build` in `web/`; a 56-check headless-Chrome run against a mock control plane that can fail or delay the next matching write and switch the user's role.
- result: Seven create forms (squad, agent, task, access grant, agent resource grant, per-type resource, LLM provider) now open from a "+ New ..." or "+ Register ..." button in their list header, in a native `<dialog>` via `showModal()` with no new dependency. A dialog closes only when the save succeeds; errors appear inside it with the input kept, because the handlers now return an error message (via a new `runFormAction`) instead of writing to the page banner hidden behind the dialog. Saving shows a pending state and locks the dialog, and a ref guards against a double submit that React state alone would let through. Escape, Cancel and the close button dismiss it, a backdrop click does not, and drafts survive being dismissed. Focus starts on the first editable field and returns to the trigger, which registers itself through the click event because Safari does not focus a button on click. Below 900px the dialog is a bottom sheet. An agent whose LLM grant fails after creation closes the dialog and is reported in the banner, so a retry cannot duplicate it. With the form columns gone, lists and the task board use the full width. Squad settings and the agent chat composer stay inline; delete confirmations still use `window.confirm`.
## 2026-09-02 23:55 ACST — Fix dead live-execution UI from PR #1
- **Objective:** make the "live agent work" feature actually work (board never carried lease fields) and fix review findings.
- **Files changed:**
  - `control-plane/internal/storage/storage.go` — new `ListBoardTaskExecutions` in `TaskStore`
  - `control-plane/internal/storage/memory.go`, `postgres.go` — implementations (active attempts incl. lapsed leases, board-scoped)
  - `control-plane/internal/httpapi/server.go` — `getBoard` attaches execution state; `attachExecutionState` deliberately omits `fencing_token`
  - `control-plane/internal/httpapi/server_test.go` — `TestBoardExposesActiveExecutionState`
  - `web/src/components/shared.tsx` — treat Go zero timestamp as "no lease"
  - `web/src/components/SquadsSection.tsx` — worker label (`<agent id>:<uuid>`), reset stale persisted assignee filter
  - `web/src/app/page.tsx` — metering/audit fetches gated on the tab that renders them (kills N+1 fan-out)
  - `web/src/components/SquadCockpit.tsx`, `web/src/app/globals.css` — owner-only panels degrade quietly instead of erroring
- **Commands/tests run:** `go vet ./...`; `go test ./...` (green); ad-hoc Postgres 16 + pgvector container test of `ListBoardTaskExecutions` (claim → 1 row, other board → 0, after complete → 0), container and temp test file removed; `npm run lint`; `NEXT_TELEMETRY_DISABLED=1 npm run build`.
- **Result:** lease state now reaches the UI; fencing tokens stay server-side; web lint/build clean.

## 2026-09-03 09:45 ACST — Spec the task execution reaper
- **Objective:** spec out the open item "expired task executions stay `status='active'` forever (no reaper)".
- **Files changed:**
  - `docs/execution-reaper.md` — new spec (v1, proposed)
- **Grounding (read-only):** `task_executions` schema (CHECK already includes `expired` — no migration needed); `ClaimNextTask` lazy per-agent reclaim (`claimReclaimableInProgress`); `HeartbeatTaskExecution`/`CompleteTaskExecution` fencing conditions; `defaultTaskExecutionLease = 2m` in `httpapi/server.go`; agent-runtime heartbeats only at claim/complete (no in-flight heartbeat — identified as hard prerequisite); outbox worker as the background-loop template; `kanban-task-lifecycle.md` already promises lease-based crash recovery.
- **Spec shape:** Part A = runtime in-flight heartbeat thread (new `SKQUAD_HEARTBEAT_INTERVAL_SECONDS`, default 40s); Part B = `ReapExpiredTaskExecutions(ctx, cutoff)` on both stores (two conditional statements in one tx; `NOT EXISTS` guard so a task with a fresh live attempt is not yanked to `todo`), worker loop `RunExecutionReaper` (interval 30s, grace 120s → reap ≈4 min after last heartbeat), idempotent under ≥2 replicas, no new API surface.
- **Result:** spec committed to docs (uncommitted); rollout order Part A → Part B; test plan covers storage parity, fencing races, and a kill -9 integration check.

## 2026-09-03 16:10 ACST — Part A: runtime in-flight lease heartbeat
- **Objective:** implement spec Part A from `docs/execution-reaper.md` — keep the execution lease alive while a task handler runs, so the reaper (Part B) never kills live long-running tasks.
- **Files changed:**
  - `agent-runtime/skquad_runtime/runtime.py` — new `TaskLeaseHeartbeat` context manager (daemon thread, `skquad-lease-heartbeat`, tick = `heartbeat("busy", task)` with execution_id + fencing token; failures logged + retried next tick; bounded join in `__exit__` before terminal calls); `BootstrapConfig.heartbeat_interval_seconds` (env `SKQUAD_HEARTBEAT_INTERVAL_SECONDS`, default 40s); `run_task_once` wraps `handle_task_with_timeout` in the context manager.
  - `agent-runtime/tests/test_runtime.py` — 4 new tests: lease kept alive during long task (≥1 in-flight tick, terminal idle last); heartbeat thread stopped on success/timeout/exception paths; handler survives failing ticks; config default 40.0 + env override. New fakes: `TimestampedHeartbeatClient`, `FailingTickHeartbeatClient`.
  - `agent-runtime/README.md` — documented the in-flight heartbeat behaviour and env var.
- **Commands/tests run:** `python3 -m unittest discover -s tests` → 43 tests OK (2 pre-existing fastapi skips); `python3 -m py_compile` on runtime + test modules.
- **Result:** Part A complete. A live, connected worker now refreshes its 2-minute lease every 40s for the whole duration of the work; two consecutive missed ticks are required before the lease lapses. Ready for Part B (control-plane reaper).

## 2026-09-03 19:55 ACST — Part B: control-plane execution reaper
- **Objective:** implement spec Part B from `docs/execution-reaper.md` — expire lapsed task executions and re-queue stuck tasks so dead workers do not leave work in-progress forever.
- **Files changed:**
  - `control-plane/internal/storage/storage.go` — `ReapExpiredTaskExecutions(ctx, cutoff)` added to `TaskStore`.
  - `control-plane/internal/storage/postgres.go` — implementation: two conditional statements in one tx (expire lapsed `active` attempts; re-queue `in-progress` → `todo` only when no live attempt remains, via `NOT EXISTS` guard).
  - `control-plane/internal/storage/memory.go` — matching implementation under the store lock (reuses `taskHasActiveExecutionLocked`).
  - `control-plane/internal/config/config.go` — `ReaperInterval`/`ReaperGrace` + `envSeconds` helper (`SKQUAD_REAPER_INTERVAL_SECONDS`=30, `SKQUAD_REAPER_GRACE_SECONDS`=120).
  - `control-plane/internal/httpapi/reaper.go` — new `RunExecutionReaper` ticker loop (cutoff = now - grace; log-and-continue on error; idempotent under replicas).
  - `control-plane/cmd/api/main.go` — reaper started unconditionally (dev parity).
  - `control-plane/internal/storage/memory_test.go` — 4 reaper tests (cutoff semantics, idempotency, heartbeat-wins, live-attempt guard, completed-skip) + `mustCreateMemoryTestTask` helper.
  - `control-plane/internal/httpapi/server_test.go` — board-after-reaper shows task re-queued to todo with no execution state; worker loop test (reaps lapsed 1ms lease, stops on ctx cancel).
  - `docs/execution-reaper.md` — status → Implemented + implementation notes (incl. the deliberate cutoff-vs-grace deviation).
  - `docs/implementation-status.md`, `docs/agent-runtime.md` — reaper + in-flight heartbeat noted.
- **Commands/tests run:** `go build ./...`, `go vet ./...`, `go test ./...` (all ok); live pgvector/pg16 container verification of the Postgres SQL (cutoff semantics, idempotency, two-active-attempt guard) — container + temp test removed after; runtime suite still 43 ok.
- **Result:** Part B complete. A dead worker's task is now re-queued to `todo` within lease + grace + interval (≈4.5 min default); fencing invariants preserved; no migration, no new API surface. Both spec parts implemented — ready for commit + GitOps deploy.

## 2026-09-03 22:05 ACST — Coverage baseline measurement (Sherlock)
- **Objective:** report current test coverage for every skquad component.
- **Files changed:** none (measurement only; profiles written to /tmp).
- **Commands run:** `go test ./... -coverpkg=./... -coverprofile=...` + `go tool cover -func`; stdlib `trace` harness for Python (no pytest/coverage on host at the time).
- **Result:** control-plane 49.0% (1762/3594 stmts), operator 72.3% (230/318), agent-runtime ~54% by trace approximation (later measured 83% with coverage.py), llm-gateway no unit tests, web no tests. CI collected no coverage at all. Four Kanbunny cards created (coverage work).

## 2026-09-03 22:20 ACST — Postgres-backed store conformance tests
- **Objective:** exercise the SQL store in CI so lease/fencing/reaper/outbox semantics are actually tested (card 27c02902).
- **Files changed:** `control-plane/internal/storage/postgres_parity_test.go` (new, 9 tests).
- **Command/test run:** local `pgvector/pgvector:pg16` podman container + `SKQUAD_TEST_DATABASE_URL=... go test ./internal/storage/... -run TestPostgresStore -v` → 9 PASS; full `go vet ./... && go test ./...` clean.
- **Result:** covers claim/heartbeat/complete fencing, reaper (expiry, idempotency, heartbeat-wins, live-attempt guard, completed-skip), access-grant evaluation, agent-memory trust + 1536-dim embedding round-trip + semantic ranking, outbox lease/retry/applied, task listing/positions, conflict/not-found mapping. control-plane coverage 49.0% → 57.9%. Tests skip when no DSN is set.

## 2026-09-03 22:30 ACST — Config + OIDC unit tests
- **Objective:** cover security-relevant config validation and OIDC token handling (card 72b95a4a).
- **Files changed:** `control-plane/internal/config/config_test.go`, `control-plane/internal/auth/oidc_test.go` (both new).
- **Command/test run:** `go vet ./... && go test ./internal/config/... ./internal/auth/... -v` → config 7 PASS, auth 4 top-level + 18 subtests PASS.
- **Result:** config defaults/overrides/env coercion (bool, duration, seconds incl. negative→default, zero respected), LiteLLM admin URL fallback, auth-mode and OIDC issuer/audience validation errors. OIDC tested against an in-process fake provider (discovery + JWKS, locally signed RS256 JWTs): happy path claim normalisation, name fallbacks, omitted `email_verified`, and rejection of wrong audience/issuer, expired, not-yet-valid, unverified, missing email, garbage and tampered tokens.

## 2026-09-03 22:40 ACST — Vitest suite for the web logic layer
- **Objective:** give the frontend a test runner plus tests for decision logic (card 949ce478).
- **Files changed:** `web/vitest.config.ts`, `web/src/lib/api.test.ts` (10 tests), `web/src/components/shared.test.ts` (26 tests), `web/package.json` (+vitest, +coverage-v8, test scripts), `web/eslint.config.mjs` (ignore generated coverage output).
- **Command/test run:** `npm test` → 36 passed; `npm run test:coverage` → api.ts 100%, shared.tsx 86.2% (StateNotice untested), thresholds statements/lines/functions 85, branches 90 pass; `npx tsc --noEmit` clean; `npm run lint` clean; `NEXT_TELEMETRY_DISABLED=1 npm run build` succeeded.
- **Result:** API client base-URL handling, header construction (auth omitted when blank, content-type only with body), 204 handling and error-message mapping covered; UI helpers for lease state (incl. Go zero-time), cost/token formatting, relative time and message delivery notes covered.

## 2026-09-03 22:45 ACST — CI coverage collection and ratchet floors
- **Objective:** make CI measure and enforce coverage, and run the Postgres store tests there (cards 5cf415da + 27c02902 CI half).
- **Files changed:** `.github/workflows/ci.yml` (pgvector service for control-plane; `-coverpkg` profiles + gate for both Go jobs; coverage.py + `--fail-under=80` for agent-runtime; `npm run test:coverage` for web; artifacts per component), `scripts/go-coverage-gate.py` (new), `.gitignore` (coverage output).
- **Command/test run:** `python3 -c "yaml.safe_load(...)"` OK; gate verified against real profiles (57.90% control-plane, 72.33% operator) and confirmed to exit 1 below the floor.
- **Result:** floors set at control-plane 55%, operator 70%, agent-runtime 80%, web 85% statements — each a few points under current actuals so regressions fail CI while headroom remains. Pushed as `b2918ba..6d32fa9`; CI run pending verification.

## 2026-09-03 23:1x ACST — GitHub Actions Node 20 deprecation bump (card 9cd93d67)
- **Objective:** Remove Node 20 deprecation warnings from CI by moving all pinned actions to Node 24 runtimes.
- **Files changed:** `.github/workflows/ci.yml`, `.github/workflows/images.yml`
- **Investigation:** Verified `runs.using` for every currently pinned action — all 10 were `node20`. Confirmed latest majors via GitHub releases API and read each major's breaking-change notes.
- **Bumps:** checkout v4→v7, upload-artifact v4→v7, setup-python v5→v7, setup-go v5→v7, setup-node v4→v7, azure/setup-helm v4→v5, docker/build-push-action v6→v7, docker/login-action v3→v4, docker/metadata-action v5→v6, docker/setup-buildx-action v3→v4. All target versions report `using: node24`.
- **Also:** web job `node-version` "20"→"22" (Node 20 is EOL; Next 16.3.3 requires `>=20.9.0`, local dev already on Node 22).
- **Risk review:** checkout v7 blocks fork checkout for `pull_request_target`/`workflow_run` — we only use `pull_request`, unaffected. setup-node v6 "limit automatic caching to npm" — we use `cache: npm`. upload-artifact v5 name-uniqueness — our artifact names are already unique per job. docker/* breaking changes are runtime/ESM plus unused env vars and legacy inputs.
- **Command run:** `python3 -c "import yaml; yaml.safe_load(...)"` on both workflows → YAML OK.
- **Result:** Workflows parse clean; no node20-pinned actions remain.
- **Out of scope / flagged:** `web/Dockerfile` still uses `node:20-alpine` (changes the *shipped* runtime, not just CI) — left for Ross to approve separately.

## 2026-09-04 00:4x ACST — httpapi authz/task-lifecycle coverage + CI toolchain fix (card 6c1dec92)
- **Objective:** Close the largest coverage hole: authorization decisions and task-lifecycle error paths in `control-plane/internal/httpapi/server.go`.
- **Files changed:** `control-plane/internal/httpapi/server_gaps_test.go` (new, 17 tests), `control-plane/internal/httpapi/server.go` (removed dead `upsertAgentCR`, zero callers), `.github/workflows/ci.yml` (control-plane gate 55→62; integration-smoke `go-version-file` → `operator/go.mod`).
- **Commands run:** `go vet ./...`, `go test ./internal/httpapi/`, `go test ./... -coverpkg=./... -coverprofile=...`, `python3 ../scripts/go-coverage-gate.py ... --min 62`.
- **Results:** all green. Local coverage (Postgres-backed tests skipped) 50.98% → 56.05%; CI coverage 60.4% → **65.19%** with floor 62 verified passing.
- **CI breakage found and fixed:** setup-go v7 pins `GOTOOLCHAIN=local`. integration-smoke resolved Go 1.23 from `control-plane/go.mod` but also builds the operator module (`go 1.26.0`), so every operator test died with `go.mod requires go >= 1.26.0 (running go 1.23.12)`. Pointed that job at `operator/go.mod` (the higher requirement).
- **Remaining 0% in server.go:** only `noopCRWriter.{UpsertSquad,DeleteSquad,UpsertAgent,DeleteAgent}` — required to satisfy the CRWriter interface but never invoked on the Server path (the outbox worker owns those writes).

## Verification summary (all four cards)
- CI run 33771973937 on main: all 7 jobs success (Control plane, Operator, LLM gateway, Agent runtime, Web, Helm chart, Integration smoke).
- Coverage floors now: control-plane 62 (actual 65.19), operator 78 (actual 79.87), llm-gateway 90 (actual 100).
- Commits: 3fdba09 (actions node24), 6f7e033 (operator tests), 4482c59 (gateway tests + 2 bug fixes), 080e047 (CI toolchain fix), a1f1a9a (httpapi tests + dead code removal), gateway path-layout fix, gate raise.

## 2026-09-04 10:4x ACST — Fix agent chat: LLM-backed replies + history endpoint + system prompt (card 59f08486)
- **Objective:** Kanbunny `59f08486-f15a-41c6-b929-c092fa54ae5b` — "BUG: The Agent chat is not working. All messages go to pending only and never reach an LLM provider." Root cause: the runtime inbox used `DefaultMessageHandler`, a stub that acks user `consult` chat messages without any LLM call, so no reply was ever produced. Card also asks for agent attributes (Name/Role/System Prompt/LLM Provider) and a real-time chat window.
- **Root cause:** `createAgentChatMessage` (control-plane) creates a pending `consult` message; the runtime acked it via the stub handler. No code path called the LLM gateway for chat, and no reply message was ever created.
- **Files changed:**
  - `control-plane/internal/httpapi/server.go` + route: new `GET /agents/me/messages/history` (full chat history, all statuses, oldest first).
  - `control-plane/internal/domain/types.go`, `storage/postgres.go`, `migrations/0005_agent_system_prompt.sql`, `httpapi/server.go` (create/patch), `kube/cr_writer.go`: new `system_prompt` agent attribute flowing domain → Postgres → API → Agent CR (`systemPrompt`).
  - `operator/internal/api/v1/types.go`, `controller/agent_controller.go`, `charts/skquad/crds/skquad.io_agents.yaml`: `spec.systemPrompt` + `SKQUAD_AGENT_SYSTEM_PROMPT` env.
  - `agent-runtime/skquad_runtime/runtime.py`: new `LLMMessageHandler` (user-authored messages → history-aware LLM call via gateway → agent-authored `reply` posted to own chat history, correlated; agent-authored ping/reply/consult acked without LLM so no reply loop; agent delegate/handoff still fail-for-retry), `chat_system_prompt()` (uses agent system_prompt, falls back to role), `ControlPlaneClient.list_message_history`/`send_chat_reply`, `BootstrapConfig.system_prompt`; `main()` now wires `LLMMessageHandler`.
  - `web/src/app/page.tsx`: chat window polls every 5s (silent refresh) so replies appear without manual refresh; agent form sends `system_prompt`.
  - `web/src/components/SquadsSection.tsx`: System Prompt textarea in Add Agent form.
  - `web/src/lib/api.ts`: `Agent.system_prompt`.
  - Docs: `docs/agent-runtime.md`, `docs/api-design.md`, `docs/data-model.md`, `docs/implementation-status.md`.
- **Tests:** runtime +9 (`LLMMessageHandlerTest`: LLM call args, reply posting, no-loop acks, delegate/handoff failure, history inclusion/exclusion, missing key, empty text, LLM error, system prompt override); control-plane +1 history-endpoint test (history includes delivered msgs, pending queue excludes them); `TestSquadAgentTaskFlow` extended for system_prompt create+patch.
- **Commands run:** `go build ./... && go vet ./... && go test ./... -count=1` (control-plane, operator), `python3 -m unittest tests.test_runtime` (53 tests), `npx tsc --noEmit`, `npx vitest run` (36), `npm run build`, `helm lint charts/skquad`.
- **Results:** all green. Note: existing deployments need the 0005 migration (auto on startup) and CRD refresh via Helm upgrade; agents pick up system_prompt on next control-plane reconcile (outbox upsert).

## 2026-09-08 15:5x ACST — Inbox feature for squad owners (card b9f13149)
- **Objective:** Kanbunny `b9f13149-d6eb-4a6d-85eb-ba426e33f0f5` — "Inbox feature": main-menu Inbox holding only owner-addressed agent notifications (task finished / action required), not all agent chatter.
- **Design:** separate `inbox_messages` table (not the agent message queue). Server emits `task_completed` on task completion and `action_required` on task block; agents can explicitly file `action_required` via new `/agents/me/notify-owner`. Owner reads/marks via `/inbox`, `/inbox/{id}/read`.
- **Files changed:**
  - `control-plane/internal/domain/types.go`: `InboxMessage`, `InboxKind` (`task_completed`/`action_required`), `IsRead()`.
  - `control-plane/internal/storage/migrations/0006_inbox_messages.sql`: new table + user/unread indexes (auto-applied on startup).
  - `control-plane/internal/storage/storage.go`: `InboxStore` interface; implemented in `memory.go` and `postgres.go` (Create/List(unreadOnly,limit)/MarkRead scoped to user).
  - `control-plane/internal/httpapi/server.go`: Store += InboxStore; routes GET `/inbox`, POST `/inbox/{messageID}/read`, POST `/agents/me/notify-owner`; `notifySquadOwner` best-effort emission in complete/block handlers; notify-owner caps message at 2000 chars, forbids forging `task_completed`, audits `inbox.notify_owner`.
  - `web/src/lib/api.ts`: `InboxMessage` type. `web/src/components/Sidebar.tsx`: Inbox nav item with unread badge. `web/src/components/InboxSection.tsx`: list + Mark read. `web/src/app/page.tsx`: inbox state loaded regardless of section (badge), 15s poll, `markInboxRead`. `web/src/app/globals.css`: inbox badges/highlight.
- **Tests:** `server_inbox_test.go` — completion notification + unread filter + idempotent read; block → action_required with summary; notify-owner happy path + blank-message 400.
- **Commands run:** `go build/vet/test ./...` (control-plane), `npx tsc --noEmit`, `npx vitest run`, `npm run build` — all green.
- **Notes:** runtime-side autonomous approval requests (agent LLM deciding to call notify-owner) not wired yet; endpoint + docs ready (`docs/api-design.md` §8a, `docs/data-model.md` inbox_messages). Deployment picks up migration 0006 automatically on next control-plane start.

## 2026-09-08 15:5x ACST — Rename Registry → Resources + read-only for non-admins (card 628f4d20)
- **Objective:** Kanbunny `628f4d20` — UI should call the registry "Resources"; non-admin users get a read-only catalog.
- **Files changed:** `web/src/components/Sidebar.tsx` (nav label Resources), `web/src/app/page.tsx` (section title, pass isAdmin), `web/src/components/RegistrySection.tsx` (isAdmin prop; register forms + Deprecate buttons hidden for non-admins, "(read-only)" catalog titles), `docs/api-design.md` §9 note.
- **Backend:** unchanged — list/get were already readable by any authenticated user; create/update/deprecate already require platform_admin. API routes stay `/registry/*`.
- **Commands run:** `npx tsc --noEmit`, `npx vitest run` (36), `npm run build` — green.

## 2026-09-14 18:45 ACST — Merge PR #2 + minor review corrections
- **Objective:** Merge PR #2 (squad-level LLM + create-form dialogs) and fix the minor review findings that are fixable client-side.
- **Files changed:** `web/src/app/page.tsx`, `web/src/components/SquadsSection.tsx` (fixes), plus merge commit `da34b45`.
- **Fixes:**
  1. Overview: clearing the LLM picker now unsets the squad LLM — removed the `|| squadLLM(selectedSquad)` fallback in `updateSquadSettings` (draft is synced from the selected squad, so empty = explicit unset).
  2. LLMPicker: shows a "No LLM provider" option when not required, so a set LLM can actually be cleared from the UI.
  3. `applySquadLLM`: fetches current grants *before* composing the success message, so the "rotate your identity" notice is judged on the server's live permission list instead of the loaded permissions of whichever agent is selected (fixes review comment #3; also drops the `agentID === selectedAgentID` blind spot).
- **Not fixed (deliberate):** review comment #1 (apply ordering) — swapping PATCH/PUT order does not remove the intermediate-failure hazard because PUT replaces all LLM grants either way; real fix is an atomic server-side endpoint (follow-up). `window.confirm` deletions — agreed future work.
- **Commands run:** `npx tsc --noEmit`, `npm run lint`, `npm run test:coverage` (64 pass, 94.89% stmts / 99.2% branch), `npm run build` — all green pre-push; CI run `34826311698` and Images green on `ca5eca1`.

## 2026-09-14 19:20 ACST — Providers main menu (Kanbunny 40205b63)
- **Objective:** Card asks to move LLM Providers out of Resources into a dedicated "Providers" main menu. PR #2 had moved them into Admin, not a separate menu.
- **Files changed:** `web/src/components/Sidebar.tsx` (add `providers` Section + Providers nav button), `web/src/components/ProvidersSection.tsx` (new: provider table + register modal, extracted from AdminSection), `web/src/components/AdminSection.tsx` (drop provider props/section; keep grid + audit), `web/src/app/page.tsx` (render ProvidersSection, load providers on `providers`, admin-gate route), `docs/web-app-ux.md`, `docs/api-design.md`.
- **Command/test run:** `npx tsc --noEmit`, `npm run lint`, `npm run test` (64 pass), `npm run build` — all green pre-push; CI run `34828219741` + Images `34828219732` green on `ef28ca7`.
- **Result:** Committed `ef28ca7`, pushed. Deployed to cluster via GitOps (`89ef50c` in k3s-cluster) at `sha-ef28ca7`; ArgoCD Synced/Healthy (llm-gateway image pull took 3m31s). Smoke: web 200, /api/v1/squads 200, /api/v1/registry/llm-providers 200. Card → in-review.

## 2026-09-14 19:20 ACST — Inbox 404 verified fixed (Kanbunny b9f13149)
- **Objective:** Ross reported the Inbox page showed a "404 Not found" box (2026-09-12).
- **Command/test run:** `GET /api/v1/inbox` on deployed cluster → 200 `[]` (owner-scoped), UI renders empty-state cleanly.
- **Result:** Root cause was Postgres inbox list semantics, already fixed in `fc68d57` and now deployed at `sha-ef28ca7`. No new code needed. Card → in-review. Known limitation unchanged: agent-runtime does not autonomously call notify-owner yet.

## 2026-09-21 23:5x–24:0x ACST — P0: Gateway virtual-key lifecycle on permission change (Kanbunny 6e2b016e)
- **Objective:** Close the control-loop hole: LiteLLM virtual keys were provisioned but never revoked/rotated when agent LLM permissions changed.
- **Design:** Gateway sync happens BEFORE the permission commit (fail-closed): last LLM grant removed → `/key/delete`; allow-list changed → `/key/update`; gateway failure → 502 and permissions unchanged. Re-grant provisions a fresh key against the existing identity. Agent/squad deletion revokes active keys pre-delete. Provider deprecation re-converges granted agents. Idempotent drift repair via `POST /api/v1/admin/gateway/keys/reconcile` (platform admin).
- **Files changed:**
  - `control-plane/internal/domain/types.go`: `AgentIdentity.GatewayKeyToken` (LiteLLM token = sha256 of key, `json:"-"`) + `GatewayKeyStatus` (none/active/revoked).
  - `control-plane/internal/storage/migrations/0007_gateway_key_lifecycle.sql`: new columns.
  - `control-plane/internal/storage/{storage,memory,postgres}.go`: `SetAgentIdentityGatewayKey`, `ListAllAgents`; rotate resets gateway fields; all identity SELECTs extended.
  - `control-plane/internal/httpapi/litellm_client.go`: `ProvisionAgentKey` now returns (key, token); added `UpdateAgentKey` (`/key/update`), `RevokeAgentKey` (`/key/delete`), shared `postKeyAdmin`.
  - `control-plane/internal/httpapi/server.go`: interface extended (provision/update/revoke); `gatewayModelsFromPerms` refactor; `syncAgentGatewayKey` (idempotent converge); `setAgentPermissions` pre-commit sync with orphan-key cleanup on failure + `gateway_key_record_stale` audit; deleteAgent/deleteSquad revoke-first; `deprecateLLMProvider` → `syncAgentsWithLLMProvider`; `reconcileGatewayKeys` admin endpoint + route; identity create/rotate record token+status.
  - `control-plane/internal/httpapi/server_gateway_key_test.go`: 9 new tests (revoke-on-last-remove, update-on-change, re-provision-on-regrant, gateway-failure-aborts-commit, delete agent/squad revoke, reconcile repairs drift + idempotent, reconcile revokes with no grants, deprecate converges).
  - Docs: `llm-gateway.md` (lifecycle section), `api-design.md` (reconcile endpoint + PUT semantics), `implementation-status.md`, `README.md` (gap removed).
- **Commands/tests:** `go build ./...`, `go vet ./...`, `go test ./... -count=1` — all green (httpapi pkg 70.1%). Local coverage gate reads 56.8% only because Postgres parity tests skip without a DB locally; CI (with postgres service) measured 65.4% at base — CI is authoritative.
- **Rebase note:** local main was 4 commits behind (fork review follow-ups 5b394d8); rebased, WORKLOG conflict resolved to upstream + this entry.
- **Result:** Committed `a97df69`, pushed. CI watch: see below.
- **CI/Deploy (2026-09-21):** CI run 35611939752 green — control-plane coverage 65.60% (floor 62), operator 79.87%. Images run 35611940579 published. GitOps promotion k3s-cluster `08196c0` → ArgoCD Synced/Healthy. Live smoke: healthz 200; `POST /api/v1/admin/gateway/keys/reconcile` → `{"checked":1,"errors":0,"none":1}` (confirms migration 0007 applied + endpoint authz path). Kanbunny 6e2b016e → in-review.

## 2026-09-22 07:4x ACST — S-83 P1: Delegate/handoff message → task materialization (resume + fix)
- **Objective:** Resume last night's WIP (started 00:15, uncommitted). Delegate/handoff messages must materialize a real board task on the target squad; the message becomes the delivered audit record, and task completion/blocking notifies the requesting agent + owner.
- **State found:** Code present but UNCOMMITTED and one failing test — `TestHandoffMaterializesTaskWithExplicitTitle` returned 400 `invalid JSON body`.
- **Root cause:** `messageRequest` had no `title` field and the decoder uses `DisallowUnknownFields()` (server.go:3144), so a handoff body `{title, message}` was rejected before reaching the materializer. Separately, `messagePayload` only stored `{message}`, so `delegatedTaskTitle` could never see an explicit title.
- **Fix (server.go):** (1) added `Title string json:"title"` to `messageRequest`; (2) `messagePayload` now emits `title` alongside `message` when set. `delegatedTaskTitle` already reads `title`→`subject`→first-line→fallback, so explicit-title handoffs now name the task correctly.
- **Files (WIP, still uncommitted):** `domain/types.go` (+OriginMessageID on Task), `httpapi/server.go` (materializeDelegatedTask, delegatedTaskTitle/Description, notifyDelegationResult/Blocked, delegate branch in createCurrentAgentMessage, messageRequest.Title, messagePayload), `storage/{storage,memory,postgres}.go` (origin_message_id plumbing), `storage/migrations/0008_task_origin_message.sql`, `httpapi/server_delegate_test.go` (5 tests), `httpapi/server_test.go` (1 line).
- **Commands/tests:** `go build ./...` OK; `go vet ./...` OK; `go test ./... -count=1` — ALL GREEN (httpapi incl. 5 delegate tests: materialize, completion-notify, blocked-notify, non-delegate-still-queues, cross-squad-grants).
- **NOT yet done:** git commit/push, CI watch, image publish, GitOps promotion (migration 0008 → cluster), board transition. Awaiting Ross's go-ahead.
- **PUSHED/DEPLOYED (2026-09-22 07:5x ACST, on Ross's go-ahead):** commit `2241bd8` → push origin/main. CI `35662526220` green (coverage gate pass). Images `35662526097` published — all 5 comps on ghcr @ `sha-2241bd8` (verified via registry tag list). GitOps: k3s-cluster `e8da14b` bumped app-skquad tags a97df69→2241bd8. apps-of-apps was stale at 08196c0 → hard-refreshed → propagated new values → skquad app Synced/Healthy. All pods rolled to sha-2241bd8, old pods gone. Migration `0008_task_origin_message` in schema_migrations; `tasks.origin_message_id` (text NOT NULL) verified in DB; healthz ok. Kanbunny S-83 → in-review.

## 2026-09-22 08:0x–08:3x ACST — S-84 P1: Git-backed squad workspace (v1) — Phase 1a: control-plane contract
- **Objective:** Start S-84. Ross decision: git repo = shared squad workspace. Build the credential-independent control-plane contract first (needed regardless of SSH-vs-HTTPS).
- **Design:** Workspace = existing `RegistryResource` type `project_workspace` with `manifest.kind=git`, `endpoint`=repo URL, `auth_ref`=opaque credential ref, `manifest.default_branch`. Grants reuse existing `AgentPermission` (no new perm code). Task↔workspace linkage via new columns; runtime reports refs via a new endpoint that re-checks assignee + grant + active.
- **Files changed:**
  - `control-plane/internal/storage/migrations/0009_task_workspace_link.sql`: tasks.workspace_resource_id / workspace_branch / workspace_commit_sha (all default '').
  - `control-plane/internal/domain/types.go`: Task.WorkspaceResourceID/Branch/CommitSHA.
  - `control-plane/internal/storage/{storage,memory,postgres}.go`: TaskStore.SetTaskWorkspace; scanTask + all 9 task SELECT projections extended with the 3 columns.
  - `control-plane/internal/httpapi/server.go`: `validateGitWorkspace` (kind=git + endpoint + default_branch + auth_ref required) called in createRegistryResource; `reportCurrentAgentTaskWorkspace` handler + route `POST /api/v1/agents/me/tasks/{taskID}/workspace` (enforces assignee-only + workspace-granted-and-active, audits task.workspace_linked / task.workspace_link_denied).
  - `control-plane/internal/httpapi/server_workspace_test.go`: registration validation (valid + 4 invalid variants) + linkage (grant→report→persist; ungranted→403; non-assignee→403).
  - `docs/adr/0009-workspace-as-git.md`: decision, contract, branch-per-task/no-auto-merge, credential-ref model, open Q1.
- **Commands/tests:** `go build ./...` OK; `go vet ./...` OK; `go test ./... -count=1` ALL GREEN (httpapi incl. new workspace tests).
- **NOT done (blocked on Ross Q1: SSH vs HTTPS credential):** operator git-credential Secret wiring into agent pods; agent-runtime clone/checkout skquad/<agent>/<task> + commit/push + report refs; agent-runtime.md/data-model.md runtime sections.
- **Commit status:** committing locally; NOT pushed/deployed pending Ross Q2 (whole vertical slice before deploy vs phased).
- **DEPLOYED (2026-09-22 ~11:3x ACST, Ross: Q1=HTTPS token, Q2=push phase 1a):** pushed 702348b → CI FAILED (postgres parity: task SELECT projections returned 13 cols vs scanTask 16 — local MemoryStore tests skipped it). Fixed in bed9591 (extended all 9 projections). Re-verified locally against a live pgvector/pg16 container (storage parity green) before pushing. CI 35678056991 green; Images 35678056998 all 5 comps @ sha-bed9591; k3s-cluster 20ba9de; apps-of-apps hard-refreshed → skquad Synced/Healthy; all pods bed9591; migration 0009 applied (workspace_branch/commit_sha/resource_id present); healthz ok.
- **LESSON:** scanTask column changes MUST be validated against a real Postgres before push — MemoryStore-only local tests give false confidence. Spin `pgvector/pgvector:pg16` (podman, port 5433) + `SKQUAD_TEST_DATABASE_URL` to run parity locally.
- **NEXT (Phase 1b):** operator HTTPS-token Secret wiring + runtime clone/checkout skquad/<agent>/<task>/commit/push/report.

## 2026-09-22 ~11:4x–12:xx ACST — S-84 Phase 1b (part 1): runtime git workspace + operator Secret mount — COMMITTED LOCAL (fe7f7ac), NOT deployed
- **Objective:** Phase 1b (Ross: "go ahead with 1b", HTTPS token). Runtime clone/push + operator Secret wiring.
- **Done + tested:**
  - `agent-runtime/skquad_runtime/git_workspace.py` (new): authed_https_url/clean_url/prepare_workspace/commit_and_push/work_branch_for. Token injected into clone/push URL, scrubbed from .git/config. 12 tests (tests/test_git_workspace.py) vs local bare repo.
  - `agent-runtime/skquad_runtime/workspace.py` (new): find_git_workspace/read_workspace_token/prepare_task_workspace/finalize_task_workspace. Best-effort (never blocks task). 10 tests (tests/test_workspace.py).
  - `agent-runtime/skquad_runtime/runtime.py`: BootstrapConfig.workspace_enabled/workspaces_dir/workspace_base; run_task_once prepares before handler + on success commit/push/report; ControlPlaneClient.report_task_workspace. Full runtime suite 65 pass.
  - `agent-runtime/Dockerfile`: apt-get install git ca-certificates.
  - `operator/internal/api/v1/types.go`: WorkspaceSecret type + AgentSpec.WorkspaceSecrets.
  - `operator/internal/controller/agent_controller.go`: mount each workspace Secret read-only at /var/run/skquad/workspaces/<resourceId>/.
- **NOT done (Phase 1b part 2 — control-plane wiring):** populate Agent CR workspaceSecrets from grants. Needs domain.Agent.WorkspaceSecrets + migration agents.workspace_secrets + storage sync (memory+postgres) + agent SELECT projection updates (SAME bug class as today's CI projection failure — MUST run postgres parity before push) + CR-writer read + tests. Then end-to-end verify + deploy.
- **State:** committed local fe7f7ac; NOT pushed/deployed. Current committed behavior is SAFE-but-INERT (no workspaceSecrets in CR -> no mounts -> no token -> runtime skips workspace).

## 2026-09-22 12:45–13:4x ACST — S-84 Phase 1b (part 2): workspace secrets derived into Agent CR — DEPLOYED
- **Objective:** Complete Phase 1b: populate Agent CR `spec.workspaceSecrets` from live grants so the operator mounts git HTTPS-token Secrets into agent pods (runtime side landed in fe7f7ac).
- **Design change vs earlier plan:** NO `agents.workspace_secrets` column. Secrets are **derived at CR-apply time** by the outbox worker from the agent's `project_workspace` grants + registry resources — grants remain the single source of truth, no sync drift possible.
- **Files changed:**
  - `control-plane/internal/domain/types.go`: `WorkspaceSecret{ResourceID,SecretName}` + `Agent.WorkspaceSecrets` (derived, not persisted).
  - `control-plane/internal/storage/{memory,postgres}.go`: `SetAgentPermissions` now enqueues `upsert_agent` outbox event (memory + tx-level pg); pg gains explicit agent-existence check (ErrNotFound parity with memory).
  - `control-plane/internal/kube/outbox_worker.go`: `workspaceGrantReader` iface + `deriveWorkspaceSecrets` — only active+git+resolvable-auth_ref grants produce entries; stale/inactive/non-git/bad-ref skipped (warn-logged) so one bad grant can't block sync; sorted for determinism.
  - `control-plane/internal/kube/cr_writer.go`: emits `spec.workspaceSecrets[]` ({resourceId,secretName}); omitted when empty.
  - `charts/skquad/crds/skquad.io_agents.yaml`: added `workspaceSecrets` array schema (was pruned by CRD). NOTE: live CRD was already updated by ArgoCD SSA at 03:32Z from this commit.
  - Tests: `storage/permissions_outbox_test.go` (memory + pg parity: grant→outbox event, unknown-agent→ErrNotFound), `kube/outbox_worker_workspace_test.go` (derive: 1 good + deprecated + non-git + bad-ref + stale-grant → only good survives; empty grants → empty), `kube/cr_writer_test.go` (emit + omit).
  - Docs: ADR-0009 credential model finalized (HTTPS token, key `token`, `k8s://ns/name` or bare name) + propagation chain; `agent-runtime.md` new §13 Git Workspace Sync + env vars table (`SKQUAD_WORKSPACE_ENABLED/WORKSPACES_DIR/WORKSPACE_BASE`).
- **Commands/tests:** `go build/vet/test ./...` green; **Postgres parity run against live pgvector/pg16 (podman :5433) green** (caught 2 test-only bugs: uuid-typed resource_id, required registered_by); operator build+tests green; runtime `unittest` 75 pass (2 skip).
- **Ship trail:** skquad `7f854d2` pushed (with fe7f7ac). CI 35683453725 success. Images 35683453696 — all 5 comps @ sha-7f854d2 verified via skopeo (control-plane publishes as `skquad-api-server` — NOT skquad-control-plane). k3s-cluster `2e38f4a`; apps-of-apps hard-refreshed → its revision advanced; skquad Synced/Healthy; all pods rolled to 7f854d2, old bed9591 pods gone; api-server `/healthz` via svc proxy → `{"status":"ok"}`.
- **Remaining for full E2E (needs Ross):** register a real `project_workspace` (kind git) with `auth_ref` → a Secret holding a real git HTTPS token under key `token`, grant to an agent, run a task, confirm branch `skquad/<agent>/<task>` pushed + refs on task card.

## 2026-09-22 14:2x–14:3x ACST — S-84 attribution: per-agent git commit authorship — DEPLOYED
- **Objective:** Ross approved cheap attribution fix: workspace token is per-workspace (shared by granted agents), so commits must carry per-agent identity.
- **Change:** `agent-runtime/skquad_runtime/workspace.py` `prepare_task_workspace` now passes `author_name=skquad/<agent-id[:8]>`, `author_email=<agent-id>@skquad.local` into `prepare_workspace` (params already existed). Runtime-only; no CR/control-plane change.
- **Tests:** new `test_commit_author_is_per_agent` asserts pushed commit author in the bare repo == `skquad/<id8> <id>@skquad.local`. Suite: 76 pass (2 skip).
- **Docs:** agent-runtime.md §13 step 5 (authorship) + ADR-0009 attribution resolution (per-workspace token; per-agent author; broker = future upgrade path).
- **Ship trail:** skquad `9e424a9` | CI 35688824805 ✓ | Images 35688824812 ✓ (agent-runtime digest ca78c0bb…) | k3s `f2fc5a1` | apps-of-apps hard-refreshed → skquad Synced/Healthy | all pods sha-9e424a9 | healthz ok. Agent pods scale-to-zero; new runtime image applies on next agent pod creation.

## 2026-09-22 19:35 ACST — Skquad UI redesign proposal (Paperclip-inspired)

- **Objective:** Review paperclipai/paperclip UI and propose a Skquad UI redesign (Ross: current UI "doesn't work").
- **Files changed:** `docs/UI-REDESIGN-PROPOSAL.md` (new).
- **Commands run:** shallow-cloned paperclip to workspace temp; reviewed README, DESIGN.md, ui/src pages/components; reviewed web/src (page.tsx, Sidebar, SquadCockpit, SquadsSection); attempted live screenshot (blocked by Cloudflare Access — expected).
- **Result:** Diagnosis (CRUD-mirror UI, no routes, modal-centric, WIP invisible, ad-hoc status vocab). Proposal: operator-first stance, two-rail nav, routed entity pages (inbox/squad/task/agent/costs), unified status vocabulary + token layer, 5-phase plan. No new APIs needed until Phase 3 (budget policies).

## 2026-09-22 19:2x–19:25 ACST — UIv2-2: fix CI web-v2 coverage gate (S-89)
- **Objective:** CI 35712532458 failed on web-v2 coverage (format.ts 42% vs 85% gate). Fix by testing, not threshold-lowering.
- **Files changed:** `web-v2/src/lib/format.test.ts` (new, 25 tests: formatCost/formatTokens/leaseState/formatRelativeTime/messageText/messageDeliveryNote; fake clock pinned 2026-09-22T10:00Z; Go zero-time + invalid-date + bucket-boundary cases).
- **Discovered semantic:** formatRelativeTime rounds to whole seconds BEFORE bucketing → 89.5s renders "in 2 minutes" (89s → 1 min). Test encodes actual behavior.
- **Commands/tests:** `npm run test:coverage` → 38 pass, format.ts + status.ts = 100% stmts/branch/funcs/lines.
- **State:** committing test-only change; WORKLOG excluded from commit per rules. Next: push → watch CI green → confirm web-v2 image published → GitOps enable webV2 (needs Ross for Cloudflare tunnel mapping of skquad-v2 if exposing).

## 2026-09-22 19:25–19:5x ACST — UIv2-2: web-v2 shipped to cluster, A/B reachable on .lab (S-89)
- **Objective:** Finish ship pipeline: CI green → image → GitOps enable → verify v2 route.
- **Done:**
  - Fixed CI coverage failure (35712532458): added `web-v2/src/lib/format.test.ts` (25 tests, 100% on format.ts). Commit `bc4d635`.
  - CI 35713067836 ✓ | Images 35713067773 ✓ | `ghcr.io/rossbrigoli/skquad-web-v2:sha-bc4d635` digest `sha256:b967c63b…` verified via skopeo.
  - GitOps: k3s-cluster `13d67a3` — webV2.enabled=true, tag sha-bc4d635, replicas 1, hosts skquad-v2.rossbrigoli.com + skquad-v2.lab (/→webV2, /api→apiServer). apps-of-apps hard-refreshed → skquad Synced/Healthy.
  - Verified: deploy/skquad-web-v2 1/1 Running (bc4d635), rollout ok. `http://skquad-v2.lab/` → 200 Next.js v2 HTML; `/api/v1/squads` → 200 through v2 host; v1 `skquad.lab` unchanged 200.
- **Note:** `/api/healthz` 404 is expected — health is `/healthz` on api-server, not routed via ingress hosts (same as v1).
- **Needs Ross:** Cloudflare Tunnel mapping `skquad-v2.rossbrigoli.com` → `skquad-v2.lab` (per standard expose procedure) for external A/B.
