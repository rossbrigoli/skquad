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
        await send_metering_event("failure", kwargs, response_obj, safe_error(response_obj))


async def send_metering_event(status: str, kwargs: Mapping[str, Any], response_obj: Any, error_message: str) -> None:
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

    usage = response_usage(response_obj)
    payload = {
        "status": status,
        "agent_id": agent_id,
        "squad_id": squad_id,
        "task_id": str(metadata.get("skquad_task_id") or ""),
        "provider_id": str(metadata.get("skquad_provider_id") or ""),
        "model": str(kwargs.get("model") or metadata.get("model") or ""),
        "input_tokens": int_value(usage.get("prompt_tokens")),
        "output_tokens": int_value(usage.get("completion_tokens")),
        "cost": float_value(kwargs.get("response_cost")),
        "currency": str(metadata.get("currency") or "USD"),
        "error": error_message[:512],
    }
    await asyncio.to_thread(post_json, endpoint + "/api/v1/gateway/metering", token, payload)


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
