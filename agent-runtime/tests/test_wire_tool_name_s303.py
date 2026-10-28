"""S-303 follow-up: provider-safe tool wire names.

The board.* builtins (S-292 / ADR-0016) use dotted names internally.
OpenAI's Responses API (and chat/completions) reject function names that
do not match ``^[A-Za-z0-9_-]+$`` - a '.' 400s the whole request
("Invalid 'tools[N].name'"), which is exactly what killed a wake on an
OpenAI-routed model (gpt-6.1-sol via /v1/responses).

The runtime keeps the canonical dotted name in the registry / config /
grants / tool.name and sanitizes only what crosses to the provider,
mapping back on dispatch. These tests lock both boundaries.
"""

import unittest

from skquad_runtime.builtin_tools import _BUILTIN_REGISTRY
from skquad_runtime.runtime import (
    ToolCall,
    ToolResult,
    _wire_schema,
    invoke_plugin_tool,
    wire_tool_name,
)


class _DottedPlugin:
    """Minimal RuntimePlugin with a configurable (possibly dotted) name."""

    def __init__(self, name):
        self.name = name
        self.calls = []

    def tools(self):
        return [
            {
                "type": "function",
                "function": {
                    "name": self.name,
                    "description": "a tool",
                    "parameters": {"type": "object", "properties": {}},
                },
            }
        ]

    def invoke(self, call, config):  # noqa: ARG002 - config unused by the fake
        self.calls.append(call)
        return ToolResult(content=f"invoked:{self.name}", ok=True)


class WireToolNameTests(unittest.TestCase):
    def test_maps_dots_and_other_invalid_chars(self):
        self.assertEqual(wire_tool_name("board.create_task"), "board_create_task")
        self.assertEqual(wire_tool_name("board.request_changes"), "board_request_changes")
        self.assertEqual(wire_tool_name("a.b:c d/e"), "a_b_c_d_e")

    def test_provider_safe_names_pass_through_unchanged(self):
        for name in ("exec", "web_fetch", "sessions_list", "spawn_subagent", "browser"):
            self.assertEqual(wire_tool_name(name), name)

    def test_every_registered_tool_name_maps_to_safe_wire_name(self):
        # Locks the invariant for the whole registry: whatever the internal
        # name, the wire name OpenAI sees is always pattern-valid.
        for name in _BUILTIN_REGISTRY:
            self.assertRegex(wire_tool_name(name), r"^[A-Za-z0-9_-]+$")


class WireSchemaTests(unittest.TestCase):
    def test_rewrites_dotted_function_name_without_mutating_source(self):
        schema = {"type": "function", "function": {"name": "board.claim", "parameters": {}}}
        out = _wire_schema(schema)
        self.assertEqual(out["function"]["name"], "board_claim")
        # the plugin's own schema dict is left untouched
        self.assertEqual(schema["function"]["name"], "board.claim")

    def test_passthrough_when_name_already_safe(self):
        schema = {"type": "function", "function": {"name": "exec", "parameters": {}}}
        self.assertIs(_wire_schema(schema), schema)

    def test_passthrough_for_nonstandard_schema(self):
        schema = {"type": "custom", "foo": "bar"}
        self.assertIs(_wire_schema(schema), schema)


class DispatchRoundTripTests(unittest.TestCase):
    def test_wire_named_call_resolves_to_dotted_plugin(self):
        plugin = _DottedPlugin("board.claim")
        result = invoke_plugin_tool(
            ToolCall(id="c1", name="board_claim", arguments={}), None, [plugin]
        )
        self.assertTrue(result.ok)
        self.assertEqual(result.content, "invoked:board.claim")
        self.assertEqual(len(plugin.calls), 1)
        self.assertEqual(plugin.calls[0].name, "board.claim")

    def test_dotted_call_also_resolves_if_model_echoes_it(self):
        # Defensive: if a provider ever echoes the dotted form back, the
        # same sanitization on both sides still matches.
        plugin = _DottedPlugin("board.claim")
        result = invoke_plugin_tool(
            ToolCall(id="c2", name="board.claim", arguments={}), None, [plugin]
        )
        self.assertTrue(result.ok)
        self.assertEqual(result.content, "invoked:board.claim")

    def test_exact_safe_name_still_resolves(self):
        plugin = _DottedPlugin("exec")
        result = invoke_plugin_tool(
            ToolCall(id="c3", name="exec", arguments={}), None, [plugin]
        )
        self.assertTrue(result.ok)
        self.assertEqual(result.content, "invoked:exec")

    def test_unknown_tool_still_rejected(self):
        result = invoke_plugin_tool(
            ToolCall(id="c4", name="no_such_tool", arguments={}),
            None,
            [_DottedPlugin("exec")],
        )
        self.assertFalse(result.ok)


if __name__ == "__main__":
    unittest.main()
