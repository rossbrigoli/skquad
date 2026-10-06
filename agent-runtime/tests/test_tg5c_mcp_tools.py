"""TG-5 slice C (S-244-series): synthetic mcp_list / mcp_call tests.

Mirrors tests/test_tg4d_rest_call.py. Covers wake-time discovery from the
resource surface (no network), gateway call routing with the agent's own
credential, client-side fail-closed rejection of ungranted resources,
4xx/5xx pass-through semantics, transport failure, the interim
requires_confirmation gate, fail-closed registration, the exactly-one-
instance rule, and the mcp-aware prompt line. Fake tokens only.
"""

from __future__ import annotations

import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from urllib import error

os.environ.setdefault("SKQUAD_BUILTIN_TOOLS_ENABLED", "false")

import skquad_runtime.runtime as _rt_module  # noqa: E402
from skquad_runtime.prompt_fetch import PromptSource as _PromptSource  # noqa: E402
from skquad_runtime.runtime import (  # noqa: E402
    LLMMessageHandler,
    LiteLLMTaskHandler,
    RuntimeMessage,
    ToolCall,
    load_bootstrap_config,
    resource_prompt_line,
    runtime_resource,
)
from skquad_runtime.synthetic_tools import (  # noqa: E402
    McpCallTool,
    McpListTool,
    build_mcp_tools,
    build_synthetic_tools,
)


# Same prompt-fetcher stub pattern as tests/test_tg4d_rest_call.py.
class _StubPromptFetcher:
    def fetch(self):
        return _PromptSource(
            "ok",
            '<skquad_platform trust="platform">stub composed prompt</skquad_platform>',
            "0" * 64,
        )


_stub_prompt_fetcher = _StubPromptFetcher()
if not hasattr(_rt_module.PromptedRuntime, "_REAL_FETCHER_INSTANCE"):
    _rt_module.PromptedRuntime._REAL_FETCHER_INSTANCE = (
        _rt_module.PromptedRuntime._fetcher_instance
    )


def _stubbed_fetcher_instance(self):
    explicit = getattr(self, "_fetcher", None)
    if explicit is not None:
        return explicit
    return _stub_prompt_fetcher


_rt_module.PromptedRuntime._fetcher_instance = _stubbed_fetcher_instance

FAKE_CRED = "FAKE-AGENT-CRED-MCP"
GATEWAY_URL = "http://gateway.test:8080"


def mcp_payload(
    resource_id: str,
    name: str,
    tools_allow: list[str] | None = None,
    confirm_tools: list[str] | None = None,
) -> dict:
    """Mimics the agentRuntimeResource JSON the CP publishes for an mcp
    grant: typed-resource surfacing (endpoint_config + constraints,
    secret-stripped). The allowed-tool list lives in
    ``constraints.tools_allow`` (canonical CP field, validated against
    the registration-time snapshot in mcp_registration.go); the interim
    confirmation flags live in ``constraints.per_tool.<name>``."""
    constraints: dict = {}
    if tools_allow is not None:
        constraints["tools_allow"] = tools_allow
    if confirm_tools:
        constraints["per_tool"] = {
            tool: {"requires_confirmation": True} for tool in confirm_tools
        }
    payload = {
        "resource_type": "mcp",
        "resource_id": resource_id,
        "name": name,
        "description": "MCP server",
        "endpoint": "https://mcp.example.internal",
        "manifest": {},
        "endpoint_config": {"url": "https://mcp.example.internal", "auth_kind": "bearer"},
        "constraints": constraints,
    }
    return payload


def skill_payload(resource_id: str = "skill-1") -> dict:
    return {
        "resource_type": "skill",
        "resource_id": resource_id,
        "name": "My Skill",
        "description": "does things",
        "manifest": {"package_ref": "builtin://myskill"},
    }


class _FakeResponse:
    def __init__(self, payload: dict, status: int = 200) -> None:
        self._raw = json.dumps(payload).encode("utf-8")
        self.status = status

    def read(self) -> bytes:
        return self._raw

    def getcode(self) -> int:
        return self.status

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class CapturingOpener:
    def __init__(self, response: _FakeResponse) -> None:
        self.response = response
        self.calls: list = []

    def __call__(self, req):
        self.calls.append(req)
        return self.response


class ExplodingOpener:
    def __init__(self) -> None:
        self.called = False

    def __call__(self, req):
        self.called = True
        raise AssertionError("gateway must not be called")


