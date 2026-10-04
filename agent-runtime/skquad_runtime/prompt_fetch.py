"""Fetch-at-wake composed prompt client (S-PROMPT WP3, ADR-0011 D4).

The control plane composes the effective four-tier prompt and serves it at
``GET /api/v1/agents/me/prompt`` (agent-credential auth, ``ETag`` =
composition sha256). The runtime fetches it before the first LLM call of
every wake, so a scale-to-zero agent picks up org/squad/agent tier edits
with no redeploy.

Failure policy (ADR-0011 D4 — fail loudly, never a silent degraded
prompt):

- network failure / 5xx / 404 / malformed body -> :class:`PromptFetchError`
  (the wake must fail; the agent cannot claim tasks anyway — same
  dependency).
- Since WP6 there is no legacy env fallback: the control-plane composed-prompt
  endpoint is a hard requirement of the runtime.

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

#: The fetch could not complete trustworthily (network/5xx/404/malformed
#: body). Per ADR-0011 D4 the wake must fail loudly — never substitute a
#: lesser prompt for a fetch that failed. Since WP6 (legacy env removal)
#: there are no transitional LEGACY_* statuses: the composed-prompt
#: endpoint is mandatory.
FETCH_FAILED = "fetch_failed"


class PromptFetchError(RuntimeError):
    """Raised when the composed prompt fetch fails and no fallback is allowed."""


class PromptSource:
    """One fetched composed-prompt result (frozen by convention).

    ``status`` is "ok" when ``prompt`` is populated (other statuses were
    the pre-WP6 transitional values; failures now raise).
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

        Returns a PromptSource with status "ok"; raises
        :class:`PromptFetchError` for every failure (network/5xx/404/
        malformed). Since WP6 there is no legacy fallback path.
        """
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
        _raise_for_bad_status(status, self.base_url + PROMPT_ENDPOINT)
        prompt, sha = _parse_prompt_payload(body)
        self.cache.store(resp_etag or f'"{sha}"', prompt, sha)
        LOGGER.info("prompt fetched: prompt_source=composed sha=%s", sha[:12])
        return PromptSource("ok", prompt, sha, resp_etag)


def _parse_prompt_payload(body: bytes) -> tuple[str, str]:
    """Parse the composed-prompt JSON body into (prompt, sha)."""
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
    return str(prompt), str(sha)


def _raise_for_bad_status(status: int, url: str) -> None:
    """Raise the specific PromptFetchError for non-OK/non-304 statuses."""
    if status == 404:
        raise PromptFetchError(
            f"prompt fetch failed: control-plane has no composed-prompt endpoint at "
            f"{url} (WP6: the legacy env fallback is removed; "
            "upgrade the control plane — ADR-0011 D4)"
        )
    if 500 <= status <= 599:
        raise PromptFetchError(
            f"prompt fetch failed: control-plane returned HTTP {status} "
            f"at {url} (ADR-0011 D4: no silent fallback)"
        )
    if status != 200:
        raise PromptFetchError(
            f"prompt fetch failed: unexpected HTTP {status} at {url}"
        )


def _header(response: object, name: str) -> str:
    headers = getattr(response, "headers", None)
    if headers is None:
        return ""
    try:
        value = headers.get(name)
    except Exception:  # pragma: no cover - defensive for odd fakes
        return ""
    return value or ""
