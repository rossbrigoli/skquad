# skquad-build-service (MIG-8 / S-314)

Governed build path that lets a skquad agent **build and publish a
container image without docker daemon access, without registry
credentials, and without host access**. The agent submits a build
request through the tool-gateway's governed `rest` driver; this
service enforces every policy gate and schedules a **kaniko Job** in
the dedicated `skquad-builds` namespace.

## Delivery shape: HTTP service + kaniko (not MCP)

Chosen deliberately over a "build MCP":

- The platform's `rest` driver already provides per-agent credential
  custody, path ACLs, rate limiting and audit — a build MCP would
  re-implement all of that behind a worse fit (builds are
  submit/poll operations, not tool-call-shaped).
- The service is independently deployable/testable and mirrors the
  established quarantine-service pattern (terminal-service, §6.6):
  privileged capability lives behind a narrow API in its own
  namespace; agents hold nothing.
- MCP remains possible later as a thin adapter over this same API.

## API

All `/v1` routes require `Authorization: Bearer <principal token>`
(verified against SHA-256 hashes; the gateway injects the per-agent
credential from CP custody, so the agent never handles it directly).

| Route | Purpose |
|---|---|
| `POST /v1/builds` | Submit `{repo, revision, dockerfile, destination}` → `202` + build record (includes `resolved_sha`) |
| `GET /v1/builds` | List the caller's builds |
| `GET /v1/builds/{id}` | Fetch one build (own builds only) |
| `GET /healthz`, `GET /readyz` | Probes |

Errors: `400` validation, `401` auth, `403` policy denial,
`404` unknown build/revision, `429` quota, `502` upstream probe
failure.

## Governance pipeline (every gate default-deny, ordered cheapest-first)

1. **Syntax** — repo/revision/dockerfile/destination parse cleanly
   (`internal/buildspec`). Digest-pinned destinations rejected: the
   digest is build *output*, never agent input.
2. **Git host floor** — probes only against `git_hosts_allow`
   (SSRF guard for our own `ls-remote`/`fetch`).
3. **Principal repo allowlist** — glob semantics identical to the
   git driver (`rossbrigoli/*` never matches `evilcorp/repo`).
4. **Live git-grant scope** — the agent's own CP policy
   (`GET /internal/v1/policy?agent=`) must contain a git grant whose
   effective `repos_allow` covers the repo. **Fail-closed**: CP
   unreachable ⇒ deny. TTL-cached (default 60 s).
5. **Destination namespaces** — destination must start with BOTH a
   global `destinations_allow` prefix AND one of the principal's
   prefixes (default target: `docker.io/brigss007/agent-builds/`).
6. **Revision pinning** — the revision is resolved to a **full commit
   SHA before scheduling** (`ls-remote` for named refs; depth-1 fetch
   for full SHAs). The kaniko Job receives only the resolved SHA, so
   a queued Job can never build a different commit than was approved.
7. **Quotas** — per-principal concurrent (authoritative via the Jobs
   API) and hourly (in-process window).
8. **Audit** — every submit/deny/failure emits a structured JSON
   audit event (principal, agent_id, repo, revision, destination,
   decision). Events never contain tokens or registry secrets.

## Kaniko Job hardening

Rendered by `internal/kanikojob` (template lives in code so it is
unit-tested):

- Kaniko pod: `automountServiceAccountToken: false`, SA with **no
  RBAC**, `allowPrivilegeEscalation: false`, all capabilities
  dropped, `restartPolicy: Never`, `backoffLimit: 0` (a retry could
  re-resolve mutable state).
- Registry push credential: pre-provisioned docker-config Secret
  mounted read-only at `/kaniko/.docker`; scoped to the allowlisted
  registry path; agents never see it.
- Digest: `--digest-file=/dev/termination-log` → surfaced via the
  kaniko pod's termination message in `GET /v1/builds/{id}`.
- Namespace `skquad-builds`: PSS `baseline` (kaniko needs root in
  its own container), audit/warn `restricted`.

## Registry pinning note

Default `kaniko_image` is `gcr.io/kaniko-project/executor:v1.24.0`
— the last official upstream release (Google deprecated the project
in 2026). The actively maintained community fork
`ghcr.io/osscontainertools/kaniko` (v1.28.x) is the upgrade path;
switch only after supply-chain review.

## Files

```
cmd/build-service/        main: wiring (config → principals → K8s client → HTTP)
internal/config/        YAML config, strict parse, fail-closed validation
internal/principal/     principals file, constant-time bearer auth
internal/buildspec/     request parsing + pure validation (deny-path tests)
internal/policyclient/  CP /internal/v1/policy client, TTL cache, fail-closed
internal/revision/      git revision → full SHA (injectable git runner)
internal/kanikojob/     kaniko Job template render + status/digest mapping
internal/service/       orchestration: gates → quota → schedule
internal/httpapi/       routes, auth middleware, error mapping
deploy/                 namespace, RBAC, deployment, netpol sketch, examples
```

## Registering with the platform (ops)

1. Create `skquad-builds` + RBAC (`deploy/00`, `deploy/10`).
2. Create SealedSecrets: `skquad-build-service-config`
   (`config.yaml` + `principals.json`) and
   `skquad-build-registry` (docker-config scoped to
   `brigss007/agent-builds`).
3. Deploy (`deploy/20`), apply netpol (`deploy/30`, adjust labels).
4. Register a `rest` resource `build` with the control plane:
   `path_allow: ["/v1/builds", "/v1/builds/*"]`, methods
   `[GET, POST]`, egress class `internal`, per-agent credential =
   the principal token (CP custody).
5. Grant agents the resource; the agent's git grant ceiling must
   cover any repo it wants to build (gate 4 enforces this live).

## Tests

`go test ./...` — deny-path coverage: auth (unknown/wrong/disabled),
validation (all fields), host floor, repo globs, CP scope
(covered/narrow-constraints/no-grant/404/500/mismatch), destination
boundary bypass, revision (missing/ambiguous/unreachable/drift),
quotas (hourly + concurrent), cross-principal visibility, digest
extraction, Job template invariants.
