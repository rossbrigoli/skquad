#!/usr/bin/env bash
# bypass_suite.sh — TG-9 (S-255) network-bypass assertions.
#
# Asserts that from an AGENT network position and from the BROWSER pod:
#   * direct internet egress fails (agents: no internet at all;
#     browser: only via its localhost:8888 SSRF proxy)
#   * direct egress to the lab host (192.168.68.131) fails
#   * direct egress to link-local metadata (169.254.169.254) fails
#   * the browser pod cannot reach the Kubernetes API
#   * unauthenticated / wrong-token calls to the tool-gateway dispatch
#     endpoints return 401
#
# Creates ONE throwaway pod (skquad-drill-agent-*) inside the squad namespace
# so it inherits the real agent NetworkPolicies (default-deny + DNS +
# platform-egress). Deleted on exit. Read-only otherwise.
#
# Exit: 0 only if every check PASSes. Any FAIL = policy violation (system
# finding) and is reported loudly, not hidden.

set -u -o pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${DIR}/lib.sh"
require kubectl jq

SUITE="bypass"
AGENT_POD="${DRILL_PREFIX}-agent-${DRILL_TS}"
GUARD_POD="${DRILL_PREFIX}-guard-${DRILL_TS}"
SKQUAD_SYS_NS="${SKQUAD_SYS_NS:-skquad-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-skquad-operator}"
CLEANED=0

