"""S-160: subagent tests.

The subagent loop is exercised with fake completions and a fake granted
plugin — no live gateway. Coverage: inheritance (system prompt, model,
kwargs/metadata), recursion guard, turn cap, kill switch, and the
task-path wiring.
"""

import os
import unittest
from unittest import mock

from skquad_runtime.runtime import ToolCall, ToolResult
from skquad_runtime.subagents import (
    SUBAGENT_TOOL_NAME,
    SubagentPlugin,
    maybe_add_subagent_plugin,
    subagent_max_turns,
    subagents_enabled,
)

PARENT_PROMPT = "PARENT COMPOSED PROMPT"


class EchoPlugin:
    name = "echo"

    def __init__(self):
        self.calls = []

    def tools(self):
        return [
            {
                "type": "function",
                "function": {
                    "name": "echo",
                    "parameters": {"type": "object", "properties": {"text": {"type": "string"}}},
                },
            }
        ]

    def invoke(self, call, config):
        self.calls.append(call)
        return ToolResult(content="echo:" + str(call.arguments.get("text", "")), ok=True)


def make_plugin(completion, plugins=None, max_turns=5, captured=None):
    def kwargs_factory(messages, child_tools):
        if captured is not None:
            captured.append({"messages": list(messages), "tools": list(child_tools)})
        kwargs = {
            "model": "test-model",
            "messages": messages,
            "extra_body": {"litellm_metadata": {"skquad_agent_id": "agent-1"}},
        }
        if child_tools:
            kwargs["tools"] = child_tools
        return kwargs

    return SubagentPlugin(
        system_prompt=PARENT_PROMPT,
        completion=completion,
        plugins=plugins if plugins is not None else [EchoPlugin()],
        kwargs_factory=kwargs_factory,
        origin="task:task-1",
        max_turns=max_turns,
    )


def tool_call_response(name, args, call_id="c1"):
    return {
        "choices": [
            {
                "message": {
                    "content": "",
                    "tool_calls": [
                        {
                            "id": call_id,
                            "type": "function",
                            "function": {"name": name, "arguments": args},
                        }
                    ],
                }
            }
        ]
    }


def content_response(text):
    return {"choices": [{"message": {"content": text}}]}


class SubagentPluginTest(unittest.TestCase):
    def test_nested_loop_returns_final_answer(self):
        echo = EchoPlugin()
        responses = [tool_call_response("echo", {"text": "hello"}), content_response("sub answer")]

        def completion(**kwargs):
            return responses.pop(0)

        plugin = make_plugin(completion, plugins=[echo])
        result = plugin.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "do it"}), None)
        self.assertTrue(result.ok)
        self.assertEqual(result.content, "sub answer")
        self.assertEqual(len(echo.calls), 1)

    def test_inherits_system_prompt_and_model(self):
        captured = []
        responses = [content_response("done")]

        def completion(**kwargs):
            return responses.pop(0)

        plugin = make_plugin(completion, captured=captured)
        plugin.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "t"}), None)
        self.assertEqual(len(captured), 1)
        messages = captured[0]["messages"]
        self.assertEqual(messages[0]["role"], "system")
        self.assertEqual(messages[0]["content"], PARENT_PROMPT)
        self.assertEqual(captured[0]["messages"][1]["role"], "user")
        self.assertIn("subagent_task", messages[1]["content"])  # trust-labelled task

    def test_gateway_metadata_marked_subagent(self):
        captured = []
        plugin = make_plugin(lambda **kw: content_response("ok"), captured=captured)
        plugin.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "t"}), None)
        metadata = captured[0]["messages"]  # touch to keep shape obvious
        self.assertTrue(metadata)
        # The factory's kwargs got the marker injected in-place:
        # re-run to inspect the actual dict via a fresh factory capture.
        captured2 = []

        def kwargs_factory(messages, child_tools):
            captured2.append({"extra_body": {"litellm_metadata": {"skquad_agent_id": "a"}}})
            return captured2[-1]

        plugin2 = SubagentPlugin(
            system_prompt="p",
            completion=lambda **kw: content_response("ok"),
            plugins=[],
            kwargs_factory=kwargs_factory,
            origin="task:t",
            max_turns=2,
        )
        plugin2.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "t"}), None)
        self.assertTrue(captured2[0]["extra_body"]["litellm_metadata"]["skquad_subagent"])

    def test_child_tools_exclude_spawn_subagent(self):
        plugin = make_plugin(lambda **kw: content_response("ok"))
        child = plugin._child_tools
        names = [t["function"]["name"] for t in child]
        self.assertIn("echo", names)
        self.assertNotIn(SUBAGENT_TOOL_NAME, names)

    def test_empty_task_rejected(self):
        plugin = make_plugin(lambda **kw: content_response("ok"))
        result = plugin.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "  "}), None)
        self.assertFalse(result.ok)
        self.assertIn("non-empty", result.content)

    def test_turn_cap_stops_loop(self):
        # Completion always asks for another tool call → must stop at max_turns.
        plugin = make_plugin(lambda **kw: tool_call_response("echo", {"text": "x"}), max_turns=3)
        result = plugin.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "t"}), None)
        self.assertTrue(result.ok)
        self.assertIn("max turns reached", result.content)

    def test_blocked_child_tool_propagates_failure(self):
        class BadPlugin:
            name = "bad"

            def tools(self):
                return [{"type": "function", "function": {"name": "bad"}}]

            def invoke(self, call, config):
                return ToolResult(content="nope", ok=False)

        plugin = make_plugin(
            lambda **kw: tool_call_response("bad", {}),
            plugins=[BadPlugin()],
            max_turns=2,
        )
        result = plugin.invoke(ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "t"}), None)
        self.assertFalse(result.ok)
        self.assertIn("subagent blocked", result.content)


