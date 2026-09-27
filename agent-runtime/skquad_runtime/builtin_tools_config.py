"""Fetch-at-wake builtin-tools configuration client (BT-RUNTIME).

The control plane serves the agent's builtin-tool allowlist at
``GET /api/v1/agents/me/tools`` (agent-credential auth, ``ETag`` on
the tool set). The runtime fetches it before the first LLM call of every
wake, so a scale-to-zero agent picks up tool policy edits with no
redeploy — mirroring the composed-prompt pattern in
:mod:`skquad_runtime.prompt_fetch` (ADR-0011 D4).

Failure policy (fail loudly, never a silent degraded tool set):

- network failure / 5xx / malformed JSON -> :class:`BuiltinToolsFetchError`
  (the wake must fail; a partially-known tool set is unsafe).
- 404 (control plane predates the endpoint) -> ``LEGACY_NO_ENDPOINT``:
  the caller falls back to its legacy builtin set.
- ``SKQUAD_BUILTIN_TOOLS_ENABLED=false`` -> the caller treats the agent
  as ``KILL_SWITCHED`` (no builtin tools at all) via
  :func:`builtin_tools_enabled`.

ETag caching is per-process: repeat fetches within one process lifetime
send ``If-None-Match`` and a 304 reuses the cached tool list.
"""

from __future__ import annotations

import json
import logging
import os
import threading
import urllib.error
import urllib.request

LOGGER = logging.getLogger(__name__)

TOOLS_ENDPOINT = "/api/v1/agents/me/tools"

#: The fetch completed and ``tools`` reflects the control-plane policy.
OK = "ok"

#: The fetch could not complete trustworthily (network/5xx/malformed body).
#: The wake must fail loudly — never run with a partially-known tool set.
FETCH_FAILED = "fetch_failed"

#: The control plane has no builtin-tools endpoint (404). Transitional:
#: the caller may use its legacy builtin set.
LEGACY_NO_ENDPOINT = "legacy_no_endpoint"

#: Kill switch ``SKQUAD_BUILTIN_TOOLS_ENABLED`` is false. The caller runs
#: the agent with no builtin tools (see :func:`builtin_tools_enabled`).
KILL_SWITCHED = "kill_switched"


class BuiltinToolsFetchError(RuntimeError):
    """Raised when the builtin-tools fetch fails and no fallback is allowed."""


class BuiltinToolsConfigResult:
    """One fetched builtin-tools result (frozen by convention).

    ``status`` is one of OK / FETCH_FAILED / LEGACY_NO_ENDPOINT /
    KILL_SWITCHED. ``tools`` is the parsed list of tool entries
    (``{"name", "enabled", "policy"}``) when OK. ``etag`` is the
    validator to send on the next fetch.
    """

    __slots__ = ("status", "tools", "etag")

    def __init__(self, status: str, tools: list | None = None, etag: str = "") -> None:
        self.status = status
        self.tools: list = tools if tools is not None else []
        self.etag = etag

    def __repr__(self) -> str:  # never include tool bodies in logs
        return f"BuiltinToolsConfigResult(status={self.status!r}, tools={len(self.tools)})"


