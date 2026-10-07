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
CLEANED=0

cleanup() {
  [[ "$CLEANED" -eq 1 ]] && return
  CLEANED=1
  log "cleanup: deleting throwaway pod ${AGENT_POD} in ${SQUAD_NS}"
  kubectl -n "$SQUAD_NS" delete pod "$AGENT_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
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
  record "$SUITE" "agent-startup-race" FAIL "${RACE_HITS}/15 startup probes REACHED internet (first block: ${RACE_FIRST_BLOCK:-never}) — NetworkPolicy enforcement lags pod creation; exfiltration window for new agent pods"
else
  record "$SUITE" "agent-startup-race" PASS "no unrestricted startup window observed (first block: ${RACE_FIRST_BLOCK:-n/a})"
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