class KillSwitchTest(unittest.TestCase):
    def test_disabled_env_removes_capability(self):
        plugins, tools = [], []
        out_plugins, out_tools = maybe_add_subagent_plugin(
            plugins,
            tools,
            system_prompt="p",
            completion=lambda **kw: content_response("ok"),
            kwargs_factory=lambda m, t: {"messages": m},
            origin="task:t",
            environ={ "SKQUAD_SUBAGENT_ENABLED": "false"},
        )
        self.assertEqual(out_plugins, [])
        self.assertEqual(out_tools, [])
        self.assertFalse(subagents_enabled({"SKQUAD_SUBAGENT_ENABLED": "false"}))

    def test_enabled_by_default(self):
        self.assertTrue(subagents_enabled({}))
        out_plugins, out_tools = maybe_add_subagent_plugin(
            [],
            [],
            system_prompt="p",
            completion=lambda **kw: content_response("ok"),
            kwargs_factory=lambda m, t: {"messages": m},
            origin="task:t",
            environ={},
        )
        self.assertEqual(len(out_plugins), 1)
        self.assertEqual(out_tools[0]["function"]["name"], SUBAGENT_TOOL_NAME)

    def test_max_turns_env(self):
        self.assertEqual(subagent_max_turns({}), 12)
        self.assertEqual(subagent_max_turns({"SKQUAD_SUBAGENT_MAX_TURNS": "4"}), 4)
        self.assertEqual(subagent_max_turns({"SKQUAD_SUBAGENT_MAX_TURNS": "junk"}), 12)


class TaskPathWiringTest(unittest.TestCase):
    def test_task_handler_exposes_spawn_subagent(self):
        """handle_task must offer spawn_subagent and route it to the sub loop."""
        os.environ["SKQUAD_BUILTIN_TOOLS_ENABLED"] = "false"
        os.environ.pop("SKQUAD_SUBAGENT_ENABLED", None)
        with mock.patch(
            "skquad_runtime.runtime.PromptedRuntime.system_prompt", return_value=PARENT_PROMPT
        ):
            from skquad_runtime.runtime import LiteLLMTaskHandler, RuntimeTask

            seen = []
            responses = [
                tool_call_response(SUBAGENT_TOOL_NAME, {"task": "dig in"}, call_id="p1"),
                content_response("sub final"),  # subagent's only turn
                content_response("SKQUAD_STATUS: done\nparent wraps up"),
            ]

            def completion(**kwargs):
                seen.append(kwargs)
                return responses.pop(0)

            handler = LiteLLMTaskHandler(
                plugins=[EchoPlugin()], completion=completion, discover_resources=False, model="m"
            )
            task = RuntimeTask(
                id="task-1",
                squad_id="squad-1",
                title="t",
                description="d",
                status="in-progress",
                assignee_agent_id="agent-1",
                execution_id="e",
                fencing_token="f",
            )
            config = _task_config()
            result = handler.handle_task(task, config)
            self.assertEqual(result.status, "done")
            # Parent turn 1 must have advertised spawn_subagent.
            tool_names = [t["function"]["name"] for t in seen[0].get("tools", [])]
            self.assertIn(SUBAGENT_TOOL_NAME, tool_names)
            # Subagent call reused the parent's system prompt.
            self.assertEqual(seen[1]["messages"][0]["content"], PARENT_PROMPT)
            self.assertTrue(seen[1]["extra_body"]["litellm_metadata"]["skquad_subagent"])


