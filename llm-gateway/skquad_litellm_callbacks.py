"""LiteLLM proxy callbacks for Skquad metering ingestion."""

from __future__ import annotations

import asyncio
import json
import logging
import os
from typing import Any, Mapping
from urllib import error, request

from litellm.integrations.custom_logger import CustomLogger


LOGGER = logging.getLogger(__name__)


class SkquadMeteringCallback(CustomLogger):
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


proxy_handler_instance = SkquadMeteringCallback()