def gateway_config(tmp: str, gateway_url: str = GATEWAY_URL):
    env = {
        "SKQUAD_AGENT_ID": "agent-1",
        "SKQUAD_SQUAD_ID": "squad-1",
        "SKQUAD_AGENT_CREDENTIAL_PATH": str(Path(tmp) / "agent"),
        "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH": str(Path(tmp) / "llm-gateway"),
        "SKQUAD_LLM_GATEWAY_URL": "http://llm-gateway",
        "SKQUAD_DEFAULT_MODEL": "model-1",
    }
    if gateway_url:
        env["SKQUAD_TOOL_GATEWAY_URL"] = gateway_url
    Path(tmp, "agent").write_text(FAKE_CRED, encoding="utf-8")
    Path(tmp, "llm-gateway").write_text("virtual-key", encoding="utf-8")
    return load_bootstrap_config(env)


def mcp_tools(resources, config, opener=None) -> tuple[McpListTool, McpCallTool]:
    tools = build_mcp_tools(resources, config)
    assert len(tools) == 2, "expected exactly mcp_list + mcp_call"
    list_tool, call_tool = tools
    assert isinstance(list_tool, McpListTool) and isinstance(call_tool, McpCallTool)
    if opener is not None:
        call_tool._opener = opener
    return list_tool, call_tool


class McpRegistrationTest(unittest.TestCase):
    def test_no_mcp_grant_tools_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [runtime_resource(skill_payload())]
            names = [t.name for t in build_synthetic_tools(resources, config)]
            self.assertNotIn("mcp_list", names)
            self.assertNotIn("mcp_call", names)

    def test_mcp_grant_without_gateway_env_absent(self):
        # Fail-closed: grants exist but SKQUAD_TOOL_GATEWAY_URL is unset.
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp, gateway_url="")
            self.assertEqual(config.tool_gateway_url, "")
            resources = [
                runtime_resource(mcp_payload("m-1", "Srv", ["tool_a"]))
            ]
            self.assertEqual(build_mcp_tools(resources, config), [])

    def test_exactly_one_of_each_with_multiple_mcp_grants(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [
                runtime_resource(mcp_payload("m-1", "One", ["a", "b"])),
                runtime_resource(mcp_payload("m-2", "Two", ["c"])),
                runtime_resource(skill_payload()),
            ]
            tools = build_synthetic_tools(resources, config)
            names = [t.name for t in tools]
            self.assertEqual(names.count("mcp_list"), 1)
            self.assertEqual(names.count("mcp_call"), 1)
            list_schema = next(t.tools()[0]["function"] for t in tools if t.name == "mcp_list")
            call_schema = next(t.tools()[0]["function"] for t in tools if t.name == "mcp_call")
            self.assertEqual(list_schema["parameters"]["properties"]["resource_id"]["enum"], ["m-1", "m-2"])
            self.assertEqual(call_schema["parameters"]["properties"]["resource_id"]["enum"], ["m-1", "m-2"])
            self.assertEqual(call_schema["parameters"]["required"], ["resource_id", "tool"])
            self.assertIs(call_schema["parameters"]["additionalProperties"], False)


class McpListTest(unittest.TestCase):
    def test_lists_allowed_tools_from_wake_data_no_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = ExplodingOpener()
            list_tool, _ = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["search", "fetch"]))],
                config,
                opener,
            )
            result = list_tool.invoke(
                ToolCall(id="c-1", name="mcp_list", arguments={"resource_id": "m-1"}),
                config,
            )
            self.assertTrue(result.ok)
            self.assertIn("search", result.content)
            self.assertIn("fetch", result.content)
            self.assertFalse(opener.called, "mcp_list must never hit the network")

    def test_ungranted_resource_rejected_without_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = ExplodingOpener()
            list_tool, _ = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))], config, opener
            )
            result = list_tool.invoke(
                ToolCall(id="c-1", name="mcp_list", arguments={"resource_id": "m-evil"}),
                config,
            )
            self.assertFalse(result.ok)
            self.assertIn("resource not granted", result.content)
            self.assertFalse(opener.called)

    def test_empty_allowlist_renders_default_deny_note(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            list_tool, _ = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", []))], config, ExplodingOpener()
            )
            result = list_tool.invoke(
                ToolCall(id="c-1", name="mcp_list", arguments={"resource_id": "m-1"}), config
            )
            self.assertTrue(result.ok)
            self.assertIn("no allowed tools", result.content)


