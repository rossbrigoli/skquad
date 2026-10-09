"""LiteLLM proxy callbacks for Skquad metering ingestion."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from typing import Any, Mapping
from urllib import error, request

from litellm.integrations.custom_logger import CustomLogger

from vision_gate import apply_vision_gate


LOGGER = logging.getLogger(__name__)


def _default_vision_resolver(model_name: str):
    """Production capability resolver: read the deployment's model_info.

    Best-effort against the live litellm router. Any failure or a missing
    ``supports_vision`` key returns None (passthrough) — the gateway must
    never strip a request it cannot confidently classify. The runtime is
    the primary gate; this is defense-in-depth.
    """
    if not model_name:
        return None
    try:
        import litellm  # local import: keeps the stdlib-only test harness working

        info = litellm.get_model_info(model_name)
    except Exception:  # noqa: BLE001 - unknown ⇒ passthrough
        return None
    if not isinstance(info, dict) or "supports_vision" not in info:
        return None
    return bool(info.get("supports_vision"))


class SkquadMeteringCallback(CustomLogger):
    def __init__(self, vision_resolver=None) -> None:
        super().__init__()
        # S-200: injectable so tests drive the gate without a live router.
        # Defaults to the litellm model_info resolver (best-effort).
        self._vision_resolver = vision_resolver or _default_vision_resolver

    async def async_pre_call_hook(self, user_api_key_dict, cache, data, call_type):
        """S-200: strip image parts headed to a non-vision model.

        Defense-in-depth behind the runtime gate. Never raises: any error
        resolving capability or rewriting the body leaves the request
        untouched (passthrough) rather than breaking the LLM call.
        """
        try:
            model = str((data or {}).get("model") or "")
            capable = self._vision_resolver(model)
            data, stripped = apply_vision_gate(data, capable)
            if stripped:
                LOGGER.warning(
                    "skquad vision gate: stripped %s image part(s) for non-vision model=%s",
                    stripped,
                    model,
                )
        except Exception as exc:  # noqa: BLE001 - never fail the proxied call
            LOGGER.warning("skquad vision gate: skipped (error: %s)", exc)
        return data

    async def async_log_success_event(self, kwargs, response_obj, start_time, end_time):
        await send_metering_event("success", kwargs, response_obj, "")

    async def async_log_failure_event(self, kwargs, response_obj, start_time, end_time):
        # ADR-0010 D7: an upstream 401/403 must fall back AND alert, so a
        # dead provider credential never runs silently on fallback.
        alert = "upstream_auth_failure" if is_upstream_auth_failure(kwargs, response_obj) else ""
        if alert:
            metadata = callback_metadata(kwargs)
            LOGGER.error(
                "ALERT skquad: upstream provider auth failure (401/403) agent=%s squad=%s model=%s - "
                "traffic is failing over; rotate the provider credential (ADR-0010 D7)",
                metadata.get("skquad_agent_id", ""),
                metadata.get("skquad_squad_id", ""),
                kwargs.get("model", ""),
            )
        await send_metering_event("failure", kwargs, response_obj, safe_error(response_obj), alert)

    async def async_post_call_failure_hook(
        self,
        request_data: dict,
        original_exception: Exception,
        user_api_key_dict: Any,
        traceback_str: str | None = None,
    ):
        """S-265: replace the client-facing error with a displayable,
        classified OpenAI-compatible error (see the module comment above
        build_failure_error for what litellm provides vs what we add).

        Fail-safe: any error in our transformation returns None so the
        client still gets litellm's default error rather than a crash.
        """
        try:
            spec = build_failure_error(request_data or {}, original_exception)
            return make_proxy_exception(spec)
        except Exception as hook_exc:  # noqa: BLE001 - never mask the original failure
            LOGGER.warning("skquad failure-shaping hook skipped: %s", hook_exc)
            return None


async def send_metering_event(
    status: str,
    kwargs: Mapping[str, Any],
    response_obj: Any,
    error_message: str,
    alert: str = "",
) -> None:
    endpoint = os.environ.get("SKQUAD_CONTROL_PLANE_URL", "").rstrip("/")
    token = os.environ.get("SKQUAD_GATEWAY_CALLBACK_TOKEN", "").strip()
    if not endpoint or not token:
        LOGGER.debug("skquad metering callback disabled: missing endpoint or token")
        return

    metadata = callback_metadata(kwargs)
    agent_id = str(metadata.get("skquad_agent_id") or "")
    squad_id = str(metadata.get("skquad_squad_id") or "")
    if not agent_id or not squad_id:
        LOGGER.warning("skquad metering callback skipped: missing agent/squad metadata")
        return

    requested = str(kwargs.get("model") or metadata.get("model") or "")
    # Best available signal. LiteLLM's body `model` can be restamped to match the
    # client under router aliases; the deployment-level truth is the opaque
    # x-litellm-model-id response header, which the callback API does not expose.
    # In our architecture primary and fallback are distinct model_list entries, so
    # a fallback-served response is built from the fallback deployment and carries
    # its name. Treat as best-effort, not a guarantee (see ADR-0010 L1/L2).
    served = str(value(response_obj, "model") or requested)
    if served != requested:
        LOGGER.info(
            "skquad metering: served model differs from requested agent=%s requested=%s served=%s",
            agent_id,
            requested,
            served,
        )

    usage = response_usage(response_obj)
    payload = {
        "status": status,
        "agent_id": agent_id,
        "squad_id": squad_id,
        "task_id": str(metadata.get("skquad_task_id") or ""),
        "provider_id": str(metadata.get("skquad_provider_id") or ""),
        "model": requested,
        # ADR-0010 Risk 3: metering must distinguish a fallback-served turn from a
        # primary-served one. kwargs["model"] is what the CLIENT asked for; the
        # served model lives on the response body. Without this, metering.model_used
        # collapses onto the requested model and fallback turns are invisible.
        "model_used": served,
        "input_tokens": int_value(usage.get("prompt_tokens")),
        "output_tokens": int_value(usage.get("completion_tokens")),
        "cost": float_value(kwargs.get("response_cost")),
        "currency": str(metadata.get("currency") or "USD"),
        "error": error_message[:512],
        "alert": alert,
    }
    await asyncio.to_thread(post_json, endpoint + "/api/v1/gateway/metering", token, payload)


def upstream_status_code(kwargs: Mapping[str, Any], response_obj: Any) -> int | None:
    """Best-effort HTTP status of a failed upstream call.

    LiteLLM surfaces the raised exception either as the response_obj of the
    failure callback or under kwargs["exception"]; its exception objects
    carry a status_code attribute.
    """
    for item in (value(kwargs, "exception"), response_obj):
        code = value(item, "status_code")
        try:
            code = int(code)
        except (TypeError, ValueError):
            continue
        if code:
            return code
    return None


def is_upstream_auth_failure(kwargs: Mapping[str, Any], response_obj: Any) -> bool:
    """True for upstream 401/403 — the D7 class that falls back loudly."""
    return upstream_status_code(kwargs, response_obj) in (401, 403)


def callback_metadata(kwargs: Mapping[str, Any]) -> Mapping[str, Any]:
    # LiteLLM carries call metadata in two places: nested under litellm_params and
    # directly on kwargs. Prefer litellm_params, but the top-level copy must remain
    # reachable - short-circuiting on an empty nested dict here silently drops
    # metering events (and therefore billing) for every call that only carries
    # metadata at the top level.
    litellm_params = kwargs.get("litellm_params") or {}
    if isinstance(litellm_params, Mapping):
        metadata = litellm_params.get("metadata")
        if isinstance(metadata, Mapping) and metadata:
            return metadata
    metadata = kwargs.get("metadata") or {}
    if isinstance(metadata, Mapping):
        return metadata
    return {}


def response_usage(response_obj: Any) -> Mapping[str, Any]:
    usage = value(response_obj, "usage") or {}
    if isinstance(usage, Mapping):
        return usage
    return {
        "prompt_tokens": value(usage, "prompt_tokens"),
        "completion_tokens": value(usage, "completion_tokens"),
    }


def post_json(url: str, token: str, payload: Mapping[str, Any]) -> None:
    data = json.dumps(payload).encode("utf-8")
    req = request.Request(
        url,
        data=data,
        method="POST",
        headers={
            "Authorization": "Bearer " + token,
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
    )
    try:
        with request.urlopen(req, timeout=5) as response:
            if response.status < 200 or response.status >= 300:
                LOGGER.warning("skquad metering callback returned status %s", response.status)
    except Exception as exc:  # noqa: BLE001 - telemetry must never fail the LLM call
        # URLError covers most transport failures, but http.client and socket can
        # surface bare OSErrors. Metering is best-effort: losing one event is
        # acceptable, failing the proxied request is not.
        LOGGER.warning("skquad metering callback failed: %s", exc)


def value(item: Any, key: str) -> Any:
    if isinstance(item, Mapping):
        return item.get(key)
    return getattr(item, key, None)


def int_value(item: Any) -> int:
    try:
        return int(item or 0)
    except (TypeError, ValueError):
        return 0


def float_value(item: Any) -> float:
    try:
        return float(item or 0)
    except (TypeError, ValueError):
        return 0.0


def safe_error(item: Any) -> str:
    if item is None:
        return ""
    return str(item)


# ---------------------------------------------------------------------------
# S-265: displayable, classified LLM failure errors
#
# What litellm ALREADY provides (verified against litellm 1.103.2 source):
#   * The proxy serialises failures as OpenAI-compatible JSON —
#     {"error": {"message", "type", "param", "code"}} — via
#     litellm.proxy._types.ProxyException.to_dict().
#   * CustomLogger.async_post_call_failure_hook(request_data,
#     original_exception, user_api_key_dict) may return/raise an
#     HTTPException (ProxyException included) to REPLACE the client-facing
#     error; returning None keeps litellm's default. See the "call hooks"
#     docs: https://docs.litellm.ai/docs/proxy/call_hooks
#   * litellm maps upstream failures to typed exceptions carrying
#     .status_code / .llm_provider / .model:
#     https://docs.litellm.ai/docs/exception_mapping
#
# What we ADD here (litellm gives none of this):
#   * a guaranteed machine-readable classification block under
#     error.provider_specific_fields.skquad (retryable, category, model,
#     provider, request_id) — ProxyException.provider_specific_fields is
#     merged into the error dict by to_dict(), which is the sanctioned
#     injection point for extra fields on the OpenAI error shape;
#   * a clean human-readable message per category;
#   * secret redaction: litellm's default message passes raw upstream text
#     through, which can embed request fragments. We never forward the raw
#     upstream message unredacted/truncated.
# ---------------------------------------------------------------------------

LLM_ERROR_CATEGORIES = (
    "auth",
    "rate_limit",
    "no_credits",
    "timeout",
    "bad_request",
    "provider_error",
    "unknown",
)

# Chat-displayable message per category. Deliberately free of provider
# internals; the technical detail is appended separately, redacted+truncated.
LLM_ERROR_USER_MESSAGES = {
    "auth": (
        "The model provider rejected my credentials (authentication error). "
        "The provider key needs to be rotated by an admin — retrying won't help."
    ),
    "no_credits": (
        "The model provider account is out of credits/balance. "
        "No model calls will succeed until the account is topped up."
    ),
    "rate_limit": (
        "The model provider is rate-limiting us (too many requests). "
        "A retry after a short wait should work."
    ),
    "timeout": (
        "The model provider didn't respond in time (connection timeout). "
        "This is usually transient — a retry may work."
    ),
    "bad_request": (
        "The model provider rejected this request as invalid (bad request). "
        "Retrying the same message won't help."
    ),
    "provider_error": (
        "The model provider had a server-side error (5xx). "
        "A retry after a short wait should work."
    ),
    "unknown": (
        "The model call failed for an unexpected reason. Please retry; "
        "if it keeps happening, check the gateway logs."
    ),
}

# Categories the client may usefully retry. Overridden by the gateway's own
# classification when it is present (skquad.retryable).
LLM_ERROR_RETRYABLE = frozenset({"rate_limit", "timeout", "provider_error"})

# Provider "you owe us money" signals — checked before plain rate-limit so an
# exhausted-credits 429 is distinguishable from a throttling 429.
_CREDITS_RE = re.compile(
    r"credit|billing|payment|balance|insufficient[_ ]funds|out of funds|quota exceeded",
    re.IGNORECASE,
)
_TIMEOUT_RE = re.compile(r"timed? out|timeout|connection (?:error|reset|refused|closed|aborted)", re.IGNORECASE)

# Secret redaction: any of these surviving into a user-facing message is a bug.
# Order matters only for readability; every pattern is applied to the whole text.
_SECRET_PATTERNS = (
    # Bearer values are scrubbed FIRST: the key:value pattern would
    # otherwise consume only the word "Bearer" and strand the token.
    re.compile(r"Bearer\s+[^\s,;'\"]+", re.IGNORECASE),  # Authorization values
    re.compile(
        r"(?i)(api[_-]?key|authorization|access[_-]?token|secret|password)"
        r"\"?\s*[:=]\s*\"?[^\s,;'\"]+"
    ),                                                    # key: value pairs
    re.compile(r"sk-[A-Za-z0-9_\-]{6,}"),                # OpenAI/skquad-style keys
    re.compile(r"AIza[0-9A-Za-z_\-]{8,}"),               # Google API keys
    re.compile(r"gsk_[A-Za-z0-9]{8,}"),                  # Groq keys
    re.compile(r"gh[pousr]_[A-Za-z0-9]{8,}"),            # GitHub tokens
)


def redact_secrets(text: str) -> str:
    """Scrub credential-shaped substrings. Applied before any truncation."""
    redacted = _SECRET_PATTERNS[0].sub("[REDACTED]", text)
    redacted = _SECRET_PATTERNS[1].sub(lambda m: m.group(1) + "=[REDACTED]", redacted)
    for pattern in _SECRET_PATTERNS[2:]:
        redacted = pattern.sub("[REDACTED]", redacted)
    return redacted


def classify_llm_error(exc: BaseException) -> tuple[str, bool]:
    """Map an exception to (category, retryable).

    Status-code driven with keyword fallbacks so it works for litellm typed
    exceptions (which carry .status_code) AND bare exceptions from tests or
    non-litellm failure paths. ``no_credits`` is checked before
    ``rate_limit`` because providers report exhausted billing as 429 too.
    """
    status = getattr(exc, "status_code", None)
    try:
        status = int(status)
    except (TypeError, ValueError):
        status = None
    message = str(exc)
    if status in (401, 403):
        return "auth", False
    if status == 402 or (status == 429 and _CREDITS_RE.search(message)):
        return "no_credits", False
    if status == 429:
        return "rate_limit", True
    if status == 400:
        return "bad_request", False
    if status == 408 or _TIMEOUT_RE.search(message):
        return "timeout", True
    if status is not None and status >= 500:
        return "provider_error", True
    if _CREDITS_RE.search(message):
        return "no_credits", False
    return "unknown", False


def _request_id_from(exc: BaseException) -> str:
    for attr in ("request_id", "request_uuid"):
        value = getattr(exc, attr, None)
        if value:
            return str(value)[:64]
    match = re.search(r"request[_-]id[\"']?\s*[:=]\s*[\"']?([A-Za-z0-9\-]{6,64})", str(exc))
    return match.group(1) if match else ""


def skquad_error_block(request_data: Mapping[str, Any], exc: BaseException, category: str, retryable: bool) -> dict:
    """Machine-readable classification carried alongside the OpenAI error."""
    return {
        "retryable": bool(retryable),
        "category": category,
        "model": str(request_data.get("model") or getattr(exc, "model", "") or ""),
        "provider": str(getattr(exc, "llm_provider", "") or ""),
        "request_id": _request_id_from(exc),
    }


def build_failure_error(request_data: Mapping[str, Any], exc: BaseException) -> dict:
    """Compose the client-facing error pieces (never raw upstream text).

    The human message is the category's friendly text plus a redacted,
    truncated technical detail (≤160 chars) so a chat window can show
    something actionable without leaking request fragments.
    """
    category, retryable = classify_llm_error(exc)
    detail = redact_secrets(str(exc)).strip().replace("\n", " ")
    if len(detail) > 160:
        detail = detail[:157] + "..."
    message = LLM_ERROR_USER_MESSAGES.get(category, LLM_ERROR_USER_MESSAGES["unknown"])
    if detail:
        message = f"{message} (details: {detail})"
    type_by_category = {
        "auth": "authentication_error",
        "bad_request": "invalid_request_error",
        "rate_limit": "rate_limit_error",
        "no_credits": "insufficient_quota",
    }
    status = getattr(exc, "status_code", None)
    try:
        code = str(int(status))
    except (TypeError, ValueError):
        code = "500"
    return {
        "message": message,
        "type": type_by_category.get(category, "api_error"),
        "param": None,
        "code": code,
        "category": category,
        "retryable": retryable,
        "skquad": skquad_error_block(request_data, exc, category, retryable),
    }


def make_proxy_exception(spec: Mapping[str, Any]):
    """Build litellm's ProxyException from a failure spec (lazy import).

    ProxyException is what the proxy re-raises verbatim from the failure
    hook, and its to_dict() merges provider_specific_fields into the
    OpenAI-compatible error object — our skquad block rides along.
    """
    from litellm.proxy._types import ProxyException  # lazy: keeps stdlib-only tests green

    return ProxyException(
        message=spec["message"],
        type=spec["type"],
        param=spec["param"],
        code=spec["code"],
        provider_specific_fields={"skquad": spec["skquad"]},
    )


proxy_handler_instance = SkquadMeteringCallback()
