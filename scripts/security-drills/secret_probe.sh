#!/usr/bin/env bash
# secret_probe.sh — TG-9 (S-255) environment-secret custody probe.
#
# Gateway-custody pattern: AGENT-position pods must carry ZERO secrets in
# their environment — credentials live with the tool-gateway/control plane
# and are injected at call time. The browser pod is also data-plane and is
# asserted the same way.
#
# For every pod in skquad-system + skquad-browser (+ any pod anywhere with
# the agent label skquad.io/agent-id):
#   * dump env var NAMES only (NEVER values)
#   * flag names matching TOKEN|SECRET|KEY|PASSWORD
#   * for AGENT/BROWSER-position pods: any flagged name with a non-empty
#     literal value, OR any valueFrom.secretKeyRef, is a VIOLATION.
#
# EXEMPT (list-only, justified): control-plane infra pods legitimately hold
# secrets — api-server (DB + secret writer), operator (controller creds),
# tool-gateway (custodian of resource credentials — that IS the custody
# pattern), llm-gateway (BYOM provider keys are its job), postgres (DB),
# web/embedder (session/KB infra). They are listed for inventory but not
# asserted.
#
# Read-only. Exit 0 only if no agent/browser-position violations.

set -u -o pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${DIR}/lib.sh"
require kubectl jq

SUITE="secret-probe"
SECRET_NAME_RE='TOKEN|SECRET|KEY|PASSWORD'

# Pod classes:
#  ASSERTED: browser pods (data-plane) + agent runtime pods (label skquad.io/agent-id)
#  EXEMPT:   skquad-system infra (see header justification)
# NOTE: a running agent-runtime pod in a squad namespace would be caught by the
# label selector below. As of 2026-10-07 no agent runtime pods are running
# (verified: `kubectl get pods -A -l skquad.io/agent-id` empty) — the browser
# pod is the only live data-plane workload; the squad-namespace assertion runs
# against any that appear (and against bypass_suite's throwaway pod if present).

VIOL=0

# probe_pod <namespace> <pod> <class:ASSERTED|EXEMPT>
probe_pod() {
  local ns="$1" pod="$2" class="$3"
  local json
  json=$(kubectl -n "$ns" get pod "$pod" -o json 2>/dev/null) || { err "cannot read pod ${ns}/${pod}"; return; }

  # Per container: name, then flagged env entries: NAME (literal:nonempty|literal:empty|secretRef|configMapRef|fieldRef|other)
  local lines
  lines=$(printf '%s' "$json" | jq -r --arg re "$SECRET_NAME_RE" '
    .spec.containers[] as $c
    | ($c.env // [])[]
    | select(.name | test($re))
    | [ $c.name, .name,
        ( if (.value // "") != "" then "literal:nonempty"
          elif has("valueFrom") then
            ( if (.valueFrom | has("secretKeyRef")) then "secretRef"
              elif (.valueFrom | has("configMapKeyRef")) then "configMapRef"
              elif (.valueFrom | has("fieldRef")) then "fieldRef"
              else "otherRef" end )
          else "literal:empty" end ) ]
    | @tsv')

  local init_lines
  init_lines=$(printf '%s' "$json" | jq -r --arg re "$SECRET_NAME_RE" '
    (.spec.initContainers // [])[] as $c
    | ($c.env // [])[]
    | select(.name | test($re))
    | [ "init:" + $c.name, .name,
        ( if (.value // "") != "" then "literal:nonempty"
          elif has("valueFrom") then
            ( if (.valueFrom | has("secretKeyRef")) then "secretRef"
              else "otherRef" end )
          else "literal:empty" end ) ]
    | @tsv')

  local all="${lines}${init_lines:+$'\n'${init_lines}}"
  local count
  count=$(printf '%s' "$all" | grep -c . || true)

  if [[ "$class" == "EXEMPT" ]]; then
    record "$SUITE" "inventory:${ns}/${pod}" INFO "exempt infra pod — ${count} secret-ish env var(s): $(printf '%s' "$all" | awk -F'\t' '{printf "%s(%s) ", $2, $3}' | sed 's/ $//')"
    return
  fi

  if [[ "$count" -eq 0 ]]; then
    record "$SUITE" "assert:${ns}/${pod}" PASS "zero secret-ish env vars (gateway custody holds)"
    return
  fi

  # ASSERTED pod: any literal:nonempty or secretRef is a violation.
  local bad
  bad=$(printf '%s\n' "$all" | awk -F'\t' '$3=="literal:nonempty" || $3=="secretRef" {printf "%s/%s(%s) ", $1, $2, $3}')
  if [[ -n "$bad" ]]; then
    record "$SUITE" "assert:${ns}/${pod}" FAIL "secrets present in data-plane env: ${bad}"
    VIOL=$((VIOL + 1))
  else
    record "$SUITE" "assert:${ns}/${pod}" PASS "secret-ish names present but all empty/non-secret refs: $(printf '%s' "$all" | awk -F'\t' '{printf "%s(%s) ", $2, $3}')"
  fi
}

# --- agent-position pods (label-based, all namespaces) ------------------------
AGENT_PODS=$(kubectl get pods -A -l skquad.io/agent-id -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
if [[ -z "$AGENT_PODS" ]]; then
  record "$SUITE" "agent-runtime-pods" INFO "no running pods with label skquad.io/agent-id (agent runtime idle — browser pod is the live data-plane position; squad-netpol assertion covered by bypass_suite throwaway pod)"
else
  while IFS=/ read -r ns pod; do
    [[ -n "$pod" ]] && probe_pod "$ns" "$pod" ASSERTED
  done <<< "$AGENT_PODS"
fi

# --- browser namespace: ALL pods asserted --------------------------------------
while IFS= read -r pod; do
  [[ -n "$pod" ]] && probe_pod "$BROWSER_NS" "$pod" ASSERTED
done < <(kubectl -n "$BROWSER_NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

# --- skquad-system: inventory + exempt ----------------------------------------
# Exception carve-out: drill pods themselves (skquad-drill-*) are also asserted,
# not exempt — they carry no secrets by construction.
while IFS= read -r pod; do
  [[ -z "$pod" ]] && continue
  if [[ "$pod" == ${DRILL_PREFIX}-* ]]; then
    probe_pod skquad-system "$pod" ASSERTED
  else
    probe_pod skquad-system "$pod" EXEMPT
  fi
done < <(kubectl -n skquad-system get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

flush_results "$SUITE"
log "violations: ${VIOL}"
[[ "$VIOL" -eq 0 ]] && { log "SECRET PROBE: custody pattern holds for all data-plane pods."; exit 0; }
log "SECRET PROBE: ${VIOL} data-plane pod(s) carry secrets in env — custody pattern VIOLATED (system finding)."
exit 1
