# skquad build-service (MIG-8 / S-314)

A small in-cluster service that lets a skquad agent **build and publish a
container image for the project it is working on without holding any
registry or docker-daemon credentials**. The agent submits a governed
build request; the service validates it and runs a
[kaniko](https://github.com/GoogleContainerTools/kaniko) Job in a
dedicated namespace that builds and pushes using service-held
credentials.

Design lineage: `docs/tool-gateway.md` §6.6 (Terminal-as-a-Service) —
privileged capability lives behind a service with per-call governance;
the sandbox holds nothing stealable. Builds follow the same GitOps
philosophy as the rest of the platform: git-revision-pinned, reproducible,
revertible via `git revert` + rebuild.

## Why a REST service (not a build MCP)

The card allowed either shape. Chosen: **REST service consumed through
the existing tool-gateway `rest` driver**, because:

1. **Repo guidance.** `docs/tool-gateway.md` §11 recommends MCP when an
   official server exists and raw `rest` otherwise. There is no standard
   build MCP; we'd be writing a bespoke MCP for 3 endpoints.
2. **The governance envelope already exists.** The `rest` driver gives
   per-agent credential custody (TG-4c), method/path ceilings, rate
   limits, size caps and gateway-side audit for free. A build MCP would
   re-implement all of that behind `tools/call`.
3. **Typed surface is tiny.** Submit / status / list — plain JSON with
   documented shapes is sufficient; no need for protocol-level tool
   enumeration in v1.
4. **Testability.** The whole API is unit-testable with `httptest`; no
   MCP client harness needed in CI.

A thin MCP façade can be layered over the same service later without
changing anything in this module.

## API

All routes require `Authorization: Bearer <token>` (the per-agent
credential the gateway injects for the registered `rest` resource).

| Route | Purpose |
|---|---|
| `POST /api/v1/builds` | Submit a build. `201` → `{build_id, state:"queued", repo, revision, destination}` |
| `GET /api/v1/builds` | List the caller's builds |
| `GET /api/v1/builds/{id}` | Status: `queued`/`running`/`succeeded`/`failed` + `digest` on success |
| `GET /healthz` | Liveness |

Request body:

```json
{
  "repo": "https://github.com/rossbrigoli/skquad.git",
  "revision": "main",
  "dockerfile": "Dockerfile",
  "destination": "ghcr.io/rossbrigoli/agent-builds/skquad:demo-1",
  "build_args": {"VERSION": "0.1.0"}
}
```

Errors: `{"error":"<code>","reason":"..."}` — format problems `400`,
policy denials/quotas `403`, upstream (CP policy / ls-remote) failures
`502` (fail-closed). Deny codes: `invalid_repo`, `git_host_not_allowed`,
`repo_not_allowed`, `outside_grant_scope`, `policy_unavailable`,
`invalid_revision`, `revision_not_found`, `revision_check_failed`,
`invalid_dockerfile`, `invalid_destination`, `tag_forbidden`,
`destination_not_allowed`, `invalid_build_args`, `quota_concurrent`,
`quota_rate`.

## Governance chain (all default-deny)

Implemented in `internal/govern`, ordered cheapest-first:

1. **Repo canonicalization** — accepted URL forms normalize to
   `host/org/repo`; credentials-in-URL and odd schemes rejected.
2. **Git host floor** (`git_hosts_allow`) — SSRF guard for the probe.
3. **Principal repo allowlist** — glob semantics identical to the git
   driver / `egresspolicy.globCovers`.
4. **Live git-grant-scope check** — the requested repo must also fall
   inside the agent's *actual* git grant (`GET {CP}/internal/v1/policy?agent=`,
   constraints-wins-over-ceiling precedence, TTL cache, fail-closed).
   This enforces "repo allowlist ⊆ agent's git grant scope" at request
   time, not just at provisioning time.
5. **Revision** — branch/tag name or full 40-hex SHA. Named refs are
   verified to exist via a minimal smart-HTTP `ls-remote` (no clone).
   Full SHAs are format-checked; existence is enforced by the build
   itself (kaniko fails → nothing pushed).
6. **Dockerfile path** — relative, no `..` traversal, no absolute paths.
7. **Destination** — explicit `:tag` required; `latest` forbidden;
   must start with a **global** allowlist prefix **and** the principal's
   prefix. Default global prefix `ghcr.io/rossbrigoli/agent-builds/`
   keeps agent output namespaced away from platform images
   (`skquad-*`), so a build can never overwrite a component image.
