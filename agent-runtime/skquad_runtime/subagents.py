"""S-160: subagents.

``spawn_subagent`` runs a nested agent loop that INHERITS the parent's
model, grants (tool/plugin set), configuration and composed system
prompt, but starts from an EMPTY context. Only the subagent's final
answer is returned into the parent's context as the tool result, so
long exploration, big tool outputs, and multi-step digging do not burn
the parent's context window.

Guarantees:
- Same model / gateway / virtual key / metering identity as the parent
  (calls carry ``skquad_subagent: true`` in the gateway metadata).
- Same grants: the subagent can only call tools the parent could call.
- Same system prompt: the composed four-tier prompt, byte-identical.
- No recursion: the subagent's tool list excludes ``spawn_subagent``.
- Bounded: ``SKQUAD_SUBAGENT_MAX_TURNS`` (default 12) LLM turns.
- Kill switch: ``SKQUAD_SUBAGENT_ENABLED=false`` removes the tool.
"""

from __future__ import annotations

import os
from typing import Callable, Mapping, Sequence

SUBAGENT_TOOL_NAME = "spawn_subagent"
DEFAULT_SUBAGENT_MAX_TURNS = 12
ENV_SUBAGENT_ENABLED = "SKQUAD_SUBAGENT_ENABLED"
ENV_SUBAGENT_MAX_TURNS = "SKQUAD_SUBAGENT_MAX_TURNS"

# kwargs_factory(messages, child_tools) -> completion kwargs. The parent
# supplies this so the subagent's LLM calls carry exactly the parent's
# gateway/auth/metering shape; the runtime adds the subagent marker.
KwargsFactory = Callable[[list, list], dict]


def subagents_enabled(environ: Mapping[str, str] | None = None) -> bool:
    env = os.environ if environ is None else environ
    raw = env.get(ENV_SUBAGENT_ENABLED, "true").strip().lower()
    return raw not in {"0", "false", "no", "off"}


def subagent_max_turns(environ: Mapping[str, str] | None = None) -> int:
    env = os.environ if environ is None else environ
    raw = env.get(ENV_SUBAGENT_MAX_TURNS, "").strip()
    if not raw:
        return DEFAULT_SUBAGENT_MAX_TURNS
    try:
        return max(1, int(raw))
    except ValueError:
        return DEFAULT_SUBAGENT_MAX_TURNS


def _tool_schema_name(schema: object) -> str:
    if isinstance(schema, Mapping):
        fn = schema.get("function")
        if isinstance(fn, Mapping):
            return str(fn.get("name") or "")
        return str(schema.get("name") or "")
    return ""


