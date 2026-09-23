# Operator Install and Operations Runbook

This runbook covers the current Skquad Kubernetes install and day-two
operations. It describes implemented behavior only; production hardening items
that are still roadmap work are called out as boundaries.

## Scope

The Helm chart installs the control plane in one namespace, normally
`skquad-system`:

- API server
- web app
- LiteLLM gateway
- operator
- optional PostgreSQL
- `squads.skquad.io` and `agents.skquad.io` CRDs

The control plane API owns domain state in PostgreSQL. It writes Squad and
Agent custom-resource intents through a durable Kubernetes outbox. The operator
then reconciles those CRs into per-squad namespaces, base resources, and agent
Deployments.

## Prerequisites

- Kubernetes cluster access.
- Helm 3.
- Container registry access to the configured Skquad images.
- For non-development installs: pre-created Secrets for PostgreSQL, LiteLLM
  master key, OIDC configuration, and provider API keys.

For the lab cluster:

```bash
export KUBECONFIG=$HOME/projects/k3s-cluster/kubeconfig
```

## Install

Development install with chart-managed PostgreSQL and development auth:

```bash
helm upgrade --install skquad charts/skquad \
  --namespace skquad-system \
  --create-namespace
```

Verify control-plane workloads:

```bash
kubectl --namespace skquad-system get pods
kubectl --namespace skquad-system rollout status deployment/skquad-api-server
kubectl --namespace skquad-system rollout status deployment/skquad-operator
kubectl --namespace skquad-system rollout status deployment/skquad-llm-gateway
kubectl --namespace skquad-system rollout status deployment/skquad-web
```

Port-forward the API:

```bash
kubectl --namespace skquad-system port-forward service/skquad-api-server 8080:80
curl --fail http://127.0.0.1:8080/healthz
```

Default values are suitable for development only. They enable dev auth, create
a development PostgreSQL password, create a development LiteLLM master key, and
leave ingress disabled.

## Production-Oriented Values

Use external Secrets instead of chart-managed credentials:

```yaml
postgres:
  enabled: false
  existingSecret: skquad-postgres
  secretKeys:
    databaseUrl: database-url

apiServer:
  authMode: oidc
  databaseUrlSecret:
    name: skquad-postgres
    key: database-url
  oidc:
    issuer: https://issuer.example.com
    audience: skquad
    # IdP groups mapped to platform_admin (SKQUAD_OIDC_ADMIN_GROUPS).
    # Promotion is one-way: gaining the group promotes, losing it does not demote.
    adminGroups:
      - my-org:platform

llmGateway:
  masterKeySecret:
    name: skquad-litellm-master
    key: master-key
  databaseUrlSecret:
    name: skquad-litellm-db
    key: database-url
  extraEnv:
    - name: LOCAL_LLM_API_KEY
      valueFrom:
        secretKeyRef:
          name: local-llm-api-key
          key: api-key
```

Configure model aliases in `llmGateway.config` and grant providers/resources
through the Skquad API or web admin workflows. Do not put raw provider API keys
in ConfigMaps.

## Break-glass Admin Access

An OIDC-independent `platform_admin` login for recovery when the IdP or group
bindings cannot be trusted (see
[identity-security.md §2.1](identity-security.md)). Disabled by default.

```yaml
breakGlass:
  enabled: true            # ConfigMap toggle — flip without re-sealing anything
  username: breakglass
  tokenTTL: 60m
  maxAttempts: 5
  window: 15m
  allowedCIDRs:
    - 192.168.68.0/24    # LAN
    - 100.64.0.0/10      # Tailscale CGNAT
  secretName: skquad-breakglass   # SealedSecret: password-hash + jwt-key
```

Operating rules:

- **Never use the public hostname.** Requests carrying Cloudflare signals
  (`CF-Ray`/`CF-Connecting-IP`) are refused with 403 regardless of source IP.
  Use the internal address (`http://skquad-v2.lab`) or the Tailscale IP.
- Generate the credential material locally:

  ```bash
  go run ./control-plane/cmd/breakglass-hash   # emits argon2id PHC hash + JWT key
  ```

  Put the hash and key in the SealedSecret (`password-hash`, `jwt-key`); never
  in git. The SealedSecret is `optional: true` so a disabled deploy never
  blocks on it.
- **Changing the SealedSecret does not roll the pod** (the chart renders no
  template fragment for it under GitOps, so the checksum annotation is
  constant). After re-sealing, restart explicitly:

  ```bash
  kubectl --namespace skquad-system rollout restart deployment/skquad-api-server
  ```
- Enabled-but-misconfigured fails fast: the API server panics at startup with
  `break-glass is enabled but misconfigured: ...` until the Secret unseals.
  This is intended — no half-configured admin door.