class McpCallTest(unittest.TestCase):
    def test_posts_correct_gateway_path_with_agent_credential(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = CapturingOpener(
                _FakeResponse({"content": [{"type": "text", "text": "result"}]})
            )
            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["do_thing"]))],
                config,
                opener,
            )
            result = call_tool.invoke(
                ToolCall(
                    id="c-1",
                    name="mcp_call",
                    arguments={
                        "resource_id": "m-1",
                        "tool": "do_thing",
                        "arguments": {"q": "hello"},
                    },
                ),
                config,
            )
            self.assertTrue(result.ok)
            self.assertIn("result", result.content)
            self.assertEqual(len(opener.calls), 1)
            req = opener.calls[0]
            self.assertEqual(req.full_url, f"{GATEWAY_URL}/v1/mcp/m-1/call")
            self.assertEqual(req.get_header("Authorization"), f"Bearer {FAKE_CRED}")
            self.assertEqual(req.get_header("X-skquad-agent-id"), "agent-1")
            self.assertEqual(req.get_header("Content-type"), "application/json")
            sent = json.loads(req.data.decode("utf-8"))
            self.assertEqual(sent["tool"], "do_thing")
            self.assertEqual(sent["arguments"], {"q": "hello"})

    def test_ungranted_resource_rejected_without_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = ExplodingOpener()
            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))], config, opener
            )
            result = call_tool.invoke(
                ToolCall(
                    id="c-1",
                    name="mcp_call",
                    arguments={"resource_id": "m-evil", "tool": "a", "arguments": {}},
                ),
                config,
            )
            self.assertFalse(result.ok)
            self.assertIn("resource not granted", result.content)
            self.assertFalse(opener.called)

    def test_gateway_4xx_carries_status_and_error_body_ok_true(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            err_body = json.dumps(
                {"error": {"code": "denied", "message": "tool not allowed"}}
            ).encode("utf-8")
            http_err = error.HTTPError(
                f"{GATEWAY_URL}/v1/mcp/m-1/call",
                403,
                "Forbidden",
                {},  # type: ignore[arg-type]
                io.BytesIO(err_body),
            )

            def opener(req):
                raise http_err

            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))], config, opener
            )
            result = call_tool.invoke(
                ToolCall(
                    id="c-1",
                    name="mcp_call",
                    arguments={"resource_id": "m-1", "tool": "a", "arguments": {}},
                ),
                config,
            )
            # The LLM must SEE the rejection: ok=True carrying status+body.
            self.assertTrue(result.ok)
            self.assertIn("403", result.content)
            self.assertIn("denied", result.content)
            self.assertIn("tool not allowed", result.content)

    def test_gateway_5xx_carries_status_ok_true(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            err_body = json.dumps(
                {"error": {"code": "mcp_failed", "message": "upstream exploded"}}
            ).encode("utf-8")
            http_err = error.HTTPError(
                f"{GATEWAY_URL}/v1/mcp/m-1/call",
                502,
                "Bad Gateway",
                {},  # type: ignore[arg-type]
                io.BytesIO(err_body),
            )

            def opener(req):
                raise http_err

            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))], config, opener
            )
            result = call_tool.invoke(
                ToolCall(
                    id="c-1",
                    name="mcp_call",
                    arguments={"resource_id": "m-1", "tool": "a", "arguments": {}},
                ),
                config,
            )
            self.assertTrue(result.ok)
            self.assertIn("502", result.content)
            self.assertIn("mcp_failed", result.content)

    def test_transport_failure_not_ok(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)

            def opener(req):
                raise OSError("connection refused")

            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))], config, opener
            )
            with self.assertLogs("skquad_runtime.synthetic_tools", level="WARNING") as logs:
                result = call_tool.invoke(
                    ToolCall(
                        id="c-1",
                        name="mcp_call",
                        arguments={"resource_id": "m-1", "tool": "a", "arguments": {}},
                    ),
                    config,
                )
            self.assertFalse(result.ok)
            self.assertNotIn(FAKE_CRED, result.content)
            self.assertNotIn(FAKE_CRED, "\n".join(logs.output))

    def test_malformed_gateway_response_not_ok(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)

            class BadResponse(_FakeResponse):
                def read(self):
                    return b"not-json"

            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))],
                config,
                CapturingOpener(BadResponse({})),
            )
            result = call_tool.invoke(
                ToolCall(
                    id="c-1",
                    name="mcp_call",
                    arguments={"resource_id": "m-1", "tool": "a", "arguments": {}},
                ),
                config,
            )
            self.assertFalse(result.ok)

    def test_requires_confirmation_tool_never_executed(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = ExplodingOpener()
            _, call_tool = mcp_tools(
                [
                    runtime_resource(
                        mcp_payload(
                            "m-1", "Srv", ["safe", "dangerous"], confirm_tools=["dangerous"]
                        )
                    )
                ],
                config,
                opener,
            )
            result = call_tool.invoke(
                ToolCall(
                    id="c-1",
                    name="mcp_call",
                    arguments={"resource_id": "m-1", "tool": "dangerous", "arguments": {}},
                ),
                config,
            )
            self.assertFalse(result.ok)
            self.assertIn("requires admin confirmation", result.content)
            self.assertIn("not executed", result.content)
            self.assertFalse(opener.called, "confirmation-required tools must not reach the gateway")

    def test_credential_never_appears_in_logs(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = CapturingOpener(_FakeResponse({"content": []}))
            _, call_tool = mcp_tools(
                [runtime_resource(mcp_payload("m-1", "Srv", ["a"]))], config, opener
            )
            with self.assertLogs("skquad_runtime", level="DEBUG") as logs:
                result = call_tool.invoke(
                    ToolCall(
                        id="c-1",
                        name="mcp_call",
                        arguments={"resource_id": "m-1", "tool": "a", "arguments": {"x": 1}},
                    ),
                    config,
                )
            self.assertTrue(result.ok)
            self.assertNotIn(FAKE_CRED, "\n".join(logs.output))
            self.assertNotIn(FAKE_CRED, result.content)


class McpTurnPathTest(unittest.TestCase):
    class FakeCP:
        def __init__(self, resources) -> None:
            self._resources = resources

        def list_resources(self):
            return self._resources

    def _chat_plugins(self, resources):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            handler = LLMMessageHandler(client=self.FakeCP(resources))
            message = RuntimeMessage(
                id="m-1",
                from_type="user",
                from_id="user-1",
                to_agent_id="agent-1",
                squad_id="squad-1",
                message_type="chat",
                payload={"message": "hi"},
                status="pending",
                correlation_id="",
                attempts=0,
                max_attempts=3,
            )
            plugins, tools, early = handler._compose_chat_plugins(
                message, config, None, lambda **kw: None, "vk", "model-1"
            )
            self.assertIsNone(early)
            return plugins, tools

    def test_chat_path_registers_mcp_tools_exactly_once(self):
        plugins, tools = self._chat_plugins(
            [
                runtime_resource(mcp_payload("m-1", "One", ["a"])),
                runtime_resource(mcp_payload("m-2", "Two", ["b"])),
            ]
        )
        names = [p.name for p in plugins]
        self.assertEqual(names.count("mcp_list"), 1)
        self.assertEqual(names.count("mcp_call"), 1)
        schemas = [t["function"]["name"] for t in tools]
        self.assertIn("mcp_list", schemas)
        self.assertIn("mcp_call", schemas)

    def test_task_path_registers_mcp_tools_exactly_once(self):
        calls: list = []

        def completion(**kwargs):
            calls.append(kwargs)
            return {"choices": [{"message": {"content": "Done."}}]}

        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [
                runtime_resource(mcp_payload("m-1", "One", ["a"])),
                runtime_resource(mcp_payload("m-2", "Two", ["b"])),
                runtime_resource(skill_payload()),
            ]
            handler = LiteLLMTaskHandler(resources=resources, completion=completion)
            task = _rt_module.RuntimeTask(
                id="task-1",
                squad_id="squad-1",
                title="T",
                description="",
                status="in-progress",
                assignee_agent_id="agent-1",
            )
            result = handler.handle_task(task, config)
            self.assertEqual(result.status, "in-review")
            tools = calls[0].get("tools") or []
            names = [t["function"]["name"] for t in tools]
            self.assertEqual(names.count("mcp_list"), 1)
            self.assertEqual(names.count("mcp_call"), 1)

    def test_task_path_without_mcp_grant_has_no_mcp_tools(self):
        calls: list = []

        def completion(**kwargs):
            calls.append(kwargs)
            return {"choices": [{"message": {"content": "Done."}}]}

        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            handler = LiteLLMTaskHandler(
                resources=[runtime_resource(skill_payload())], completion=completion
            )
            task = _rt_module.RuntimeTask(
                id="task-1",
                squad_id="squad-1",
                title="T",
                description="",
                status="in-progress",
                assignee_agent_id="agent-1",
            )
            handler.handle_task(task, config)
            tools = calls[0].get("tools") or []
            names = [t["function"]["name"] for t in tools]
            self.assertNotIn("mcp_list", names)
            self.assertNotIn("mcp_call", names)


class McpPromptLineTest(unittest.TestCase):
    def test_mcp_line_renders_id_and_tools(self):
        resource = runtime_resource(
            mcp_payload("ab12cd34-99", "Weather MCP", ["get_forecast", "get_alerts"])
        )
        line = resource_prompt_line(resource)
        self.assertIn("mcp | Weather MCP", line)
        self.assertIn("id=ab12cd34-99", line)
        self.assertIn("tools=[get_forecast,get_alerts]", line)

    def test_mcp_line_without_tools_allow_omits_tools(self):
        resource = runtime_resource(mcp_payload("m-9", "Sparse", None))
        line = resource_prompt_line(resource)
        self.assertIn("id=m-9", line)
        self.assertNotIn("tools=", line)

    def test_mcp_line_empty_allowlist_omits_tools(self):
        resource = runtime_resource(mcp_payload("m-8", "Empty", []))
        line = resource_prompt_line(resource)
        self.assertIn("id=m-8", line)
        self.assertNotIn("tools=", line)


if __name__ == "__main__":
    unittest.main()
