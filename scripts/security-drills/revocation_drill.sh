#!/usr/bin/env bash
# revocation_drill.sh — TG-9 (S-255) revocation-latency timing harness.
#
# HOW GRANTS MATERIALIZE (inspected 2026-10-07):
#   Grants are DB-only (control-plane Postgres). They are NOT K8s objects or
#   secrets, so a drill cannot (and must not) delete grant rows directly.
#   The tool-gateway resolves grants via the CP endpoint
#       GET <api-server>/internal/v1/policy?agent=<agent-id>
#   through a TTL+ETag cache (tool-gateway/internal/policy/client.go):
#     - SKQUAD_TOOL_GATEWAY_POLICY_TTL (live value: 30s)
#     - fail-closed: expired entry + CP unreachable => deny (no stale-allow)
#   Therefore worst-case revocation staleness at the gateway ≈ PolicyTTL.
#
# HARNESS MECHANICS:
#   Port-forwards skquad-api-server and polls the policy endpoint every
#   INTERVAL seconds, recording timestamp + ETag + sha256(snapshot).
#
#   Full mode (admin has just revoked a grant via UI/API):
#     ./revocation_drill.sh --agent <id> --grant-delete-ts 2026-10-07T02:00:00Z [--resource <res-id>]
#     → finds the first poll whose snapshot changed AFTER the delete timestamp
#       and reports staleness = change_ts - delete_ts. Target ≤ 30s.
#
#   Poll-only mode (no revocation performed — proves the harness works):
#     ./revocation_drill.sh --poll-only
#     → verifies endpoint liveness, ETag/304 revalidation behaviour, reads the
#       gateway's configured PolicyTTL, and reports the theoretical bound.
#
# Read-only against the cluster (port-forward + GETs). Exit 0 on PASS.

set -u -o pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${DIR}/lib.sh"
require kubectl jq openssl

SUITE="revocation-drill"
AGENT_ID=""
DELETE_TS=""
RESOURCE_ID=""
POLL_ONLY=0
INTERVAL=2
POLL_SECONDS=60
LOCAL_PORT="${LOCAL_PORT:-18080}"
TARGET_STALENESS=30

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --agent) AGENT_ID="${2:?}"; shift 2 ;;
    --grant-delete-ts) DELETE_TS="${2:?}"; shift 2 ;;
    --resource) RESOURCE_ID="${2:?}"; shift 2 ;;
    --poll-only) POLL_ONLY=1; shift ;;
    --interval) INTERVAL="${2:?}"; shift 2 ;;
    --poll-seconds) POLL_SECONDS="${2:?}"; shift 2 ;;
    -h|--help) usage ;;
    *) err "unknown arg: $1"; usage ;;
  esac
done

PF_PID=""
cleanup() {
  if [[ -n "$PF_PID" ]] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

start_port_forward() {
  log "port-forwarding svc/skquad-api-server → 127.0.0.1:${LOCAL_PORT}"
  kubectl -n skquad-system port-forward svc/skquad-api-server "${LOCAL_PORT}:80" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 20); do
    if curl -s -o /dev/null --max-time 2 "http://127.0.0.1:${LOCAL_PORT}/internal/v1/policy?agent=preflight" ; then
      return 0
    fi
    sleep 0.5
  done
  # any HTTP answer counts (404/400 fine — endpoint alive); transport failure is fatal
  curl -s -o /dev/null --max-time 2 "http://127.0.0.1:${LOCAL_PORT}/healthz" && return 0
  err "port-forward never became ready"
  exit 2
}

# poll_once <agent> [etag] → prints: http_code|etag|sha256|body_len
poll_once() {
  local agent="$1" etag="${2:-}"
  local hdr_file body_file args=(-s --max-time 5 -D - -o /tmp/drill_body.$$.json)
  [[ -n "$etag" ]] && args+=(-H "If-None-Match: ${etag}")
  local headers code
  touch /tmp/drill_body.$$.json   # 304 responses write no body
  headers=$(curl "${args[@]}" "http://127.0.0.1:${LOCAL_PORT}/internal/v1/policy?agent=${agent}")
  code=$(printf '%s' "$headers" | awk 'NR==1 {print $2}')
  local new_etag sha len
  new_etag=$(printf '%s' "$headers" | awk -v IGNORECASE=1 '/^etag:/ {gsub(/\r/,""); print $2}')
  sha=$(openssl dgst -sha256 /tmp/drill_body.$$.json 2>/dev/null | awk '{print $2}')
  len=$(wc -c < /tmp/drill_body.$$.json 2>/dev/null || echo 0)
  rm -f /tmp/drill_body.$$.json
  echo "${code}|${new_etag:-none}|${sha:-none}|${len}"
}

