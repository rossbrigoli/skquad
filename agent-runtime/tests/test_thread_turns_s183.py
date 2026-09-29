"""S-183: agent working turns stream into the task thread.

The task page must show the agent's work-in-progress, not only the final
summary. The handler pushes each assistant narration that accompanies a
tool call to the control plane (via the per-task sink run_task_once
attaches); the final answer still arrives through the completion event,
and the step-limit exit is now explicitly labelled as a cut-off.
"""

import json
import tempfile
import unittest
from pathlib import Path

import skquad_runtime.runtime as _rt_module
from skquad_runtime.prompt_fetch import PromptSource as _PromptSource
from skquad_runtime.runtime import (
    ControlPlaneClient,
    LiteLLMTaskHandler,
    ToolResult,
    load_bootstrap_config,
)

# Same composed-prompt stub as tests/test_runtime.py (S-147: the fetch is
# mandatory; these tests don't drive the real HTTP layer).
class _StubPromptFetcher:
    def fetch(self):
        return _PromptSource(
            "ok",
            '<skquad_platform trust="platform">stub composed prompt</skquad_platform>',
            "0" * 64,
        )


_stub_prompt_fetcher = _StubPromptFetcher()
if not hasattr(_rt_module.PromptedRuntime, "_REAL_FETCHER_INSTANCE"):
    _rt_module.PromptedRuntime._REAL_FETCHER_INSTANCE = _rt_module.PromptedRuntime._fetcher_instance


def _stubbed_fetcher_instance(self):
    explicit = getattr(self, "_fetcher", None)
    if explicit is not None:
        return explicit
    return _stub_prompt_fetcher


_rt_module.PromptedRuntime._fetcher_instance = _stubbed_fetcher_instance


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
                    "description": "Echo a message.",
                    "parameters": {
                        "type": "object",
                        "properties": {"message": {"type": "string"}},
                        "required": ["message"],
                    },
                },
            }
        ]

    def invoke(self, call, _config):
        self.calls.append(dict(call.arguments))
        return ToolResult(content=f"echo: {call.arguments['message']}")


def task_config(tmp):
    credential = Path(tmp) / "agent"
    credential.write_text("credential", encoding="utf-8")
    virtual_key = Path(tmp) / "llm-gateway"
    virtual_key.write_text("virtual-key", encoding="utf-8")
    return load_bootstrap_config(
        {
            "SKQUAD_AGENT_ID": "agent-1",
            "SKQUAD_SQUAD_ID": "squad-1",
            "SKQUAD_AGENT_CREDENTIAL_PATH": str(credential),
            "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH": str(virtual_key),
            "SKQUAD_LLM_GATEWAY_URL": "http://gateway",
            "SKQUAD_DEFAULT_MODEL": "model-1",
            "SKQUAD_BUILTIN_TOOLS_ENABLED": "false",
        }
    )


def fake_task(task_id):
    from skquad_runtime.runtime import RuntimeTask

    return RuntimeTask(
        id=task_id,
        squad_id="squad-1",
        title="Task",
        description="",
        status="in-progress",
        assignee_agent_id="agent-1",
        execution_id=f"exec-{task_id}",
        worker_id="worker-1",
        fencing_token=f"fence-{task_id}",
        lease_expires_at="2026-08-28T02:00:00Z",
    )


def fake_completion(content):
    return {"choices": [{"message": {"content": content}}]}


def fake_tool_completion(call_id, name, arguments, content=""):
    return {
        "choices": [
            {
                "message": {
                    "content": content,
                    "tool_calls": [
                        {
                            "id": call_id,
                            "type": "function",
                            "function": {
                                "name": name,
                                "arguments": json.dumps(arguments),
                            },
                        }
                    ],
                }
            }
        ]
    }