class SubagentPlugin:
    """The parent-side machinery for one spawn_subagent capability.

    All parent context (system prompt, completion callable, sibling tool
    list, kwargs factory) is captured at wake time so the subagent
    inherits it exactly.
    """

    name = SUBAGENT_TOOL_NAME

    def __init__(
        self,
        *,
        system_prompt: str,
        completion: Callable[..., object],
        plugins: Sequence[object],
        kwargs_factory: KwargsFactory,
        origin: str,
        max_turns: int | None = None,
    ) -> None:
        self._system_prompt = system_prompt
        self._completion = completion
        self._plugins = list(plugins)
        self._kwargs_factory = kwargs_factory
        self._origin = origin
        self._max_turns = max_turns if max_turns is not None else subagent_max_turns()
        # Child toolset: the parent's tools minus spawn_subagent itself —
        # subagents must not recurse.
        self._child_tools = [
            schema
            for schema in _all_tool_schemas(self._plugins)
            if _tool_schema_name(schema) != SUBAGENT_TOOL_NAME
        ]

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": SUBAGENT_TOOL_NAME,
                    "description": (
                        "Delegate a self-contained sub-task to a fresh subagent. The subagent "
                        "inherits your model, grants, configuration and system prompt but starts "
                        "with an EMPTY context, so heavy exploration does not fill your context "
                        "window. Give it complete, standalone instructions; only its final "
                        "answer returns to you."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task": {
                                "type": "string",
                                "description": (
                                    "Self-contained instructions for the subagent, including "
                                    "everything it needs (no access to this conversation)."
                                ),
                            }
                        },
                        "required": ["task"],
                    },
                },
            }
        ]

    def invoke(self, call: object, config: object) -> object:
        # Late import: runtime.py owns the loop helpers and imports this
        # module lazily; a module-level cycle would break package import.
        from .runtime import (
            ToolResult,
            assistant_message,
            first_message,
            invoke_plugin_tool,
            message_value,
            parse_tool_calls,
            wrap_untrusted,
        )

        arguments = getattr(call, "arguments", None)
        task_text = str((arguments or {}).get("task") or "").strip()
        if not task_text:
            return ToolResult(content="spawn_subagent requires a non-empty 'task' argument", ok=False)

        messages: list[dict[str, object]] = [
            {"role": "system", "content": self._system_prompt},
            {
                "role": "user",
                "content": wrap_untrusted(task_text, "subagent_task", origin=self._origin),
            },
        ]
        last_content = ""
        for _ in range(self._max_turns):
            kwargs = self._kwargs_factory(messages, self._child_tools)
            _mark_subagent_metadata(kwargs)
            try:
                response = self._completion(**kwargs)
            except Exception as exc:  # noqa: BLE001 — surfaced as a failed tool result
                return ToolResult(content=f"subagent LLM call failed: {exc}", ok=False)
            message = first_message(response)
            content = str(message_value(message, "content") or "")
            calls = parse_tool_calls(message)
            if not calls:
                return ToolResult(
                    content=content or "(subagent finished without a final answer)", ok=True
                )
            messages.append(assistant_message(content, calls))
            for child_call in calls:
                child_result = invoke_plugin_tool(child_call, config, self._plugins)
                if not child_result.ok:
                    return ToolResult(content=f"subagent blocked: {child_result.content}", ok=False)
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": child_call.id,
                        "name": child_call.name,
                        "content": wrap_untrusted(
                            child_result.content, "tool_result", tool=child_call.name, subagent="true"
                        ),
                    }
                )
            last_content = content
        return ToolResult(
            content=(last_content + "\n[subagent stopped: max turns reached]").strip(), ok=True
        )


def _all_tool_schemas(plugins: Sequence[object]) -> list[Mapping[str, object]]:
    schemas: list[Mapping[str, object]] = []
    for plugin in plugins:
        try:
            schemas.extend(plugin.tools())  # type: ignore[attr-defined]
        except Exception:  # noqa: BLE001 — a broken plugin must not crash the picker
            continue
    return schemas


def _mark_subagent_metadata(kwargs: dict) -> None:
    """Tag gateway metadata so subagent spend is attributable (S-160)."""
    extra_body = kwargs.get("extra_body")
    if not isinstance(extra_body, dict):
        extra_body = {}
        kwargs["extra_body"] = extra_body
    metadata = extra_body.get("litellm_metadata")
    if not isinstance(metadata, dict):
        metadata = {}
        extra_body["litellm_metadata"] = metadata
    metadata["skquad_subagent"] = True


def maybe_add_subagent_plugin(
    plugins: list,
    tools: list,
    *,
    system_prompt: str,
    completion: Callable[..., object],
    kwargs_factory: KwargsFactory,
    origin: str,
    environ: Mapping[str, str] | None = None,
) -> tuple[list, list]:
    """Return (plugins, tools) with the spawn_subagent capability added
    when enabled. The plugin is appended LAST so it never displaces a
    granted tool of the same iteration order."""
    if not subagents_enabled(environ):
        return plugins, tools
    plugin = SubagentPlugin(
        system_prompt=system_prompt,
        completion=completion,
        plugins=[p for p in plugins if getattr(p, "name", None) != SUBAGENT_TOOL_NAME],
        kwargs_factory=kwargs_factory,
        origin=origin,
    )
    return [*plugins, plugin], [*tools, *plugin.tools()]