# gateway policy TTL as configured live
GW_TTL=$(kubectl -n skquad-system get deploy skquad-tool-gateway \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SKQUAD_TOOL_GATEWAY_POLICY_TTL")].value}' 2>/dev/null)
GW_TTL="${GW_TTL:-unknown}"

if [[ "$POLL_ONLY" -eq 1 ]]; then
  [[ -n "$AGENT_ID" ]] || {
    AGENT_ID=$(kubectl get agents.skquad.io -A -o json 2>/dev/null \
      | jq -r '[.items[] | select(.status.phase=="Ready")][0].spec.agentId // empty')
  }
  [[ -n "$AGENT_ID" ]] || { err "no agent available to poll (no Ready agents.skquad.io found)"; exit 2; }
  log "POLL-ONLY mode: agent=${AGENT_ID} interval=${INTERVAL}s window=${POLL_SECONDS}s"
  start_port_forward

  FIRST=$(poll_once "$AGENT_ID")
  log "first poll: http|etag|sha|len = ${FIRST}"
  CODE="${FIRST%%|*}"
  if [[ "$CODE" != "200" ]]; then
    record "$SUITE" "policy-endpoint-live" FAIL "GET /internal/v1/policy → ${CODE} (expected 200)"
  else
    record "$SUITE" "policy-endpoint-live" PASS "GET /internal/v1/policy → 200 (etag=$(echo "$FIRST" | cut -d'|' -f2 | cut -c1-20)…, body=$(echo "$FIRST" | cut -d'|' -f4)B)"
  fi

  ETAG=$(echo "$FIRST" | cut -d'|' -f2)
  SECOND=$(poll_once "$AGENT_ID" "$ETAG")
  CODE2=$(echo "$SECOND" | cut -d'|' -f1)
  if [[ "$CODE2" == "304" ]]; then
    record "$SUITE" "etag-revalidation" PASS "If-None-Match → 304 (cache revalidation works; unchanged policy refreshes TTL without re-transfer)"
  else
    record "$SUITE" "etag-revalidation" INFO "If-None-Match → ${CODE2} (304 expected if ETag supported; $([ -z "$ETAG" ] || [ "$ETAG" = none ] && echo 'no ETag advertised by CP — gateway falls back to full refetch per TTL')"
  fi

  record "$SUITE" "gateway-policy-ttl" INFO "live SKQUAD_TOOL_GATEWAY_POLICY_TTL=${GW_TTL} → worst-case revocation staleness ≈ ${GW_TTL} (cache is TTL-bound and fail-closed)"

  # short observation loop to prove timing granularity
  BASE_SHA=$(echo "$FIRST" | cut -d'|' -f3)
  CHANGED=0
  ENDE=$(( $(date +%s) + POLL_SECONDS ))
  while [[ $(date +%s) -lt $ENDE ]]; do
    sleep "$INTERVAL"
    P=$(poll_once "$AGENT_ID")
    SHA=$(echo "$P" | cut -d'|' -f3)
    log "poll @ $(date -u +%H:%M:%SZ): http=$(echo "$P"|cut -d'|' -f1) sha=${SHA:0:12}…"
    [[ "$SHA" != "$BASE_SHA" ]] && { CHANGED=1; break; }
  done
  if [[ "$CHANGED" -eq 0 ]]; then
    record "$SUITE" "poll-loop-stability" PASS "policy snapshot stable across ${POLL_SECONDS}s window (no spurious churn); harness change-detection armed for real revocations"
  else
    record "$SUITE" "poll-loop-stability" INFO "policy changed mid-window (unprompted) — harness detected it; fine, re-run in full mode for measurement"
  fi
  record "$SUITE" "harness-mechanics" INFO "full-mode usage: ./revocation_drill.sh --agent <id> --grant-delete-ts <ISO8601> --resource <res-id> — admin revokes via UI/API, harness timestamps snapshot change; target staleness ≤ ${TARGET_STALENESS}s"
  flush_results "$SUITE"
  summary_exit
  exit $?
fi

# --- full mode ---------------------------------------------------------------
[[ -n "$AGENT_ID" && -n "$DELETE_TS" ]] || { err "full mode requires --agent and --grant-delete-ts (or use --poll-only)"; exit 2; }
DELETE_EPOCH=$(date -d "$DELETE_TS" +%s 2>/dev/null) || { err "bad --grant-delete-ts: $DELETE_TS"; exit 2; }

start_port_forward
BASE=$(poll_once "$AGENT_ID")
BASE_SHA=$(echo "$BASE" | cut -d'|' -f3)
BASE_HAS_RES="no"
# fetch body to check resource presence
curl -s --max-time 5 "http://127.0.0.1:${LOCAL_PORT}/internal/v1/policy?agent=${AGENT_ID}" -o /tmp/drill_base.json
if [[ -n "$RESOURCE_ID" ]] && grep -q "$RESOURCE_ID" /tmp/drill_base.json; then BASE_HAS_RES="yes"; fi
rm -f /tmp/drill_base.json
log "baseline sha=${BASE_SHA:0:12}… resource_present=${BASE_HAS_RES}"

if [[ -n "$RESOURCE_ID" && "$BASE_HAS_RES" == "no" ]]; then
  record "$SUITE" "precondition" FAIL "resource ${RESOURCE_ID} not present in policy BEFORE delete — cannot measure revocation (wrong agent/resource or already revoked)"
  flush_results "$SUITE"; exit 1
fi

CHANGE_TS=""
ENDE=$(( $(date +%s) + POLL_SECONDS ))
while [[ $(date +%s) -lt $ENDE ]]; do
  sleep "$INTERVAL"
  P=$(poll_once "$AGENT_ID")
  SHA=$(echo "$P" | cut -d'|' -f3)
  NOW=$(date +%s)
  log "poll @ $(date -u +%H:%M:%SZ) (t+=$((NOW-DELETE_EPOCH))s since delete): sha=${SHA:0:12}…"
  if [[ "$SHA" != "$BASE_SHA" ]]; then CHANGE_TS="$NOW"; break; fi
done

if [[ -n "$CHANGE_TS" ]]; then
  STALE=$((CHANGE_TS - DELETE_EPOCH))
  if [[ "$STALE" -le "$TARGET_STALENESS" ]]; then
    record "$SUITE" "revocation-latency" PASS "policy snapshot changed ${STALE}s after grant deletion (target ≤ ${TARGET_STALENESS}s)"
  else
    record "$SUITE" "revocation-latency" FAIL "policy snapshot changed only ${STALE}s after deletion — EXCEEDS ${TARGET_STALENESS}s target"
  fi
else
  record "$SUITE" "revocation-latency" FAIL "no policy change observed within ${POLL_SECONDS}s of deletion — revocation not propagating (or delete_ts in the future)"
fi

flush_results "$SUITE"
summary_exit
