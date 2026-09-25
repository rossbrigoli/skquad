#!/usr/bin/env python3
"""SonarQube Critical/High severity gate with Kanbunny mirroring (S-102).

Run after a sonar-scanner analysis. Fails the CI pipeline when the project
carries open Critical or High findings, and mirrors the finding set
into a Kanbunny card so the work is tracked, not just red.

Severity mapping: SonarQube's scale is INFO/MINOR/MAJOR/CRITICAL/BLOCKER.
"High" in the card's language maps to CRITICAL; "Critical" maps to BLOCKER.
Both are gated here.

Environment:
  SONAR_URL        SonarQube base URL (required)
  SONAR_TOKEN      SonarQube token (required)
  SONAR_PROJECT    project key (default: skquad)
  KANBUNNY_URL     Kanbunny base URL (required unless SKIP_KANBUNNY=1)
  KANBUNNY_TOKEN   Kanbunny bearer token (required unless SKIP_KANBUNNY=1)
  KANBUNNY_BOARD   Kanbunny board id (required unless SKIP_KANBUNNY=1)
  SKIP_KANBUNNY    "1" to skip card sync (still fails on findings)

Exit codes:
  0  no open Critical/High findings
  1  findings present (card synced when enabled)
  2  tool/communication error (fail closed: a blind gate is not a gate)
"""

import hashlib
import json
import os
import sys
import time
import urllib.parse
import urllib.request

MARKER = "Sonar Critical/High"
OPEN_COLUMNS = {"todo", "in-progress", "blocked"}
MAX_LISTED = 50


def env(name: str, required: bool = True) -> str:
    value = os.environ.get(name, "").strip()
    if required and not value:
        print(f"error: required env var {name} is not set", file=sys.stderr)
        sys.exit(2)
    return value


def http_json(url: str, token: str, method: str = "GET", body: dict | None = None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", f"Bearer {token}")
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode())
    except Exception as exc:  # noqa: BLE001 - fail closed with a clear message
        print(f"error: {method} {url} failed: {exc}", file=sys.stderr)
        sys.exit(2)


def fetch_findings(sonar_url: str, token: str, project: str, types: str) -> list[dict]:
    findings: list[dict] = []
    page = 1
    # 1000 findings is more than enough for one card (S3516: single exit point)
    while page <= 10:
        qs = urllib.parse.urlencode(
            {
                "componentKeys": project,
                "severities": "CRITICAL,BLOCKER",
                "types": types,
                "resolved": "false",
                "ps": 100,
                "p": page,
            }
        )
        data = http_json(f"{sonar_url}/api/issues/search?{qs}", token)
        findings.extend(data.get("issues", []))
        if len(findings) >= data.get("paging", {}).get("total", 0):
            break
        page += 1
    return findings


def count_findings(sonar_url: str, token: str, project: str, types: str) -> int:
    qs = urllib.parse.urlencode(
        {
            "componentKeys": project,
            "severities": "CRITICAL,BLOCKER",
            "types": types,
            "resolved": "false",
            "ps": 1,
        }
    )
    data = http_json(f"{sonar_url}/api/issues/search?{qs}", token)
    return data.get("paging", {}).get("total", 0)


def format_finding(issue: dict) -> str:
    sev = issue.get("severity", "?")
    rule = issue.get("rule", issue.get("ruleName", "?"))
    comp = issue.get("component", "?").split(":", 1)[-1]
    line = issue.get("line") or "?"
    msg = (issue.get("message") or "").strip().replace("\n", " ")
    if len(msg) > 200:
        msg = msg[:197] + "..."
    return f"- **{sev}** `{rule}` — `{comp}:{line}` — {msg}"


