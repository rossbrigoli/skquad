# skquad k8s-mcp — scoped Kubernetes/gitops MCP server (MIG-7 / S-313)

A small in-cluster **MCP server** (streamable-HTTP, JSON-RPC 2.0) that gives
skquad agents Kubernetes visibility and *bounded* gitops power — with the
scope enforced in code, not by convention. The kubeconfig never touches an
agent pod: this server holds a least-privilege ServiceAccount; agents reach
it only through the skquad tool-gateway as an `mcp` resource with a
per-agent bearer credential.

## Design principles

1. **Deny-by-default scoping.** Namespace allowlist, ArgoCD app allowlist,
   and a hard resource deny list are enforced at call time. A denied call
   never reaches the Kubernetes API.
2. **No escape hatches.** There is no raw-kubectl tool, no generic
   request/verb/path parameter, no exec/attach/port-forward. The tool
   surface is exactly the eight tools below.
3. **Least-privilege RBAC.** The server's ServiceAccount gets only
   `pods get/list`, `pods/log get`, `events get/list`, `services get/list`
   in the allowlisted namespaces, and `applications.argoproj.io get/list`
   (+ `patch` pinned by `resourceNames` to the allowlisted apps) in the
   ArgoCD namespace. No cluster-admin, no secrets.
4. **Bounded output.** Log tails, event counts and message lengths are
   capped so results cannot flood an agent context window.
5. **Fail-closed startup.** No auth token or no namespace allowlist → the
   server refuses to start. Empty ArgoCD app allowlist → all gitops
   tools deny everything.

## Tool catalog

| Tool | Kind | Args | Notes |
|---|---|---|---|
| `k8s_pods_list` | read | `namespace` (req), `label_selector` | name/phase/ready/restarts/node |
| `k8s_services_list` | read | `namespace` (req) | name/type/clusterIP/ports |
| `k8s_events_list` | read | `namespace` (req), `limit` | default 50, max 200, messages truncated |
| `k8s_pod_describe` | read | `namespace`, `pod` (req) | describe-equivalent: labels, images, conditions, statuses + pod events |
| `k8s_pod_logs` | read | `namespace`, `pod` (req), `container`, `tail_lines` | default 100 lines, hard cap `K8SMCP_MAX_LOG_LINES` (500) |
| `argocd_app_status` | gitops-read | `app` (req) | sync/health/conditions; allowlisted apps only |
| `argocd_app_refresh` | gitops-write | `app` (req) | hard refresh (re-read git); allowlisted apps only |
| `argocd_app_sync` | gitops-write | `app` (req), `revision` | **prune is not permitted** (rejected even if passed as false-positive attempts); allowlisted apps only |

Explicitly **not** provided: secrets/configmaps reads (hard deny list in
the client, on top of RBAC), pod exec/attach/port-forward, any
apply/delete/patch of arbitrary resources, any namespace outside the
allowlist, any ArgoCD app outside the allowlist.

## Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `K8SMCP_ADDR` | `:8081` | listen address |
| `K8SMCP_AUTH_TOKEN_FILE` / `K8SMCP_AUTH_TOKEN` | — (required) | bearer token clients must present (file preferred; mounted secret) |
| `K8SMCP_ALLOWED_NAMESPACES` | — (required) | comma-separated explicit namespaces; RFC-1123 labels only, **no wildcards** |
| `K8SMCP_ARGOCD_NAMESPACE` | `argocd` | namespace of Application CRs |
| `K8SMCP_ARGO_ALLOWED_APPS` | empty | comma-separated app names allowed for status/refresh/sync; empty = deny all |
| `K8SMCP_MAX_LOG_LINES` | `500` | hard cap on log tail |
| `K8SMCP_TIMEOUT_SECONDS` | `30` | upstream API timeout |
| `K8SMCP_API_URL` | `https://kubernetes.default.svc:443` | API server |
| `K8SMCP_CA_PATH` | in-cluster CA | TLS bundle for API server |
| `K8SMCP_SA_TOKEN_PATH` | in-cluster SA token | mounted ServiceAccount token (re-read per request; rotation-safe) |

## Wire protocol

`POST /mcp` — JSON-RPC 2.0 (`initialize`, `tools/list`, `tools/call`),
`Authorization: Bearer <token>` (constant-time compare), plain-JSON
responses; `notifications/*` → 202. Compatible with the skquad
tool-gateway MCP driver (`tool-gateway/internal/drivers/mcp`).
`GET /healthz` — unauthenticated liveness.

## Registration with skquad

Register as an `mcp` resource (registration enumerates the eight tools
via the gateway snapshot; unreachable server ⇒ registration fails):

```json
{
  "type": "mcp",
  "name": "k8s-scoped",
  "endpoint_config": {
    "url": "http://skquad-k8s-mcp.skquad-system.svc.cluster.local:8081/mcp",
    "auth_kind": "bearer"
  },
  "policy_ceiling": {
    "egress_class": "internal",
    "tools_allow": ["k8s_pods_list", "k8s_services_list", "k8s_events_list", "k8s_pod_describe", "k8s_pod_logs", "argocd_app_status"],
    "tools_deny": ["argocd_app_sync", "argocd_app_refresh"]
  }
}
```

The example ceiling above is **read-only**: grant `argocd_app_refresh` /
`argocd_app_sync` per-agent (via the approve-tools flow) only where a
write-capable agent is wanted. The server-side app allowlist is the second
lock: even a fully-granted agent can only touch `K8SMCP_ARGO_ALLOWED_APPS`.

## Deployment

Helm: `k8sMcp.*` values in `charts/skquad` (disabled by default). The
chart renders the Deployment, Service, ServiceAccount, and the scoped
Roles/RoleBindings (per read namespace + the ArgoCD namespace with
`resourceNames`-pinned patch). The bearer token comes from an existing
Secret (`k8sMcp.authTokenSecret`), created out-of-band like other
skquad credentials.

## Development

```bash
go build ./... && go test ./...   # unit tests: scoping + deny paths
```

Tests cover: fail-closed config (no token / no namespaces / wildcards),
deny-list resource guard (secrets/configmaps/exec never hit network),
namespace allowlist, ArgoCD app allowlist (status/refresh/sync),
prune=true rejection, log-tail clamping, MCP wire auth (401), catalog
shape (exactly 8 tools, no escape hatches), unknown-tool RPC errors.