- Login endpoints: `POST /api/v1/auth/breakglass/login`, status via
  `GET /api/v1/auth/breakglass/status`. Guard order: disabled → 404 ·
  via Cloudflare → 403 · non-allowlisted source → 403 · rate-limited → 429 ·
  bad credentials → 401.
- The break-glass user is a normal `platform_admin` row
  (`oidc_issuer='local'`, `oidc_subject='breakglass'`). Disable the feature
  via the ConfigMap when the crisis is over.

## Upgrade

Render and lint before upgrading:

```bash
helm lint charts/skquad
helm template skquad charts/skquad --namespace skquad-system --include-crds >/tmp/skquad-render.yaml
```

For lab GitOps, update the Skquad Application values in the k3s GitOps repo and
let ArgoCD apply the change. For a direct Helm environment:

```bash
helm upgrade skquad charts/skquad \
  --namespace skquad-system \
  -f values-prod.yaml
```

Watch rollout:

```bash
kubectl --namespace skquad-system rollout status deployment/skquad-api-server
kubectl --namespace skquad-system rollout status deployment/skquad-operator
kubectl --namespace skquad-system rollout status deployment/skquad-llm-gateway
kubectl --namespace skquad-system rollout status deployment/skquad-web
```

The chart currently installs CRDs from `charts/skquad/crds`. Review CRD changes
before upgrade; Helm does not remove CRDs on uninstall and CRD downgrade is not
automatic.

## Namespace Model

- `skquad-system` holds the control plane and all Squad/Agent CRs.
- Each Squad CR names one managed data-plane namespace.
- The operator creates each squad namespace, `skquad-agent` ServiceAccount,
  starter ResourceQuota, default-deny NetworkPolicy, DNS egress policy,
  platform egress policy, and a namespace-local Secret writer RoleBinding for
  the control-plane API server.
- Agent Deployments run in the squad namespace, not in `skquad-system`.

List managed CRs:

```bash
kubectl --namespace skquad-system get squads.skquad.io
kubectl --namespace skquad-system get agents.skquad.io
```

Inspect generated data-plane resources:

```bash
kubectl get ns --selector skquad.io/managed-by=skquad
kubectl --namespace squad-<id> get sa,deploy,netpol,resourcequota,role,rolebinding
```

## Generated Secrets

Agent identity create/rotate writes two Kubernetes Secrets into the squad
namespace:

- runtime credential Secret mounted at `/var/run/skquad/credentials/agent`;
- LiteLLM virtual-key Secret mounted at
  `/var/run/skquad/credentials/llm-gateway`.

The raw credential and virtual key are not stored in PostgreSQL. The control
plane stores the runtime credential verifier hash and the Kubernetes Secret
refs. Secret writes still happen synchronously during identity create/rotate
because the outbox intentionally does not persist raw token material.

The API server no longer receives cluster-wide Secret writer RBAC from the
chart. Instead, each reconciled squad namespace gets a local
`skquad-api-agent-secret-writer` Role and RoleBinding that grants the chart's
API server ServiceAccount only the Secret verbs needed for generated runtime
credential and virtual-key Secrets in that namespace.

Check Secret refs on an Agent CR:

```bash
kubectl --namespace skquad-system get agent <agent-cr-name> -o jsonpath='{.spec.credentialSecret}{"\n"}{.spec.virtualKeySecret}{"\n"}'
```

Check the mounted runtime pod contract without printing secret values:

```bash
kubectl --namespace squad-<id> get deploy <agent-cr-name> \
  -o jsonpath='{.spec.template.spec.volumes[*].secret.secretName}{"\n"}{.spec.template.spec.containers[0].volumeMounts[*].mountPath}{"\n"}'
```

## Database Migrations

When `SKQUAD_DATABASE_URL` is set, each API server replica runs embedded
Postgres migrations during startup. Migration execution is serialized with a
Postgres advisory lock, then each applied SQL file is recorded in
`schema_migrations` with its filename and SHA-256 checksum.

Operational boundaries:

- do not edit an already-applied migration file; add the next numbered file;
- a checksum mismatch means the embedded migration history changed after being
  applied and startup should fail until the history is corrected;
- rollback is a database restore or forward-fix migration, not an automatic down
  migration.

## Ingress

Ingress is disabled by default:

```yaml
ingress:
  enabled: false
```

When enabled, the chart routes `/api` to the API server and `/` to the web app.
Use `ingressRoute.enabled=true` for Traefik-native clusters. Do not expose a
development-auth deployment publicly. For the lab environment, create a
Traefik IngressRoute or chart ingress for the internal `*.lab` host, then map
the public `<service>.rossbrigoli.com` host through Cloudflare Tunnel.

## Troubleshooting

### API Auth

Symptoms:

- every request is treated as the dev admin;
- OIDC requests return 401;
- user cannot access a squad they expect to see;
- register/edit/delete controls are disabled for a user who should be admin
  (the web UI hides/disables them unless `/auth/me` reports
  `role=platform_admin`).

