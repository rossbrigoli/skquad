"""Fetch-at-wake composed prompt client (S-PROMPT WP3, ADR-0011 D4).

The control plane composes the effective four-tier prompt and serves it at
``GET /api/v1/agents/me/prompt`` (agent-credential auth, ``ETag`` =
composition sha256). The runtime fetches it before the first LLM call of
every wake, so a scale-to-zero agent picks up org/squad/agent tier edits
with no redeploy.

Failure policy (ADR-0011 D4 — fail loudly, never a silent degraded
prompt):

- network failure / 5xx  -> :class:`PromptFetchError` (the wake must fail;
  the agent cannot claim tasks anyway — same dependency).
- 404 (control plane predates the endpoint) or the feature flag
  ``SKQUAD_PROMPT_FETCH_ENABLED=false`` -> the caller falls back to the
  legacy env path and logs ``prompt_source=env_legacy``.

ETag caching is per-process: the composed sha doubles as the validator, so
repeat fetches within one process lifetime send ``If-None-Match`` and a
304 reuses the cached body (no full prompt re-send).
"""

from __future__ import annotations

import json
import logging
import threading
import urllib.error
import urllib.request

LOGGER = logging.getLogger(__name__)

PROMPT_ENDPOINT = "/api/v1/agents/me/prompt"

#: The fetch could not complete trustworthily (network/5xx/malformed body).
#: Per ADR-0011 D4 the wake must fail loudly — never substitute a lesser
#: prompt for a fetch that failed.
FETCH_FAILED = "fetch_failed"

#: The control plane has no composed-prompt endpoint (404). Transitional:
#: the caller may use the legacy env path.
LEGACY_NO_ENDPOINT = "legacy_no_endpoint"

#: Feature flag ``SKQUAD_PROMPT_FETCH_ENABLED`` is false. Transitional
#: fallback to the legacy env path (removed in WP6).
LEGACY_FLAG_DISABLED = "legacy_flag_disabled"


class PromptFetchError(RuntimeError):
    """Raised when the composed prompt fetch fails and no fallback is allowed."""


class PromptSource:
    """One fetched composed-prompt result (frozen by convention).

    ``status`` is one of FETCH_FAILED / LEGACY_NO_ENDPOINT /
    LEGACY_FLAG_DISABLED, or "ok" when ``prompt`` is populated.
    ``sha`` is the control-plane composition sha256 (recorded in the run
    journal); ``etag`` is the validator to send on the next fetch.
    """

    __slots__ = ("status", "prompt", "sha", "etag")

    def __init__(self, status: str, prompt: str = "", sha: str = "", etag: str = "") -> None:
        self.status = status
        self.prompt = prompt
        self.sha = sha
        self.etag = etag or f'"{sha}"'

    def __repr__(self) -> str:  # never include the prompt body in logs
        return f"PromptSource(status={self.status!r}, sha={self.sha!r})"


