"""S-T6 (2026-11-04): git credential helper for gateway-backed git.

Emits the *agent's own gateway credential* as HTTP Basic material
(``username = agent id``, ``password = <agent credential>``) for git
credential requests aimed at the tool-gateway's git proxy host — and for
NO other host. This is what lets a plain ``git clone/fetch/push`` inside
an agent pod (including inside a task subagent's shell tools) run under
the agent's identity: the gateway authenticates the credential against
the CP-delivered hash, enforces the grant's ``repos_allow``/``allow_push``,
and injects the real upstream PAT server-side. The upstream credential
never exists in the pod.

Wiring (all env, injected by the operator today for the synthetic
gateway tools — no new mounts):

- ``SKQUAD_AGENT_ID``            — the agent identity presented as the
  Basic username.
- ``SKQUAD_AGENT_CREDENTIAL_PATH`` — file holding the agent's gateway
  credential (same file ``rest_call`` sends as ``Bearer``).
- ``SKQUAD_TOOL_GATEWAY_URL``    — the gateway base URL; the helper only
  answers for this host.

Configured as the runtime image's global ``credential.helper`` (see
``Dockerfile``). Nothing secret is ever written to disk by this helper:
the credential is read from its mount at call time and written only to
the stdout pipe git reads from. ``store``/``erase`` are deliberately
no-ops so git never persists anything.
"""

from __future__ import annotations

import os
import sys
from typing import Mapping
from urllib.parse import urlsplit

# Mirrors skquad_runtime.runtime.DEFAULT_AGENT_CREDENTIAL_PATH. Duplicated
# deliberately: this helper must stay importable without pulling the full
# runtime module graph (fastapi/litellm) into every git invocation.
DEFAULT_AGENT_CREDENTIAL_PATH = "/var/run/skquad/credentials/agent"


def gateway_host(environ: Mapping[str, str]) -> str:
    """Host[:port] of the tool gateway, lowercased. Empty when unset."""
    raw = (environ.get("SKQUAD_TOOL_GATEWAY_URL") or "").strip()
    if not raw:
        return ""
    parsed = urlsplit(raw if "://" in raw else f"//{raw}")
    host = (parsed.hostname or "").lower()
    # A netloc containing whitespace is not a host (e.g. "not a url").
    if not host or any(ch.isspace() for ch in host):
        return ""
    try:
        port = parsed.port
    except ValueError:
        return host
    return f"{host}:{port}" if port else host


def credential_for(
    request: Mapping[str, str], environ: Mapping[str, str]
) -> tuple[str, str] | None:
    """Return ``(username, password)`` for a git credential request, or
    ``None`` when the helper must stay silent (never answer for a host
    other than the gateway, or when identity material is missing)."""
    gw = gateway_host(environ)
    if not gw:
        return None
    host = (request.get("host") or "").strip().lower()
    # Normalize the default-port case both ways: git omits ":80"/":443"
    # for default schemes, the configured URL may or may not carry it.
    if host != gw and f"{host}:80" != gw and f"{host}:443" != gw:
        return None
    agent_id = (environ.get("SKQUAD_AGENT_ID") or "").strip()
    if not agent_id:
        return None
    cred_path = (
        environ.get("SKQUAD_AGENT_CREDENTIAL_PATH") or DEFAULT_AGENT_CREDENTIAL_PATH
    )
    try:
        credential = open(cred_path, encoding="utf-8").read().strip()
    except OSError:
        return None
    if not credential:
        return None
    return agent_id, credential


def parse_request(stdin_lines) -> dict[str, str]:
    """Parse git's ``key=value`` credential request lines."""
    request: dict[str, str] = {}
    for line in stdin_lines:
        line = line.strip()
        if not line or "=" not in line:
            continue
        key, _, value = line.partition("=")
        request[key.strip()] = value.strip()
    return request


def main(
    argv: list[str] | None = None,
    stdin=None,
    environ: Mapping[str, str] | None = None,
    stdout=None,
) -> int:
    argv = list(sys.argv[1:]) if argv is None else argv
    environ = os.environ if environ is None else environ
    stdin = sys.stdin if stdin is None else stdin
    stdout = sys.stdout if stdout is None else stdout
    # Only `get` answers. `store`/`erase` are no-ops: nothing is persisted.
    if not argv or argv[0] != "get":
        return 0
    request = parse_request(stdin.read().splitlines())
    pair = credential_for(request, environ)
    if pair is None:
        return 0
    username, password = pair
    stdout.write(f"username={username}\npassword={password}\n")
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