Checks:

```bash
kubectl --namespace skquad-system get deploy skquad-api-server \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SKQUAD_AUTH_MODE")].value}{"\n"}'
kubectl --namespace skquad-system get deploy skquad-api-server \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SKQUAD_OIDC_ADMIN_GROUPS")].value}{"\n"}'
kubectl --namespace skquad-system logs deployment/skquad-api-server --tail=100
```

For the "buttons disabled" case: check the user's role via `/auth/me`, then
confirm the IdP token actually carries a group listed in
`SKQUAD_OIDC_ADMIN_GROUPS` (matching is case-insensitive). Promotion applies
on the next authenticated request — no re-login needed if the session token
already carries the bound group. Remember promotion is one-way: removing the
group config does not demote anyone; demotion is an explicit `SetUserRole`
action. If the OIDC path itself is unusable, use the
[break-glass login](#break-glass-admin-access).

Current boundary: OIDC account identity still needs hardening to use
`issuer + subject` as the stable key. Do not treat mutable email alone as a
production-grade account binding yet.

### Kubernetes Outbox and CR Writer

Symptoms:

- API mutation succeeded but no Squad/Agent CR appears yet;
- Agent status changes lag behind task assignment;
- outbox failures accumulate.

Checks:

```bash
kubectl --namespace skquad-system logs deployment/skquad-api-server --tail=200
kubectl --namespace skquad-system get squads.skquad.io,agents.skquad.io
```

The API accepts domain mutations and queues Kubernetes intents. A temporary
Kubernetes API failure should not make the accepted domain mutation fail, but
Secret writes during identity create/rotate are still synchronous.

### Operator Reconciliation

Symptoms:

- squad namespace is missing;
- agent Deployment is missing or has stale env;
- finalizer is stuck.

Checks:

```bash
kubectl --namespace skquad-system logs deployment/skquad-operator --tail=200
kubectl --namespace skquad-system describe squad <squad-cr-name>
kubectl --namespace skquad-system describe agent <agent-cr-name>
kubectl --namespace squad-<id> describe deploy <agent-cr-name>
```

The operator uses explicit finalizers because Squad and Agent CRs live in
`skquad-system` while managed resources live in other namespaces. Do not remove
finalizers manually unless you have already cleaned up the managed resources.

### Agent Scale-to-Zero

Symptoms:

- agent never wakes for assigned work;
- agent stays running after becoming idle;
- unsupported messages keep an agent active.

Checks:

```bash
kubectl --namespace skquad-system get agent <agent-cr-name> \
  -o jsonpath='{.spec.desiredActive}{"\n"}{.status.idleSince}{"\n"}'
kubectl --namespace squad-<id> get deploy <agent-cr-name> \
  -o jsonpath='{.spec.replicas}{"\n"}'
```

Current boundary: unsupported `delegate`/`handoff` messages are retried and then
dead-lettered unless a specialized handler is installed. Automatic task
materialization for those message types is still a follow-up workflow slice.

### Runtime Readiness

Symptoms:

- agent pod is running but not ready;
- runtime cannot claim tasks;
- model calls fail with unauthorized gateway errors.

Checks:

```bash
kubectl --namespace squad-<id> logs deployment/<agent-cr-name> --tail=200
kubectl --namespace squad-<id> exec deploy/<agent-cr-name> -- printenv | grep '^SKQUAD_' | sort
kubectl --namespace squad-<id> get deploy <agent-cr-name> \
  -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}{"\n"}'
```

Do not print Secret contents in shared logs. Verify only the Secret names and
mount paths unless actively debugging on a private terminal.

## Security and RBAC Notes

- The operator needs cluster-scope access for namespaces and cross-namespace
  managed resources, including namespace-local Roles/RoleBindings that grant
  the API server generated Secret write/delete authority per squad namespace.
- The API server's generated Secret authority is namespace-scoped by the
  operator-created RoleBinding in each reconciled squad namespace.
- Agent pods receive only mounted runtime credentials and LiteLLM virtual keys;
  they do not receive provider API keys directly.
- NetworkPolicy permits DNS and egress to API server / LLM gateway pods in the
  control-plane namespace by pod selector. Registry-derived external egress
  policies are still future work.
- Do not enable public ingress while `apiServer.authMode=dev`.

## Uninstall

For a direct Helm install:

```bash
helm uninstall skquad --namespace skquad-system
```

Helm does not remove CRDs by default. Before deleting CRDs, confirm all managed
Squad and Agent CRs are gone and their finalizers have cleaned up data-plane
resources:

```bash
kubectl --namespace skquad-system get squads.skquad.io,agents.skquad.io
kubectl get ns --selector skquad.io/managed-by=skquad
```

If you used chart-managed PostgreSQL, uninstalling the release can remove the
database workload. Preserve or back up persistent volumes before destructive
cleanup.