class ThreadTurnStreamingTests(unittest.TestCase):
    def test_working_narration_streams_to_sink_final_does_not(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = task_config(tmp)
            plugin = EchoPlugin()
            calls = []

            def completion(**kwargs):
                calls.append(kwargs)
                if len(calls) == 1:
                    return fake_tool_completion(
                        "call-1", "echo", {"message": "hi"}, content="Checking the runtime API surface."
                    )
                if len(calls) == 2:
                    return fake_tool_completion(
                        "call-2", "echo", {"message": "again"}, content="Now checking ports."
                    )
                return fake_completion("SKQUAD_STATUS: done\nFinal report here.")

            handler = LiteLLMTaskHandler(plugins=[plugin], completion=completion, discover_resources=False)
            streamed = []
            handler.attach_thread_sink(streamed.append)

            result = handler.handle_task(fake_task("task-1"), config)

            self.assertEqual(result.status, "done")
            # Both working turns streamed, in order; the final answer is
            # NOT streamed (it arrives via the completion event).
            self.assertEqual(streamed, ["Checking the runtime API surface.", "Now checking ports."])

    def test_empty_narration_turn_is_not_streamed(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = task_config(tmp)
            plugin = EchoPlugin()
            calls = []

            def completion(**kwargs):
                calls.append(kwargs)
                if len(calls) == 1:
                    return fake_tool_completion("call-1", "echo", {"message": "x"})  # no content
                return fake_completion("SKQUAD_STATUS: done\nDone.")

            handler = LiteLLMTaskHandler(plugins=[plugin], completion=completion, discover_resources=False)
            streamed = []
            handler.attach_thread_sink(streamed.append)

            result = handler.handle_task(fake_task("task-1"), config)

            self.assertEqual(result.status, "done")
            self.assertEqual(streamed, [])

    def test_step_limit_exit_is_labelled_as_cut_off(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = task_config(tmp)
            plugin = EchoPlugin()

            def completion(**kwargs):
                return fake_tool_completion(
                    "call-1", "echo", {"message": "x"}, content="Now the key question: still working."
                )

            handler = LiteLLMTaskHandler(
                plugins=[plugin], completion=completion, discover_resources=False, max_steps=2
            )
            streamed = []
            handler.attach_thread_sink(streamed.append)

            result = handler.handle_task(fake_task("task-1"), config)

            self.assertEqual(result.status, "in-review")
            self.assertIn("Stopped after 2 steps without finishing", result.summary)
            self.assertIn("Now the key question: still working.", result.summary)
            # The narration itself was still streamed.
            self.assertEqual(streamed, ["Now the key question: still working."] * 2)

    def test_handler_without_sink_still_works(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = task_config(tmp)
            plugin = EchoPlugin()
            calls = []

            def completion(**kwargs):
                calls.append(kwargs)
                if len(calls) == 1:
                    return fake_tool_completion("call-1", "echo", {"message": "x"}, content="working")
                return fake_completion("SKQUAD_STATUS: done\nDone.")

            handler = LiteLLMTaskHandler(plugins=[plugin], completion=completion, discover_resources=False)
            result = handler.handle_task(fake_task("task-1"), config)
            self.assertEqual(result.status, "done")


class ControlPlaneThreadPushTests(unittest.TestCase):
    def test_append_task_thread_swallows_failures(self):
        def failing_opener(_request):
            raise OSError("connection refused")

        client = ControlPlaneClient(
            "http://control-plane", "agent-1", "credential", opener=failing_opener
        )
        # Must not raise — thread visibility is best-effort.
        client.append_task_thread("task-1", "a working turn")

    def test_append_task_thread_posts_message_body(self):
        seen = {}

        class FakeResponse:
            status = 204

            def read(self):
                return b""

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

        def ok_opener(request):
            seen["url"] = request.full_url
            seen["body"] = json.loads(request.data.decode("utf-8"))
            return FakeResponse()

        client = ControlPlaneClient(
            "http://control-plane", "agent-1", "credential", opener=ok_opener
        )
        client.append_task_thread("task-9", "checking ports")
        self.assertEqual(seen["url"], "http://control-plane/api/v1/agents/me/tasks/task-9/thread")
        self.assertEqual(seen["body"], {"message": "checking ports"})


if __name__ == "__main__":
    unittest.main()
