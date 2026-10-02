"""S-200: gateway-side vision gate (defense-in-depth).

The runtime is the primary vision gate — it only embeds image content
parts when the bound model reports ``supports_vision`` (see
agent-runtime). This module is the SECOND layer: if any caller (an older
runtime, a direct API client) sends image content parts to a model that
is NOT vision-capable, the gateway strips them and substitutes a text
marker so the request degrades gracefully instead of erroring upstream.

The gate is deliberately conservative: it only strips when capability is
explicitly ``False``. An unknown capability (``None``) passes through
unchanged, because silently mangling a request we don't understand is
worse than trusting the upstream. Capability is resolved from the
deployment's ``model_info.supports_vision`` (provisioned by the control
plane), via an injectable resolver so this stays testable without a live
litellm router.

Pure functions only — no litellm import — so the CI stdlib-only harness
exercises the real logic.
"""

from __future__ import annotations

from typing import Any, Callable, Mapping, MutableMapping

OMITTED_MARKER = "[image omitted: model is not vision-capable]"


def message_has_image(content: Any) -> bool:
    """True if a message content is a part list carrying an image_url."""
    if not isinstance(content, list):
        return False
    for part in content:
        if isinstance(part, Mapping) and part.get("type") == "image_url":
            return True
    return False


def _strip_parts(content: list) -> tuple[list, int]:
    """Replace image_url parts with a single text marker; keep the rest."""
    out: list = []
    stripped = 0
    marker_added = False
    for part in content:
        if isinstance(part, Mapping) and part.get("type") == "image_url":
            stripped += 1
            if not marker_added:
                out.append({"type": "text", "text": OMITTED_MARKER})
                marker_added = True
            continue
        out.append(part)
    return out, stripped


def apply_vision_gate(data: Any, vision_capable: bool | None) -> tuple[Any, int]:
    """Strip image parts from a request body when the model is not capable.

    Returns ``(data, stripped_count)``. ``data`` is mutated in place for
    the message list (matching how litellm hands the body to hooks) and
    also returned for clarity.

    - ``vision_capable`` True or None → no-op (passthrough).
    - ``vision_capable`` False → every image_url part in every message is
      replaced by one text marker per message; the surrounding text parts
      (which still carry the S-194 attachment reference) are preserved.
    """
    if vision_capable is not False:
        return data, 0
    if not isinstance(data, MutableMapping):
        return data, 0
    messages = data.get("messages")
    if not isinstance(messages, list):
        return data, 0

    total_stripped = 0
    new_messages: list = []
    for msg in messages:
        if isinstance(msg, Mapping) and message_has_image(msg.get("content")):
            new_content, stripped = _strip_parts(list(msg["content"]))
            total_stripped += stripped
            updated = dict(msg)
            updated["content"] = new_content
            new_messages.append(updated)
        else:
            new_messages.append(msg)
    data["messages"] = new_messages
    return data, total_stripped


def make_model_info_resolver(
    get_model_info: Callable[[str], Mapping[str, Any] | None],
) -> Callable[[str], bool | None]:
    """Build a capability resolver from a model-info getter.

    ``get_model_info(model_name)`` returns the deployment's model_info
    mapping (or None). The resolver reads ``supports_vision``:
      - present True  → True
      - present False → False (gate active)
      - absent / any error → None (passthrough; unknown is not "incapable")
    """

    def resolve(model_name: str) -> bool | None:
        if not model_name:
            return None
        try:
            info = get_model_info(model_name)
        except Exception:  # noqa: BLE001 - unknown is safer than a wrong strip
            return None
        if not isinstance(info, Mapping) or "supports_vision" not in info:
            return None
        return bool(info.get("supports_vision"))

    return resolve