8. **Build args hygiene** — key regex, ≤20 args, no newlines.
9. **Quotas** — concurrent builds counted authoritatively via the Jobs
   API; hourly rate window in-process (best-effort).

Every submit (allow **and** deny) emits a structured JSON audit event
(`internal/audit`), and accepted builds carry their request metadata as
labels/annotations on the kaniko Job — the durable record lives in
etcd even if the service dies. Terminal results are audited on first
observation.

## Kaniko Job hardening (`internal/runner.BuildJob`)

- `privileged: false`, `allowPrivilegeEscalation: false` (kaniko needs
  root for snapshotting but never the privileged flag)
- `automountServiceAccountToken: false`; kaniko pods run under
  `skquad-kaniko`, an SA with **no RBAC binding**
- `backoffLimit: 0` — one auditable attempt, no silent retries
- `activeDeadlineSeconds` + `ttlSecondsAfterFinished` from config
- Registry push credentials arrive **only** as the mounted
  `agent-builds-registry-creds` docker-config Secret (mode 0400);
  nothing agent-controlled touches them
- Digest returned to the agent from the kaniko termination message
  (`{"imageID":"…@sha256:…"}`, with lenient fallbacks)

## RBAC (chart)

Namespace-scoped Role in `skquad-builds` only:
`batch/jobs: create,get,list,watch,delete` + `pods: get,list,watch`.
No cluster roles, no secret read via API. NetworkPolicy restricts
ingress to the release namespace (tool-gateway).

## Provisioning (platform admin, out-of-band — never in git)

1. **Registry credentials** for the allowlisted destination:
   ```bash
   kubectl -n skquad-builds create secret generic agent-builds-registry-creds \
     --from-file=.dockerconfigjson=$HOME/.docker/config.json \
     --type=kubernetes.io/dockerconfigjson
   ```
   Scope the token to the `agent-builds/` path only where the registry
   supports it (GHCR fine-grained PAT / DockerHub scoped token).
2. **Principals** (`principals.json`): one entry per agent. Token hash
   convention = base64 RawStd sha256 (same as CP `hashCredential`):
   ```bash
   echo -n "$TOKEN" | openssl dgst -sha256 -binary | openssl base64 -A
   ```
   ```json
   {"principals":[{
     "id":"sherlock",
     "agent_id":"48da5ca3-5657-4115-a49e-caa643399683",
     "token_b64sha256":"<hash>",
     "repos_allow":["github.com/rossbrigoli/*"],
     "destinations_allow":["ghcr.io/rossbrigoli/agent-builds/"],
     "active":true
   }]}
   ```
   ```bash
   kubectl -n skquad-builds create secret generic build-service-principals \
     --from-file=principals.json
   ```
3. **Deploy**: `buildService.enabled=true` (+ `cpPolicyUrl` pointing at
   the API server, e.g. `http://<api-server-svc>.skquad-system.svc.cluster.local`).
4. **Register the rest resource** (admin) with a ceiling that mirrors
   the service-side governance:
   ```yaml
   type: rest
   base_url: http://skquad-build-service.skquad-builds.svc.cluster.local:8080
   auth: { kind: bearer, secret_ref: <per-agent credential> }
   ceiling:
     methods: [GET, POST]
     path_allow: ["/api/v1/builds", "/api/v1/builds/**"]
     max_request_bytes: 16384
   ```
5. **Grant** the resource to the agent (squad owner), keeping the
   agent's git grant covering every repo in its `repos_allow`.

## Development

```bash
cd build-service
go build ./... && go vet ./... && go test ./...
```

Tests cover the full deny matrix (govern), credential handling (authn),
policy fail-closed semantics (cppolicy), pkt-line ls-remote (gitcheck),
kaniko Job hardening assertions (runner) and HTTP status mapping
(httpapi) — no cluster required.

## Known limitations / follow-ups

- Full-SHA revisions are format-checked only (ref-tip verification
  would need a fetch); a bogus SHA fails in kaniko without pushing.
- Hourly quota is per-process (resets on restart); concurrent quota is
  cluster-authoritative.
- Terminal-result audit depends on the service observing the Job (GET);
  a watch-based emitter or DB-backed audit trail is a follow-up.
- Kaniko image tag is a default value — pin via
  `buildService.kanikoImage` per environment.
- Live-cluster verification (real kaniko push) not yet executed; CI is
  the gate for this slice.