class ComposedPromptCache:
    """Process-lifetime ETag cache for the composed prompt.

    Thread-safe: the lease-heartbeat thread and the task thread must never
    interleave a half-updated cache entry.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._etag: str = ""
        self._prompt: str = ""
        self._sha: str = ""

    @property
    def etag(self) -> str:
        with self._lock:
            return self._etag

    def store(self, etag: str, prompt: str, sha: str) -> None:
        with self._lock:
            self._etag = etag
            self._prompt = prompt
            self._sha = sha

    def snapshot(self) -> tuple[str, str, str]:
        with self._lock:
            return self._etag, self._prompt, self._sha


def prompt_fetch_enabled(environ: Mapping[str, str] | None = None) -> bool:
    """Transitional feature flag (SKQUAD_PROMPT_FETCH_ENABLED, default true).

    Read at call time from the live environment so operators can pin an
    agent to the legacy env prompt path without a config-schema change.
    """
    import os

    env = environ if environ is not None else os.environ
    raw = env.get("SKQUAD_PROMPT_FETCH_ENABLED")
    if raw is None:
        return True
    return raw.strip().lower() not in ("0", "false", "no", "off")


class PromptFetcher:
    """ETag-aware fetcher for ``GET /api/v1/agents/me/prompt``.

    ``opener`` mirrors :class:`ControlPlaneClient`'s injectable opener so
    tests can mock the HTTP layer without a network.
    """

    def __init__(
        self,
        base_url: str,
        agent_id: str,
        credential: str,
        opener=None,
        cache: ComposedPromptCache | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.agent_id = agent_id
        self.credential = credential
        self._opener = opener or urllib.request.urlopen
        self.cache = cache if cache is not None else ComposedPromptCache()

    @classmethod
    def from_runtime(cls, runtime) -> "PromptFetcher":
        """Build from a :class:`PromptedRuntime` (credential read at use time)."""
        credential = runtime._agent_credential()
        if not credential:
            raise PromptFetchError("agent credential is not loaded; cannot fetch the composed prompt")
        base_url = runtime.config.control_plane_url
        if not base_url:
            raise PromptFetchError("SKQUAD_CONTROL_PLANE_URL is required to fetch the composed prompt")
        return cls(base_url, runtime.config.agent_id, credential)

    def fetch(self) -> PromptSource:
        """Fetch the composed prompt, honoring the cached ETag.

        Returns a PromptSource; raises :class:`PromptFetchError` only for
        the loud-fail cases (network/5xx/malformed). 404 and flag-off
        return LEGACY_* statuses for the caller's fallback path.
        """
        if not prompt_fetch_enabled():
            LOGGER.info("prompt fetch disabled by flag; prompt_source=env_legacy")
            return PromptSource(LEGACY_FLAG_DISABLED)
        etag, cached_prompt, cached_sha = self.cache.snapshot()
        headers = {
            "Authorization": f"Bearer {self.credential}",
            "X-Skquad-Agent-ID": self.agent_id,
            "Accept": "application/json",
        }
        if etag:
            headers["If-None-Match"] = etag
        req = urllib.request.Request(self.base_url + PROMPT_ENDPOINT, headers=headers, method="GET")
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
            raise PromptFetchError(
                f"prompt fetch failed: control-plane unreachable at "
                f"{self.base_url + PROMPT_ENDPOINT}: {exc}"
            ) from exc

        if status == 304:
            if not cached_prompt:
                raise PromptFetchError(
                    "prompt fetch returned 304 but no cached prompt exists; "
                    "refusing to run with a degraded prompt (ADR-0011 D4)"
                )
            LOGGER.info("prompt cache hit (304); reusing composed prompt sha=%s", cached_sha)
            return PromptSource("ok", cached_prompt, cached_sha, etag or resp_etag)
        if status == 404:
            LOGGER.info("control-plane has no composed-prompt endpoint (404); prompt_source=env_legacy")
            return PromptSource(LEGACY_NO_ENDPOINT)
        if 500 <= status <= 599:
            raise PromptFetchError(
                f"prompt fetch failed: control-plane returned HTTP {status} "
                f"at {self.base_url + PROMPT_ENDPOINT} (ADR-0011 D4: no silent fallback)"
            )
        if status != 200:
            raise PromptFetchError(
                f"prompt fetch failed: unexpected HTTP {status} at "
                f"{self.base_url + PROMPT_ENDPOINT}"
            )
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise PromptFetchError("prompt fetch failed: response body is not valid JSON") from exc
        prompt = payload.get("prompt") if isinstance(payload, dict) else None
        sha = payload.get("sha256") if isinstance(payload, dict) else None
        if not prompt or not sha:
            raise PromptFetchError(
                "prompt fetch failed: response is missing 'prompt' or 'sha256'"
            )
        self.cache.store(resp_etag or f'"{sha}"', prompt, sha)
        LOGGER.info("prompt fetched: prompt_source=composed sha=%s", sha[:12])
        return PromptSource("ok", prompt, sha, resp_etag)


def _header(response: object, name: str) -> str:
    headers = getattr(response, "headers", None)
    if headers is None:
        return ""
    try:
        value = headers.get(name)
    except Exception:  # pragma: no cover - defensive for odd fakes
        return ""
    return value or ""
