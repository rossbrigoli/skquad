#!/usr/bin/env bash
# lib.sh — shared helpers for skquad security drills (TG-9 / S-255).
# Sourced by bypass_suite.sh, secret_probe.sh, revocation_drill.sh.
# Defensive: read-only against the cluster except for drill-owned throwaway
# resources named skquad-drill-* (created/deleted by the drills themselves).

set -u -o pipefail

export KUBECONFIG="${KUBECONFIG:-$HOME/projects/k3s-cluster/kubeconfig}"

DRILL_PREFIX="skquad-drill"
DRILL_TS="$(date +%s)"
RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_FILE="${RESULTS_FILE:-${RESULTS_DIR}/results-$(date +%Y%m%d).json}"

# Network targets (overridable for other environments)
LAB_IP="${LAB_IP:-192.168.68.131}"
METADATA_IP="${METADATA_IP:-169.254.169.254}"
INTERNET_URL="${INTERNET_URL:-https://example.com}"
GW_URL="${GW_URL:-http://skquad-tool-gateway.skquad-system.svc.cluster.local:8080}"
CP_URL="${CP_URL:-http://skquad-api-server.skquad-system.svc.cluster.local:80}"
BROWSER_NS="${BROWSER_NS:-skquad-browser}"
# Agent-position namespace: squad namespaces carry the agent netpols
# (default-deny + DNS + platform-egress only). Drill throwaway pods run here
# so they inherit the SAME network policy as real agent runtime pods.
SQUAD_NS="${SQUAD_NS:-squad-test-squad-6d9ed084}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"

FAIL_COUNT=0
PASS_COUNT=0
CHECKS_JSON="[]"   # accumulated {suite,check,status,detail}

log()  { printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
err()  { printf '%s ERROR: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }

# record <suite> <check-name> <PASS|FAIL|INFO> <detail>
record() {
  local suite="$1" check="$2" status="$3" detail="$4"
  CHECKS_JSON=$(printf '%s' "$CHECKS_JSON" | jq -c \
    --arg s "$suite" --arg c "$check" --arg st "$status" --arg d "$detail" \
    '. + [{suite:$s, check:$c, status:$st, detail:$d, ts:(now|todate)}]')
  printf '%-6s [%s] %s — %s\n' "$status" "$suite" "$check" "$detail"
  case "$status" in
    PASS) PASS_COUNT=$((PASS_COUNT + 1)) ;;
    FAIL) FAIL_COUNT=$((FAIL_COUNT + 1)) ;;
    *) : ;;
  esac
}

# flush_results <suite-name> — merge this run into the daily results JSON.
flush_results() {
  local suite="$1"
  local section
  section=$(jq -nc --arg s "$suite" --argjson c "$CHECKS_JSON" \
    '{suite:$s, checks:$c, finishedAt:(now|todate)}')
  local merged
  if [[ -f "$RESULTS_FILE" ]]; then
    merged=$(jq -n --argjson old "$(cat "$RESULTS_FILE")" --arg s "$suite" --argjson new "$section" \
      '$old | .[$s] = ($new + {runs: ((($old[$s].runs // 0) + 1))})') || { err "failed to merge results"; return 1; }
  else
    merged=$(jq -n --argjson new "$section" '{($new.suite): $new}')
  fi
  printf '%s\n' "$merged" > "$RESULTS_FILE"
  log "results written to ${RESULTS_FILE}"
}

summary_exit() {
  log "summary: ${PASS_COUNT} PASS, ${FAIL_COUNT} FAIL"
  [[ "$FAIL_COUNT" -eq 0 ]]
}

# require <cmd...> — abort if a required binary is missing
require() {
  command -v "$1" >/dev/null 2>&1 || { err "required command not found: $1"; exit 2; }
}

# find_browser_pod — first Running browser-service pod
find_browser_pod() {
  kubectl -n "$BROWSER_NS" get pods \
    -l app.kubernetes.io/component=browser-service \
    -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' \
    | head -n1
}

# curl_in_pod <ns> <pod> [container] -- <curl args...>
# Prints "<http_code_or_000> <curl_exit>" on one line.
curl_in_pod() {
  local ns="$1" pod="$2" container="$3"; shift 3
  [[ "$1" == "--" ]] && shift
  local out rc
  out=$(kubectl -n "$ns" exec "$pod" ${container:+-c "$container"} -- \
    curl -sS -o /dev/null -w '%{http_code}' "$@" 2>/dev/null)
  rc=$?
  echo "${out:-000} ${rc}"
}

# expect_blocked <suite> <check> <ns> <pod> <container> <url> [max_time_s]
# PASS when the URL is NOT reachable (curl exit != 0). Distinguishes timeout
# (28) from refused/unreachable (7/…) in the detail line.
expect_blocked() {
  local suite="$1" check="$2" ns="$3" pod="$4" container="$5" url="$6" mt="${7:-8}"
  local res rc
  res=$(curl_in_pod "$ns" "$pod" "$container" -- --max-time "$mt" "$url")
  rc="${res##* }"
  if [[ "$rc" != "0" ]]; then
    local why="blocked"
    [[ "$rc" == "28" ]] && why="timeout (blocked or no respondent)"
    record "$suite" "$check" PASS "curl exit=$rc ($why) for $url"
  else
    record "$suite" "$check" FAIL "REACHABLE (http=${res%% *}) — expected blocked: $url"
  fi
}
