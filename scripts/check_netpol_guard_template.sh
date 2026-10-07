#!/usr/bin/env bash
# check_netpol_guard_template.sh — TG-9 slice B chart-shape assertions.
#
# The netpol-guard init-container itself is built by the OPERATOR in Go
# (operator/internal/controller/agent_controller.go -> netpolGuardContainer)
# because agent pods are operator-created, not chart-templated. The chart's
# job is to deliver the guard configuration to the operator via
# SKQUAD_NETPOL_GUARD_* env vars on the operator Deployment. This script
# asserts that wiring with `helm template`:
#
#   1. defaults: guard ENABLED with the approved default knobs
#   2. --set agentNetpolGuard.enabled=false renders "false"
#   3. knob overrides render verbatim
#
# The init-container SHAPE (name, script, env, securityContext) is covered
# by operator unit tests (operator/internal/controller/netpol_guard_test.go).
#
# Exit 0 = all assertions pass.

set -u -o pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART="${CHART:-$DIR/../charts/skquad}"
FAILS=0

say() { printf '%-70s %s\n' "$1" "$2"; }
check() { # check <label> <needle> <rendered>
  if grep -qF "$2" "$3"; then say "$1" PASS; else say "$1" "FAIL (missing: $2)"; FAILS=$((FAILS+1)); fi
}
check_absent() {
  if grep -qF "$2" "$3"; then say "$1" "FAIL (unexpected: $2)"; FAILS=$((FAILS+1)); else say "$1" PASS; fi
}

command -v helm >/dev/null 2>&1 || { echo "helm not installed" >&2; exit 2; }

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# --- 1. defaults -------------------------------------------------------------
helm template drill "$CHART" > "$TMP/default.yaml" || { echo "helm template (defaults) failed" >&2; exit 2; }
check "default: guard enabled env"        'name: SKQUAD_NETPOL_GUARD_ENABLED' "$TMP/default.yaml"
check "default: guard enabled=true"      'value: "true"' "$TMP/default.yaml"
check "default: canary url"              'name: SKQUAD_NETPOL_GUARD_CANARY_URL' "$TMP/default.yaml"
check "default: canary example.com"      'value: "https://example.com"' "$TMP/default.yaml"
check "default: probe interval 250ms"    'name: SKQUAD_NETPOL_GUARD_PROBE_INTERVAL_MS' "$TMP/default.yaml"
check "default: max wait 30s"            'name: SKQUAD_NETPOL_GUARD_MAX_WAIT_SECONDS' "$TMP/default.yaml"
check "default: required probes 3"       'name: SKQUAD_NETPOL_GUARD_REQUIRED_BLOCKED_PROBES' "$TMP/default.yaml"
check "default: guard image"             'name: SKQUAD_NETPOL_GUARD_IMAGE' "$TMP/default.yaml"
check "default: guard image value"       'value: "curlimages/curl:8.10.1"' "$TMP/default.yaml"

# --- 2. disabled -------------------------------------------------------------
helm template drill "$CHART" --set agentNetpolGuard.enabled=false > "$TMP/off.yaml" || { echo "helm template (off) failed" >&2; exit 2; }
if awk '/name: SKQUAD_NETPOL_GUARD_ENABLED/{f=1} f && /value:/{print; exit}' "$TMP/off.yaml" | grep -q 'value: "false"'; then
  say "disabled: guard enabled=false" PASS
else
  say "disabled: guard enabled=false" "FAIL (expected value \"false\" under ENABLED env)"
  FAILS=$((FAILS+1))
fi

# --- 3. knob overrides --------------------------------------------------------
helm template drill "$CHART" \
  --set agentNetpolGuard.canaryUrl=https://canary.invalid \
  --set agentNetpolGuard.probeIntervalMs=500 \
  --set agentNetpolGuard.maxWaitSeconds=60 \
  --set agentNetpolGuard.requiredBlockedProbes=5 > "$TMP/knobs.yaml" || { echo "helm template (knobs) failed" >&2; exit 2; }
check "knobs: canary override"  'value: "https://canary.invalid"' "$TMP/knobs.yaml"
check "knobs: interval override" 'value: "500"' "$TMP/knobs.yaml"
check "knobs: maxwait override"  'value: "60"' "$TMP/knobs.yaml"
check "knobs: required override" 'value: "5"' "$TMP/knobs.yaml"

# --- 4. sanity: guard env lands on the OPERATOR deployment only ---------------
# (agent pods are not chart-rendered; nothing else should carry the env)
GUARD_ENV_HITS=$(grep -c 'SKQUAD_NETPOL_GUARD_' "$TMP/default.yaml" || true)
if [[ "$GUARD_ENV_HITS" -eq 6 ]]; then
  say "scope: exactly 6 guard env vars (operator deployment only)" PASS
else
  say "scope: guard env var count" "FAIL (got $GUARD_ENV_HITS, want 6)"
  FAILS=$((FAILS+1))
fi

if [[ "$FAILS" -gt 0 ]]; then
  echo "check_netpol_guard_template: $FAILS assertion(s) FAILED" >&2
  exit 1
fi
echo "check_netpol_guard_template: ALL PASS"