class BuiltinToolsConfigCache:
    """ETag-aware cache/fetcher for ``GET /api/v1/agents/me/tools``.

    Thread-safe: the lease-heartbeat thread and the task thread must never
    interleave a half-updated cache entry (same discipline as
    :class:`prompt_fetch.ComposedPromptCache`).

    ``opener`` mirrors :class:`ControlPlaneClient`'s injectable opener so
    tests can mock the HTTP layer without a network.
    """

    def __init__(
        self,
        control_plane_url: str,
        agent_credential: str,
        agent_id: str = "",
        opener=None,
    ) -> None:
        self.control_plane_url = control_plane_url.rstrip("/")
        self.agent_credential = agent_credential
        self.agent_id = agent_id
        self._opener = opener or urllib.request.urlopen
        self._lock = threading.Lock()
        self._etag: str = ""
        self._tools: list = []

    @property
    def etag(self) -> str:
        with self._lock:
            return self._etag

    def snapshot(self) -> tuple[str, list]:
        with self._lock:
            return self._etag, list(self._tools)

    def store(self, etag: str, tools: list) -> None:
        with self._lock:
            self._etag = etag
            self._tools = list(tools)

    def fetch(self) -> BuiltinToolsConfigResult:
        """Fetch the builtin-tools config, honoring the cached ETag.

        Returns a :class:`BuiltinToolsConfigResult`; raises
        :class:`BuiltinToolsFetchError` only for the loud-fail cases
        (network/5xx/malformed — status FETCH_FAILED is what the raise
        represents; callers must fail the wake). 404 returns
        LEGACY_NO_ENDPOINT for the caller's fallback path.
        """
        etag, cached_tools = self.snapshot()
        headers = {
            "Authorization": f"Bearer {self.agent_credential}",
            # authenticateAgent requires the agent id header alongside the
            # bearer token (same as prompt_fetch) — without it every fetch
            # 401s and chat wakes die loudly.
            "X-Skquad-Agent-ID": self.agent_id,
            "Accept": "application/json",
        }
        if etag:
            headers["If-None-Match"] = etag
        req = urllib.request.Request(
            self.control_plane_url + TOOLS_ENDPOINT, headers=headers, method="GET"
        )
        try:
            with self._opener(req) as response:
                status = getattr(response, "status", None) or response.getcode()
                body = response.read()
                resp_etag = _header(response, "ETag")
        except urllib.error.HTTPError as exc:
            status = exc.code
            body = b""
            resp_etag = _header(exc, "ETag")
        except Exception as exc:  # URLError, socket errors, DNS, TLS, ...
            raise BuiltinToolsFetchError(
                f"builtin-tools fetch failed: control-plane unreachable at "
                f"{self.control_plane_url + TOOLS_ENDPOINT}: {exc}"
            ) from exc

        if status == 304:
            if not cached_tools:
                raise BuiltinToolsFetchError(
                    "builtin-tools fetch returned 304 but no cached tools exist; "
                    "refusing to run with a degraded tool set"
                )
            LOGGER.info("builtin-tools cache hit (304); reusing %d cached tools", len(cached_tools))
            return BuiltinToolsConfigResult(OK, cached_tools, etag or resp_etag)
        if status == 404:
            LOGGER.info(
                "control-plane has no builtin-tools endpoint (404); "
                "builtin_tools_source=legacy"
            )
            return BuiltinToolsConfigResult(LEGACY_NO_ENDPOINT)
        if 500 <= status <= 599:
            raise BuiltinToolsFetchError(
                f"builtin-tools fetch failed: control-plane returned HTTP {status} "
                f"at {self.control_plane_url + TOOLS_ENDPOINT} (no silent fallback)"
            )
        if status != 200:
            raise BuiltinToolsFetchError(
                f"builtin-tools fetch failed: unexpected HTTP {status} at "
                f"{self.control_plane_url + TOOLS_ENDPOINT}"
            )
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise BuiltinToolsFetchError(
                "builtin-tools fetch failed: response body is not valid JSON"
            ) from exc
        tools = payload.get("tools") if isinstance(payload, dict) else None
        if not isinstance(tools, list):
            raise BuiltinToolsFetchError(
                "builtin-tools fetch failed: response is missing a 'tools' list"
            )
        self.store(resp_etag, tools)
        LOGGER.info("builtin-tools fetched: %d tools (etag=%s)", len(tools), resp_etag or "none")
        return BuiltinToolsConfigResult(OK, tools, resp_etag)


def builtin_tools_enabled(env=None) -> bool:
    """Kill switch for builtin tools (SKQUAD_BUILTIN_TOOLS_ENABLED, default TRUE).

    Disabled only when the value is explicitly falsy ("false", "0", "no",
    case-insensitive, whitespace-tolerant). Anything else — including
    unset — keeps builtin tools enabled. Read at call time from the live
    environment so operators can kill the switch without a redeploy.
    """
    if env is None:
        env = os.environ
    return env.get("SKQUAD_BUILTIN_TOOLS_ENABLED", "true").strip().lower() not in (
        "false",
        "0",
        "no",
    )


def effective_tools(result: BuiltinToolsConfigResult) -> list:
    """The tool list to use for a fetch result.

    OK -> the fetched tools. Anything else (LEGACY_NO_ENDPOINT,
    KILL_SWITCHED, FETCH_FAILED) -> empty list; the caller decides its
    fallback or fail-loud path.
    """
    if result.status == OK:
        return list(result.tools)
    return []


def _header(response: object, name: str) -> str:
    headers = getattr(response, "headers", None)
    if headers is None:
        return ""
    try:
        value = headers.get(name)
    except Exception:  # pragma: no cover - defensive for odd fakes
        return ""
    return value or ""