def _task_config():
    import tempfile
    from pathlib import Path

    from skquad_runtime.runtime import load_bootstrap_config

    tmp = tempfile.mkdtemp()
    credential = Path(tmp) / "agent"
    credential.write_text("credential", encoding="utf-8")
    virtual = Path(tmp) / "llm-gateway"
    virtual.write_text("virtual-key", encoding="utf-8")
    env = {
        "SKQUAD_AGENT_ID": "agent-1",
        "SKQUAD_SQUAD_ID": "squad-1",
        "SKQUAD_AGENT_CREDENTIAL_PATH": str(credential),
        "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH": str(virtual),
        "SKQUAD_LLM_GATEWAY_URL": "http://gateway.invalid",
        "SKQUAD_CONTROL_PLANE_URL": "http://cp.invalid",
        "SKQUAD_DEFAULT_MODEL": "m",
        "SKQUAD_MAX_LLM_STEPS": "5",
    }
    return load_bootstrap_config(env)


class TransparencyCaptureTest(unittest.TestCase):
    """S-163: thread/turn/step capture on the ToolResult details."""

    def test_thread_turns_and_steps_captured(self):
        echo = EchoPlugin()
        responses = [
            tool_call_response("echo", {"text": "hello"}),
            content_response("final"),
        ]

        def completion(**kwargs):
            return responses.pop(0)

        plugin = make_plugin(completion, plugins=[echo])
        result = plugin.invoke(
            ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "do it"}), None
        )
        self.assertTrue(result.ok)
        sub = result.details["subagent"]
        self.assertEqual(sub["turns"], 2)
        self.assertEqual(sub["steps"], ["echo"])
        roles = [e["role"] for e in sub["thread"]]
        self.assertEqual(roles, ["user", "assistant", "tool"])
        self.assertEqual(sub["thread"][0]["content"], "do it")
        self.assertEqual(sub["thread"][1]["tool_calls"][0]["name"], "echo")
        self.assertEqual(sub["thread"][2]["name"], "echo")
        self.assertTrue(sub["thread"][2]["ok"])
        self.assertEqual(sub["thread"][2]["content"], "echo:hello")

    def test_thread_items_size_capped(self):
        class BigPlugin:
            name = "big"

            def tools(self):
                return [{"type": "function", "function": {"name": "big"}}]

            def invoke(self, call, config):
                return ToolResult(content="Z" * 50000, ok=True)

        responses = [tool_call_response("big", {"q": "N" * 9000}), content_response("f")]

        def completion(**kwargs):
            return responses.pop(0)

        plugin = make_plugin(completion, plugins=[BigPlugin()])
        result = plugin.invoke(
            ToolCall(id="s1", name=SUBAGENT_TOOL_NAME, arguments={"task": "t" * 100000}), None
        )
        thread = result.details["subagent"]["thread"]
        for entry in thread:
            self.assertLessEqual(len(str(entry)), 2100)
        total = sum(len(str(e)) for e in thread)
        self.assertLessEqual(total, 24000 + 400)

    def test_cap_thread_keeps_newest_and_marks_drops(self):
        from skquad_runtime.subagents import _cap_thread

        thread = [{"role": "user", "content": "TASK"}] + [
            {"role": "tool", "name": "t", "ok": True, "content": "x" * 3000}
            for _ in range(20)
        ]
        capped = _cap_thread(thread)
        self.assertEqual(capped[0]["content"], "TASK")
        self.assertIn("dropped", str(capped[1]["content"]))
        self.assertLess(len(capped), len(thread))
        self.assertLessEqual(sum(len(str(e)) for e in capped), 24000 + 400)

    def test_chat_log_entry_carries_subagent(self):
        from skquad_runtime.runtime import LLMMessageHandler, assistant_message

        class DetailPlugin:
            name = SUBAGENT_TOOL_NAME

            def tools(self):
                return [{"type": "function", "function": {"name": SUBAGENT_TOOL_NAME}}]

            def invoke(self, call, config):
                return ToolResult(
                    content="final", ok=True, details={"subagent": {"thread": [], "turns": 1, "steps": []}}
                )

        handler = LLMMessageHandler(
            completion=lambda **kw: content_response("x"), plugins=[DetailPlugin()]
        )
        chat_messages: list = []
        log: list = []
        call = ToolCall(id="c1", name=SUBAGENT_TOOL_NAME, arguments={})
        assistant = assistant_message("", [call])
        handler._append_tool_results(chat_messages, assistant, [call], None, log, [DetailPlugin()])
        self.assertEqual(len(log), 1)
        self.assertIn("subagent", log[0])
        self.assertEqual(log[0]["subagent"]["turns"], 1)


if __name__ == "__main__":
    unittest.main()