cleanup() {
  [[ "$CLEANED" -eq 1 ]] && return
  CLEANED=1
  log "cleanup: deleting throwaway pods ${AGENT_POD} ${GUARD_POD} in ${SQUAD_NS}"
  kubectl -n "$SQUAD_NS" delete pod "$AGENT_POD" "$GUARD_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# --- preflight ---------------------------------------------------------------
if ! kubectl get ns "$SQUAD_NS" >/dev/null 2>&1; then
  err "squad namespace ${SQUAD_NS} not found (set SQUAD_NS=…)"; exit 2
fi
BROWSER_POD="$(find_browser_pod || true)"
if [[ -z "$BROWSER_POD" ]]; then
  err "no Running browser-service pod found in ${BROWSER_NS}"; exit 2
fi
log "agent-position namespace: ${SQUAD_NS}; browser pod: ${BROWSER_POD}"

# --- create throwaway agent-position pod --------------------------------------
# automountServiceAccountToken:false mirrors the hardened agent runtime posture.
log "creating throwaway pod ${AGENT_POD} (image ${CURL_IMAGE}) in ${SQUAD_NS}"
kubectl -n "$SQUAD_NS" run "$AGENT_POD" \
  --image="$CURL_IMAGE" \
  --overrides='{"spec":{"containers":[{"name":"'"$AGENT_POD"'","image":"'"$CURL_IMAGE"'","command":["sleep","1800"],"resources":{"requests":{"cpu":"10m","memory":"32Mi"},"limits":{"cpu":"200m","memory":"128Mi"}}}]}}' >/dev/null
kubectl -n "$SQUAD_NS" patch pod "$AGENT_POD" -p '{"spec":{"automountServiceAccountToken":false}}' >/dev/null 2>&1 || true

log "waiting for pod Ready (120s max)"
if ! kubectl -n "$SQUAD_NS" wait --for=condition=Ready "pod/${AGENT_POD}" --timeout=120s >/dev/null 2>&1; then
  err "throwaway pod never became Ready"; kubectl -n "$SQUAD_NS" describe pod "$AGENT_POD" | tail -15; exit 2
fi

# --- 0. startup-race probe: is there a window where netpol is not yet applied? --
# NetworkPolicy enforcement is programmed asynchronously after pod creation.
# If ANY probe in the first seconds reaches the internet, a freshly-scheduled
# malicious agent pod has an unrestricted-egress window. Probes start the
# moment exec is available.
log "startup-race: probing internet reachability every 1s for 15s from pod birth"
RACE_HITS=0
RACE_FIRST_BLOCK=""
for t in $(seq 1 15); do
  R=$(curl_in_pod "$SQUAD_NS" "$AGENT_POD" "" -- --max-time 2 "$INTERNET_URL") || true
  RC="${R##* }"
  if [[ "$RC" == "0" ]]; then
    RACE_HITS=$((RACE_HITS + 1))
  elif [[ -z "$RACE_FIRST_BLOCK" ]]; then
    RACE_FIRST_BLOCK="probe #${t} @ $(date -u +%H:%M:%S.%3N)"
  fi
  sleep 1
done
if [[ "$RACE_HITS" -gt 0 ]]; then
  record "$SUITE" "raw-startup-race-nonagent" INFO "${RACE_HITS}/15 raw startup probes REACHED internet (first block: ${RACE_FIRST_BLOCK:-never}) — INFORMATIONAL: this bare drill pod has NO netpol-guard. Real agent pods are gated by the TG-9 slice B blocking init-container (see guard-birth check below); an unguarded pod reaching the internet at birth no longer implies agent exposure."
else
  record "$SUITE" "raw-startup-race-nonagent" INFO "no unrestricted startup window observed this run for the bare (unguarded) drill pod"
fi

# --- 0b. guard-birth check: TG-9 slice B netpol-guard on agent pods ---------
# Real agent pods get a blocking `netpol-guard` init-container built by the
# operator (operator/internal/controller/agent_controller.go). This check
# creates a MIMIC pod in the same squad namespace: the same guard script and
# the SAME parameters currently deployed on the operator (read from its env,
# chart defaults as fallback), with a plain sleep app container behind it.
# Assertions:
#   * the pod reaches Ready (the guard confirmed enforcement — it never
#     lets the workload start unconfirmed),
#   * the guard log carries the CONFIRMED line,
#   * the FIRST probe from the app container (workload birth) is BLOCKED
#     => zero pre-block internet hits with the guard in place.
log "guard-birth: reading deployed guard params from ${OPERATOR_DEPLOY}"
guard_env() { # guard_env <SKQUAD_ENV_NAME> <fallback>
  local v
  v=$(kubectl -n "$SKQUAD_SYS_NS" get deploy "$OPERATOR_DEPLOY" -o json 2>/dev/null \
    | jq -r --arg n "$1" '[.spec.template.spec.containers[].env[] | select(.name==$n) | .value] | last // empty')
  [[ -n "$v" ]] && printf '%s' "$v" || printf '%s' "$2"
}
G_CANARY="$(guard_env SKQUAD_NETPOL_GUARD_CANARY_URL https://example.com)"
G_INTERVAL_MS="$(guard_env SKQUAD_NETPOL_GUARD_PROBE_INTERVAL_MS 250)"
G_MAX_WAIT="$(guard_env SKQUAD_NETPOL_GUARD_MAX_WAIT_SECONDS 30)"
G_REQUIRED="$(guard_env SKQUAD_NETPOL_GUARD_REQUIRED_BLOCKED_PROBES 3)"
G_IMAGE="$(guard_env SKQUAD_NETPOL_GUARD_IMAGE curlimages/curl:8.10.1)"
G_ENABLED="$(guard_env SKQUAD_NETPOL_GUARD_ENABLED true)"

# Canonical script lives in agent_controller.go (netpolGuardScript).
# This copy must stay in sync (TG-9 slice B).
G_SCRIPT='set -u
canary="$NETPOL_GUARD_CANARY_URL"
required="$NETPOL_GUARD_REQUIRED_BLOCKED_PROBES"
max_wait="$NETPOL_GUARD_MAX_WAIT_SECONDS"
interval="$NETPOL_GUARD_PROBE_INTERVAL_SECONDS"
deadline=$(( $(date +%s) + max_wait ))
blocked=0
echo "netpol-guard: awaiting egress enforcement (need $required consecutive blocked probes, max ${max_wait}s)"
while [ "$(date +%s)" -lt "$deadline" ]; do
  if curl -s -o /dev/null --max-time 2 "$canary"; then
    blocked=0
    echo "netpol-guard: canary REACHED (full HTTP response) — enforcement not active, counter reset"
  else
    rc=$?
    blocked=$((blocked + 1))
    echo "netpol-guard: canary blocked (curl exit=$rc) consecutive=$blocked/$required"
    if [ "$blocked" -ge "$required" ]; then
      echo "netpol-guard: egress enforcement CONFIRMED ($required consecutive blocked probes)"
      exit 0
    fi
  fi
  sleep "$interval"
done
echo "netpol-guard: FAIL-CLOSED — enforcement not confirmed within ${max_wait}s"
exit 1'

if [[ "$G_ENABLED" != "true" ]]; then
  record "$SUITE" "agent-guard-birth" FAIL "operator reports guard DISABLED (SKQUAD_NETPOL_GUARD_ENABLED=$G_ENABLED) — agent pods start without enforcement confirmation"
else
  G_INTERVAL_S=$(awk -v ms="$G_INTERVAL_MS" 'BEGIN{printf "%.3f", ms/1000}')
  log "guard-birth: creating guarded mimic pod ${GUARD_POD} (canary=${G_CANARY} interval=${G_INTERVAL_S}s max=${G_MAX_WAIT}s need=${G_REQUIRED})"
  jq -n --arg pod "$GUARD_POD" --arg img "$G_IMAGE" --arg script "$G_SCRIPT" \
        --arg canary "$G_CANARY" --arg interval "$G_INTERVAL_S" \
        --arg maxwait "$G_MAX_WAIT" --arg required "$G_REQUIRED" '
    {apiVersion:"v1", kind:"Pod",
     metadata:{name:$pod},
     spec:{automountServiceAccountToken:false,
       initContainers:[{name:"netpol-guard", image:$img,
         command:["/bin/sh","-c",$script],
         env:[{name:"NETPOL_GUARD_CANARY_URL", value:$canary},
              {name:"NETPOL_GUARD_PROBE_INTERVAL_SECONDS", value:$interval},
              {name:"NETPOL_GUARD_MAX_WAIT_SECONDS", value:$maxwait},
              {name:"NETPOL_GUARD_REQUIRED_BLOCKED_PROBES", value:$required}],
         resources:{requests:{cpu:"10m",memory:"16Mi"},limits:{cpu:"100m",memory:"64Mi"}},
         securityContext:{runAsNonRoot:true, runAsUser:1000, runAsGroup:1000, allowPrivilegeEscalation:false, readOnlyRootFilesystem:true,
                        capabilities:{drop:["ALL"]}, seccompProfile:{type:"RuntimeDefault"}}}],
       containers:[{name:"app", image:$img, command:["sleep","600"],
         resources:{requests:{cpu:"10m",memory:"32Mi"},limits:{cpu:"200m",memory:"128Mi"}},
         securityContext:{runAsNonRoot:true, runAsUser:1000, runAsGroup:1000, allowPrivilegeEscalation:false,
                        capabilities:{drop:["ALL"]}, seccompProfile:{type:"RuntimeDefault"}}}]}}' \
    | kubectl -n "$SQUAD_NS" apply -f - >/dev/null

  if ! kubectl -n "$SQUAD_NS" wait --for=condition=Ready "pod/${GUARD_POD}" --timeout=90s >/dev/null 2>&1; then
    G_PHASE=$(kubectl -n "$SQUAD_NS" get pod "$GUARD_POD" -o jsonpath='{.status.phase} {.status.initContainerStatuses[0].state}' 2>/dev/null || echo unknown)
    record "$SUITE" "agent-guard-birth" FAIL "guarded mimic pod never became Ready (state: ${G_PHASE}) — guard fail-closed without confirmation (or cluster/image issue); check logs -c netpol-guard"
  else
    G_LOG=$(kubectl -n "$SQUAD_NS" logs "$GUARD_POD" -c netpol-guard 2>/dev/null || true)
    FIRST=$(curl_in_pod "$SQUAD_NS" "$GUARD_POD" "app" -- --max-time 2 "$INTERNET_URL")
    FIRST_RC="${FIRST##* }"
    if grep -q "CONFIRMED" <<<"$G_LOG" && [[ "$FIRST_RC" != "0" ]]; then
      record "$SUITE" "agent-guard-birth" PASS "guard confirmed enforcement before workload start; first app-container probe blocked (curl exit=$FIRST_RC) — 0 pre-block internet hits"
    elif ! grep -q "CONFIRMED" <<<"$G_LOG"; then
      record "$SUITE" "agent-guard-birth" FAIL "guard container Ready but log lacks CONFIRMED line — guard did not run as expected"
    else
      record "$SUITE" "agent-guard-birth" FAIL "first app-container probe REACHED internet (http=${FIRST%% *}) despite guard confirmation — enforcement regression"
    fi
  fi
fi

# Let netpol programming settle before steady-state assertions.
sleep 10

# --- 1. agent position: direct egress must FAIL -------------------------------
expect_blocked "$SUITE" "agent->internet"        "$SQUAD_NS" "$AGENT_POD" "" "$INTERNET_URL" 8
expect_blocked "$SUITE" "agent->lab-host"       "$SQUAD_NS" "$AGENT_POD" "" "http://${LAB_IP}/" 8
expect_blocked "$SUITE" "agent->metadata-ip"    "$SQUAD_NS" "$AGENT_POD" "" "http://${METADATA_IP}/" 5

# --- 2. browser pod: direct egress must FAIL (proxy is the only path) --------
# NOTE: browser-proxy enforces SSRF/denylist policy at 127.0.0.1:8888. A raw
# curl from the pod that skips the proxy must be blocked by NetworkPolicy,
# otherwise the proxy (and its SSRF floor) is bypassable.
expect_blocked "$SUITE" "browser->internet-direct"  "$BROWSER_NS" "$BROWSER_POD" "browser-service" "$INTERNET_URL" 8
expect_blocked "$SUITE" "browser->lab-host-direct"  "$BROWSER_NS" "$BROWSER_POD" "browser-service" "http://${LAB_IP}/" 8
expect_blocked "$SUITE" "browser->metadata-direct"  "$BROWSER_NS" "$BROWSER_POD" "browser-service" "http://${METADATA_IP}/" 5
expect_blocked "$SUITE" "browser->k8s-api"         "$BROWSER_NS" "$BROWSER_POD" "browser-service" "https://kubernetes.default.svc:443/version" 8

# Sanity counter-check: the proxy path itself must WORK (proves the pod has
# legitimate egress and we are not getting all-blocked false positives).
PROXY_RES=$(curl_in_pod "$BROWSER_NS" "$BROWSER_POD" "browser-service" -- \
  --max-time 15 -x http://127.0.0.1:8888 "$INTERNET_URL")
PROXY_RC="${PROXY_RES##* }"
if [[ "$PROXY_RC" == "0" ]]; then
  record "$SUITE" "browser->proxy-allows-internet" PASS "curl via 127.0.0.1:8888 http=${PROXY_RES%% *}"
else
  record "$SUITE" "browser->proxy-allows-internet" FAIL "proxy path broken (curl exit=$PROXY_RC) — drill sanity check failed"
fi

# --- 3. gateway dispatch auth --------------------------------------------------
# Unauthenticated dispatch → 401. Wrong token → 401. Run from the agent pod so
# we test the real path (not localhost).
CODE=$(curl_in_pod "$SQUAD_NS" "$AGENT_POD" "" -- \
  --max-time 8 -X POST -H 'content-type: application/json' -d '{}' "${GW_URL}/v1/echo")
if [[ "${CODE%% *}" == "401" ]]; then
  record "$SUITE" "gateway-unauth-dispatch" PASS "POST /v1/echo no-auth → 401"
else
  record "$SUITE" "gateway-unauth-dispatch" FAIL "POST /v1/echo no-auth → http=${CODE%% *} (expected 401; exit=${CODE##* })"
fi

REAL_AGENT=$(kubectl get agents.skquad.io -A -o json 2>/dev/null \
  | jq -r '[.items[] | select(.status.phase=="Ready")][0].spec.agentId // empty')
if [[ -z "$REAL_AGENT" ]]; then
  record "$SUITE" "gateway-wrong-token-dispatch" FAIL "no Ready agent CR found to source a real agent id for the wrong-token test"
else
  CODE=$(curl_in_pod "$SQUAD_NS" "$AGENT_POD" "" -- \
    --max-time 8 -X POST \
    -H 'content-type: application/json' \
    -H "X-Skquad-Agent-ID: ${REAL_AGENT}" \
    -H 'Authorization: Bearer wrong-token-0000000000' \
    -d '{}' "${GW_URL}/v1/echo")
  if [[ "${CODE%% *}" == "401" ]]; then
    record "$SUITE" "gateway-wrong-token-dispatch" PASS "POST /v1/echo wrong token (real agent id) → 401"
  else
    record "$SUITE" "gateway-wrong-token-dispatch" FAIL "POST /v1/echo wrong token (real agent id) → http=${CODE%% *} (expected 401; exit=${CODE##* })"
  fi
  # NOTE: unknown-agent-id returns 502 policy_unavailable (fail-closed deny, not
  # an allow) — status-code asymmetry only; both deny. Tested separately:
  CODE=$(curl_in_pod "$SQUAD_NS" "$AGENT_POD" "" -- \
    --max-time 8 -X POST \
    -H 'content-type: application/json' \
    -H 'X-Skquad-Agent-ID: drill-nonexistent-agent' \
    -H 'Authorization: Bearer wrong-token-0000000000' \
    -d '{}' "${GW_URL}/v1/echo")
  record "$SUITE" "gateway-unknown-agent-dispatch" INFO "unknown agent id → http=${CODE%% *} (502=policy_unavailable fail-closed deny; acceptable but distinguishes unknown-agent from bad-cred)"
fi

# --- report --------------------------------------------------------------------
flush_results "$SUITE"
if summary_exit; then
  log "BYPASS SUITE: all isolation properties hold."
else
  log "BYPASS SUITE: ${FAIL_COUNT} FAILURE(S) — see [FAIL] lines above. These are SYSTEM policy findings, not drill artifacts."
fi
summary_exit