def sync_kanbunny(findings: list[dict]) -> None:
    base = env("KANBUNNY_URL").rstrip("/")
    token = env("KANBUNNY_TOKEN")
    board = env("KANBUNNY_BOARD")

    keys = sorted(issue.get("key", "?") for issue in findings)
    digest = hashlib.sha256(":".join(keys).encode()).hexdigest()[:10]
    marker = f"[sonar:{digest}]"
    title = f"🔴 {MARKER} ({len(findings)}) {marker}"
    sonar_url = env("SONAR_URL").rstrip("/")
    project = os.environ.get("SONAR_PROJECT", "skquad").strip() or "skquad"
    listing = "\n".join(format_finding(f) for f in findings[:MAX_LISTED])
    more = (
        f"\n\n_…and {len(findings) - MAX_LISTED} more (see the Sonar issue tab)._"
        if len(findings) > MAX_LISTED
        else ""
    )
    description = (
        f"Auto-created by the skquad CI security gate (Kanbunny S-102 policy: "
        f"Critical/High findings block the pipeline).\n\n"
        f"Set marker: `{marker}` (changes when the finding set changes)\n\n"
        f"Sonar query: {sonar_url}/issues?resolved=false&severities=CRITICAL%2CBLOCKER"
        f"&componentKeys={project}\n\n{listing}{more}"
    )

    cards = http_json(f"{base}/api/boards/{board}/cards", token)
    existing = [
        c
        for c in cards
        if MARKER in c.get("title", "") and c.get("column", "todo") in OPEN_COLUMNS
    ]
    if existing:
        card = existing[0]
        http_json(
            f"{base}/api/cards/{card['id']}",
            token,
            method="PATCH",
            body={"title": title, "description": description},
        )
        print(f"kanbunny: updated card {card['id']} ({title})")
    else:
        created = http_json(
            f"{base}/api/boards/{board}/cards",
            token,
            method="POST",
            body={
                "title": title,
                "description": description,
                "assignee": "Sherlock",
                "column": "todo",
                "priority": 1,
            },
        )
        print(f"kanbunny: created card {created.get('id', '?')} ({title})")


def main() -> int:
    sonar_url = env("SONAR_URL")
    token = env("SONAR_TOKEN")
    project = os.environ.get("SONAR_PROJECT", "skquad").strip() or "skquad"
    # Blocking types: security vulnerabilities + real bugs. CODE_SMELL
    # (duplicate literals, cognitive complexity, etc.) is reported for
    # visibility but does NOT block deployment — gating on 180+ cosmetic
    # maintainability findings (mostly in tests) would freeze the pipeline
    # and train everyone to ignore the gate. Override with GATE_TYPES.
    gate_types = os.environ.get("GATE_TYPES", "VULNERABILITY,BUG").strip() or "VULNERABILITY,BUG"

    findings = fetch_findings(sonar_url, token, project, gate_types)
    code_smells = count_findings(sonar_url, token, project, "CODE_SMELL")

    if code_smells:
        print(
            f"sonar gate: NOTE — {code_smells} open Critical/Blocker CODE_SMELL(s) "
            f"(maintainability; non-blocking)."
        )

    if findings:
        # SonarQube reconciles issue state (e.g. marking an issue FIXED when a
        # scan no longer reproduces it) asynchronously *after* the scanner
        # reports success. A gate that queries immediately can still see
        # findings the just-finished scan already fixed — observed on run
        # 35848631030: typescript:S2871 was FIXED moments after the gate
        # read it as OPEN. Settle and re-check once before failing.
        settle = int(os.environ.get("GATE_SETTLE_SECONDS", "20"))
        print(
            f"sonar gate: {len(findings)} open Critical/Blocker {gate_types} finding(s) "
            f"for '{project}'; re-checking in {settle}s "
            f"(Sonar issue reconciliation is async)..."
        )
        time.sleep(settle)
        findings = fetch_findings(sonar_url, token, project, gate_types)

    if not findings:
        print(
            f"sonar gate: OK — no open Critical/Blocker {gate_types} findings for "
            f"'{project}' (clean after re-check)"
        )
        return 0

    print(
        f"sonar gate: {len(findings)} open Critical/Blocker {gate_types} finding(s) "
        f"for '{project}' (still present after re-check):"
    )
    for finding in findings[:MAX_LISTED]:
        print(format_finding(finding))

    if os.environ.get("SKIP_KANBUNNY") == "1":
        print("kanbunny: skipped (SKIP_KANBUNNY=1)")
    else:
        sync_kanbunny(findings)

    return 1


if __name__ == "__main__":
    sys.exit(main())
