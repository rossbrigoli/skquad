# Installing skquad

This guide covers deploying skquad with Helm on different Kubernetes flavors:
**k3s / kind, vanilla (kubeadm), EKS, AKS, and OpenShift**. For the
five-minute local path, see [Getting Started](../README.md#getting-started)
in the root README.

The authoritative install surface is
[`charts/skquad/values.yaml`](../charts/skquad/values.yaml) — every knob
mentioned here exists in that file. Component design lives in
[deployment-operator.md](deployment-operator.md) and the
[chart README](../charts/skquad/README.md).

> [!IMPORTANT]
> skquad is an early-stage project (see
> [Current status](../README.md#current-status)). This guide reflects the
> current vertical slice; some production hardening paths (observability,
> HA database, external secrets) are left to your environment standards.

---

## 1. What gets deployed

One Helm release installs the **control plane** into a single namespace
(default `skquad-system`):

| Component | Default replicas | Image (GHCR) | Purpose |
| --- | --- | --- | --- |
| API server | 2 | `skquad-api-server` | REST API, authN, outbox, registry |
| Operator | 1 | `skquad-operator` | Reconciles Squad/Agent CRs → namespaces, deployments, PVCs |
| LLM gateway | 2 | `skquad-llm-gateway` | LiteLLM proxy, agent virtual keys |
| Tool gateway | 2 (HPA 2–8) | `skquad-tool-gateway` | Governed egress plane (web/rest/mcp/git) |
| Web UI | 1 | `skquad-web` | Next.js single-page app |
| Embedder | 1 | `skquad-embedder` | llama.cpp embeddings for memory RAG (GPU auto-detect, CPU fallback) |
| PostgreSQL | 1 | `pgvector/pgvector:pg16` | Bundled dev database (replace for production) |

Optional components (disabled by default):

- **Browser service** (`browserService.*`) — quarantined Playwright pool in
  its own namespace (`skquad-browser`), TG-6.
- **Terminal service** (`terminalService.*`) — governed SSH + step-ca +
  session recordings in `skquad-terminal`, TG-10.

CRDs `squads.skquad.io` and `agents.skquad.io` install from
`charts/skquad/crds` before everything else. Squad and agent resources are
created through the API/UI, **not by hand**.

## 2. Cluster prerequisites

Every flavor needs the same four capabilities:

1. **A default StorageClass** that dynamically provisions PersistentVolumeClaims.
   The chart creates PVCs for:
   - PostgreSQL (`postgres.storage.size`, default `10Gi`);
   - each agent workspace, created by the operator at runtime
     (`apiServer.defaultAgentStorageSize` = `2Gi`, capped by
     `apiServer.maxAgentStorageSize` = `10Gi`; `apiServer.storageClass: ""`
     means "use the cluster default" — never tenant-selectable);
   - step-ca CA storage when `terminalService.enabled` (1 Gi).
2. **A CNI that enforces NetworkPolicy.** This is not optional in practice:
   every agent pod starts with a `netpol-guard` init-container that refuses
   to launch until egress enforcement is *confirmed* (N consecutive blocked
   canary probes). If your CNI does not enforce NetworkPolicy, the canary
   stays reachable and **agent pods fail closed** (init container exits 1
   after `agentNetpolGuard.maxWaitSeconds`, default 30 s). All mainstream
   CNIs (Calico, Cilium, Flannel*, OVN-K) enforce policies — *Flannel
   enforces since v0.14 with `--kube-network-policy`; older Flannel does not.
3. **LLM provider credentials.** The default LiteLLM `model_list` is empty
   (`llmGateway.config`). Before agents can do model-backed work you must
   register at least one AI provider + model with real credentials
   (API keys stored in Kubernetes Secrets). See
   [llm-gateway.md](llm-gateway.md) and the chart README.
4. **Ingress + TLS** for anything beyond a laptop. The chart ships two
   routing options (pick one):
   - `ingress.*` — a standard Kubernetes `Ingress` (`/api` → API server,
     `/` → web), works with any IngressClass;
   - `ingressRoute.*` — a Traefik `IngressRoute` (`entryPoints: [web]`).

### Sizing

Sum of default pod *requests* for the control plane (with bundled Postgres):
≈ **2 vCPU / 3 GiB RAM**, before any squad workload. Practical minimums:

- **Laptop/demo:** one node (or single-node k3s/kind) with 4 vCPU / 8 GB.
- **Team:** 2–3 worker nodes, 4 vCPU / 8 GB each; control-plane components
  spread across nodes (anti-affinity is not chart-enforced — see
  `nodeSelector`/`tolerations` on each component).
- **Agent workloads:** budget per running agent pod on top (they scale to
  zero when idle, `agent.idleTimeout` default 15 min).
- **Embedder GPU (optional):** any node exposing `nvidia.com/gpu`,
  `amd.com/gpu`, or `gpu.intel.com/i915`. No GPU → automatic CPU fallback
  (slower embeddings, still functional). The embedder requests 1 CPU /
  1.5–2 GiB itself.

### Images

Release images are public GHCR images:
`ghcr.io/rossbrigoli/skquad-{api-server,operator,llm-gateway,tool-gateway,agent-runtime,web,embedder,browser-service,browser-proxy,terminal-service}`,
tagged `major.minor.build` (e.g. `0.1.299`) — see
[versioning.md](versioning.md). Override `image.*.repository`/`image.*.tag`
for private mirrors or `sha-<digest>` pinning.

## 3. Generic install (any flavor)

```bash
git clone https://github.com/rossbrigoli/skquad && cd skquad

# Review first:
helm lint charts/skquad

# Dev-grade install (NOT for exposed environments):
helm upgrade --install skquad charts/skquad \
  --namespace skquad-system --create-namespace \
  -f my-values.yaml

kubectl -n skquad-system rollout status deployment/skquad-api-server
kubectl -n skquad-system get pods
```

Verify the API:

```bash
kubectl -n skquad-system port-forward service/skquad-api-server 8080:80
curl --fail http://127.0.0.1:8080/healthz
```

The chart's defaults are **development defaults**: `apiServer.authMode: dev`
(no human authentication; a fixed admin principal), a bundled Postgres with
a well-known password, and a development LiteLLM master key. For any
non-local deployment you must at minimum:

- `apiServer.authMode: oidc` + `apiServer.oidc.{issuer,audience,adminGroups}`
  and `web.oidc.{enabled,clientId,redirectUrl}` (client secret in the
  Secret named by `web.oidc.secretName`, key `client-secret`);
- `postgres.existingSecret` + `apiServer.databaseUrlSecret.name` pointing at
  externally managed credentials (SealedSecret/SOPS/ESO), or bring your own
  PostgreSQL and set `postgres.enabled: false` with
  `apiServer.databaseUrlSecret`;
- `llmGateway.masterKeySecret.name` (don't ship the default master key);
- `toolGateway.internalTokenSecret.name` for the CP↔gateway internal token;
- ingress with TLS.

Without `apiServer.oidc.adminGroups` every OIDC user lands as a plain user
and **nobody can reach the admin UI** — bind your IdP group(s) to
`platform_admin` explicitly.

The sections below cover what changes per flavor.

---

## 4. k3s (and kind)

k3s is the reference environment for this project (development and CI run
on k3s), so chart defaults are pre-matched to it: pod CIDR `10.42.0.0/16`
and service CIDR `10.43.0.0/16` are already the
`toolGateway.clusterCIDRs` defaults.

**Storage:** k3s ships the `local-path` StorageClass — fine for single-node
and dev. For multi-node production-grade persistence use Longhorn,
Ceph-CSI, or an NFS/static provisioner and make it the default class (or
set `apiServer.storageClass` and `postgres.storage` accordingly).

**Ingress:** use the bundled Traefik via the chart's IngressRoute support:

```yaml
ingressRoute:
  enabled: true
  entryPoints: [websecure]   # web for plain HTTP only
```

Point DNS at your Traefik node/LB and terminate TLS with Traefik
certificates (or put cert-manager + Traefik provider in front).

**GPU (optional):** install the NVIDIA device plugin (with the container
toolkit) or AMD's GPU operator so nodes advertise `nvidia.com/gpu` /
`amd.com/gpu`. The embedder binds automatically. For NVIDIA set
`embedder.images.cuda` to the `-cuda` build (the default Vulkan image has
no NVIDIA proprietary ICD and silently CPU-falls back). For AMD hosts add
device mounts:

```yaml
embedder:
  gpu:
    mode: auto
    devicePaths: ["/dev/dri", "/dev/kfd"]
```

**kind (laptop):** default kindnet enforces NetworkPolicy, and the
`standard` StorageClass works out of the box. Create the cluster with a
port mapping if you want ingress instead of port-forward:

```yaml
# kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 80
        hostPort: 8080
```

Then `helm upgrade --install skquad charts/skquad -n skquad-system
--create-namespace` and open `http://localhost:8080` through an Ingress
with `ingress.className: kind`.

## 5. Vanilla Kubernetes (kubeadm)

kubeadm clusters give you no defaults for storage or ingress — you bring both:

1. **StorageClass.** Options: Longhorn, rook-Ceph (`ceph-block` style),
   NFS subdir external provisioner, or local-static. Mark it default so the
   chart's `storageClassName: ""` omission picks it up:

   ```bash
   kubectl patch storageclass <your-class> \
     -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
   ```

2. **Ingress.** ingress-nginx (DaemonSet behind a MetalLB pool or
   hostNetwork) is the common choice:

   ```yaml
   ingress:
     enabled: true
     className: nginx
     hosts:
       - host: skquad.example.com
         paths:
           - { path: /api, pathType: Prefix, service: apiServer }
           - { path: /,    pathType: Prefix, service: web }
     tls:
       - secretName: skquad-tls     # manage via cert-manager + Let's Encrypt
         hosts: [skquad.example.com]
   ```

3. **CIDRs.** kubeadm CNI CIDRs vary (Calico `192.168.0.0/16` +
   `10.96.0.0/12`, Cilium `10.244.0.0/16` + `10.96.0.0/12`, …).
   **Set `toolGateway.clusterCIDRs` to your actual pod+service CIDRs** —
   the chart defaults are k3s values and the gateway's egress fence is only
   correct if these match your cluster.

4. **OIDC.** Any IdP with `.well-known/openid-configuration` (Dex,
   Keycloak, Authservice…) works: set `apiServer.oidc.{issuer,audience}` +
   `adminGroups` (IdP groups → `platform_admin`) and the `web.oidc.*`
   block with a real `redirectUrl`
   (`https://skquad.example.com/...` per your IdP's redirect registration).

5. **GPU (optional):** nvidia device plugin DaemonSet (+ container
   runtime), or AMD GPU operator; same embedder notes as k3s.

## 6. Amazon EKS

- **Storage:** install the **EBS CSI driver** (add-on or helm) and make
  `gp3` the default StorageClass — without the CSI driver the Postgres PVC
  stays `Pending` forever. gp2 won't auto-provision on newer clusters.
- **Ingress:** two supported shapes:
  - **ALB Ingress Controller**: `ingress.className: alb`; the ALB Ingress
    in the chart routes `/api` and `/` to different services — model that
    as the two paths already templated, with an `IngressClass` referencing
    your `AlbConfig`. TLS terminates on the ALB via an **ACM certificate**
    (annotate per the ALB docs); set `web.publicBaseUrl` to the ALB DNS name.
  - **ingress-nginx behind an NLB**: classic, works like the vanilla
    section; pair with cert-manager for TLS.
- **CIDRs:** EKS pod CIDRs come from your VPC/subnet plan (VPC-CNI: pods
  get VPC IPs). Set `toolGateway.clusterCIDRs` to the VPC ranges that
  must be unreachable from governed egress (pods + services + anything
  private). The k3s defaults are wrong here — review explicitly.
- **OIDC:** use your corporate IdP (Entra ID, Okta, Cognito federation).
  EKS's own IAM OIDC provider is for the cluster, not for skquad user
  login — skquad needs a normal OIDC client/issuer pair per §3.
- **GPU:** EKS-optimized NVIDIA AMI or a GPU node pool + nvidia device
  plugin; set `embedder.images.cuda` to the `-cuda` embedder build. AMD
  MI-series: amd gpu operator, `devicePaths: [/dev/dri, /dev/kfd]`.
- **IRSA note:** the control plane itself needs no AWS credentials; only
  your storage/ingress add-ons do.

## 7. Azure AKS

- **Storage:** AKS ships Azure managed-disk CSI by default
  (`managed-premium` / `standardssd-lrs`). Ensure a default StorageClass
  exists; for cheaper dev disks use `managed-csi` standard SKU. Note
  minimum disk size is 32 GiB on managed disks — bump
  `postgres.storage.size` accordingly or use Azure Files NFS for small
  non-prod installs.
- **Ingress:** AGIC (Application Gateway Ingress Connector) or
  ingress-nginx behind a Standard LB. With AGIC, path rules
  `/api` → `skquad-api-server`, `/` → `skquad-web` map onto your
  Application Gateway's routing rules; TLS on the App Gateway (Key Vault
  cert). With ingress-nginx + cert-manager, same as vanilla.
- **OIDC:** Entra ID works as the user IdP: issuer
  `https://login.microsoftonline.com/<tenant>/v2.0`, client id/secret for
  the `skquad` app registration, `adminGroups` = Entra object IDs of your
  admin groups (Entra puts object IDs in the `groups` claim).
- **CIDRs:** AKS pod/service CIDRs depend on your kubenet vs Azure-CNI
  plan — set `toolGateway.clusterCIDRs` to match.
- **GPU:** AKS GPU node pools (NC/ND series) with the nvidia device
  plugin addon; set `embedder.images.cuda`.

## 8. OpenShift 4

OpenShift works, but its security model needs explicit attention:

- **SCC / UIDs.** The chart runs everything `runAsNonRoot` with
  `readOnlyRootFilesystem` and seccomp `RuntimeDefault`, which satisfies
  the `restricted` SCC **except** that two containers pin fixed UIDs
  (`web` = 65532, `embedder` = 1000). Under `restricted`, a fixed
  `runAsUser` must fall inside the namespace's allocated UID range or the
  pod is rejected. Options:
  1. set the namespace range to include those UIDs, or
  2. override `web.podSecurityContext.runAsUser` /
     `embedder.securityContext.runAsUser` to UIDs inside your namespace's
     range (`oc describe namespace skquad-system | grep openshift.io/sa.scc`), or
  3. grant `anyuid` SCC to the service accounts (weakest — dev only).
  TODO: verify per-release UID-range interplay against your cluster; the
  chart does not currently template OpenShift-specific SCC objects.
- **Routes/Ingress.** OpenShift's ingress operator reconciles standard
  `Ingress` objects into Routes. `ingress.enabled: true` with
  `className: openshift-default` (or omit for the cluster default class)
  and `tls:` with a Secret (cert-manager `openshift` route reconciler or
  manual). Edge termination on the router; set `web.publicBaseUrl` to the
  route hostname so redirects match.
- **CIDRs.** OpenShift defaults: pod network `10.128.0.0/14`, services
  `172.30.0.0/16` (verify with `oc get network-attachment-definitions`
  / `oc get clusternetwork`). Set `toolGateway.clusterCIDRs` accordingly
  — the k3s defaults are wrong for OpenShift.
- **Storage.** Use ODF/Ceph block, NFS-NFS CSI, or local-block as the
  default StorageClass. The bundled Postgres PVC works unchanged once a
  default class exists.
- **OIDC.** OpenShift itself can be the IdP: use the cluster OAuth server
  issuer (`https://api.<cluster>:6443`, client `openshift`-style
  registration or a dedicated `skquad` OAuthClient) with groups claims
  mapped into `apiServer.oidc.adminGroups` (e.g. an OpenShift group).
  Alternatively bring Keycloak/Entra as with other flavors.
- **GPU.** The OpenShift GPU Operator (NVIDIA) or AMD GPU operator
  advertise resources; the embedder's `/dev/dri`/`/dev/kfd` device
  mounts may require the GPU operator's security profiles or a tailored
  SCC — TODO: verify on your cluster. CPU fallback always works.
- **NetworkPolicy.** OVN-K enforces policies, so the agent `netpol-guard`
  confirms normally.

## 9. After install

1. **Health:** `curl /healthz` on the API (port-forward or via ingress).
2. **Log in.** Dev mode: the UI works with the fixed dev admin principal.
   OIDC mode: `web.oidc.enabled: true` gives authorization-code + PKCE
   login.
3. **Register an AI provider** (Settings → AI Providers) with a provider
   API key so agents get model access; the gateway provisions
   agent-scoped virtual keys from there.
4. **Create a squad + agent + task** in the UI and watch the operator
   materialize the squad namespace and agent deployment
   (`kubectl get squads,agents -A`).
5. **Verify governed egress** if you enabled the tool gateway: agent pods
   should start only after `netpol-guard` confirms enforcement — check
   `kubectl logs <agent-pod> -c netpol-guard`.

## 10. Upgrades, rollback, uninstall

- **Upgrade:** bump image tags (semantic release tags from the
  [releases workflow](ci-cd.md)) and re-run
  `helm upgrade --install`. If `embedder.backfillOnUpgrade=true`, a
  post-upgrade Job re-embeds memory rows whose model changed
  (idempotent).
- **Rollback:** `helm rollback skquad <revision> -n skquad-system`.
  Database migrations run forward-only at API startup — don't roll the
  chart back across incompatible schema revisions without checking
  [data-model.md](data-model.md) migrations.
- **Uninstall:** `helm uninstall skquad -n skquad-system` removes the
  control plane; CRDs and per-squad namespaces' data persist by design.
  Remove CRDs (`kubectl delete -f charts/skquad/crds`) only when you're
  done with all squads/agents.

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Agent pods CrashLoop, `netpol-guard: canary REACHED` | CNI doesn't enforce NetworkPolicy (or egress to the canary is unexpectedly allowed) | Use a policy-enforcing CNI; or point `agentNetpolGuard.canaryUrl` at a destination that is truly unreachable from your agent network |
| Postgres PVC `Pending` | No default StorageClass / missing CSI driver | Install + default the flavor's storage class (§4–§8) |
| Admin UI unreachable after OIDC install | `apiServer.oidc.adminGroups` not set — everyone lands as plain user | Bind your IdP admin group(s) |
| Login redirect mismatch | `web.oidc.redirectUrl` / `web.publicBaseUrl` don't match IdP registration | Align both with the public origin |
| Embedder `Pending` with `gpu: force` | No node advertises the GPU resource | `mode: auto` (CPU fallback) or add a GPU node |
| Tool gateway 502s / wrong fence | `toolGateway.clusterCIDRs` don't match the cluster's real pod/service CIDRs | Set them to your cluster's ranges |
| Web image optimizer EACCES | `fsGroup` override removed | Keep `web.podSecurityContext.fsGroup: 65532` |

---

See also: [identity & security design](identity-security.md) ·
[operator runbook](operator-runbook.md) ·
[agent storage ops](agent-storage-ops.md) ·
[chart README](../charts/skquad/README.md)
