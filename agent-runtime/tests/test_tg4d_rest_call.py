"""TG-4d (S-264): synthetic rest_call integration tests.

Covers the wake-time registration contract (fail-closed), the gateway
handler mapping, grant rejection without a network call, credential
non-leakage into logs, and the rest-aware prompt line. Fake tokens only.
"""

from __future__ import annotations

import base64
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
    RuntimeResource,
    ToolCall,
    load_bootstrap_config,
    resource_prompt_line,
    runtime_resource,
)
from skquad_runtime.synthetic_tools import (  # noqa: E402
    RestCallTool,
    build_rest_call_tools,
    build_synthetic_tools,
)


# Same prompt-fetcher stub pattern as tests/test_runtime.py (WP6 makes the
# composed-prompt fetch mandatory; these tests do not run the prompt HTTP
# layer). Guard against double-import under unittest discover.
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

FAKE_CRED = "FAKE-AGENT-CRED"
GATEWAY_URL = "http://gateway.test:8080"


def rest_payload(resource_id: str, name: str, methods: list[str], paths: list[str]) -> dict:
    """Mimics the agentRuntimeResource JSON the CP publishes for a rest
    grant with a TG-4 tools surface (rest_tool_surface.go shape)."""
    return {
        "resource_type": "rest",
        "resource_id": resource_id,
        "name": name,
        "description": "REST API",
        "manifest": {},
        "tools": [
            {
                "name": "rest_call",
                "description": "Call the registered REST resource via gateway",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "resource_id": {"type": "string", "const": resource_id},
                        "method": {"type": "string", "enum": methods},
                        "path": {"type": "string"},
                        "headers": {
                            "type": "object",
                            "additionalProperties": {"type": "string"},
                        },
                        "body": {"type": "string"},
                    },
                    "required": ["resource_id", "method", "path"],
                    "additionalProperties": False,
                },
                "constraints": {
                    "methods": methods,
                    "path_allow": paths,
                    "path_deny": ["/admin/**"],
                    "max_request_bytes": 65536,
                    "max_response_bytes": 262144,
                    "rate_per_min": 60,
                    "allowed_headers": ["accept", "content-type"],
                },
            }
        ],
    }


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


def gateway_config(tmp: str, gateway_url: str = GATEWAY_URL) -> "object":
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


def rest_call_tool(resources, config, opener=None) -> RestCallTool:
    tools = build_rest_call_tools(resources, config)
    assert tools, "expected a rest_call tool to be built"
    tool = tools[0]
    if opener is not None:
        tool._opener = opener
    return tool


class SyntheticToolRegistrationTest(unittest.TestCase):
    def test_no_rest_grant_tool_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [runtime_resource(skill_payload())]
            self.assertEqual(build_synthetic_tools(resources, config), [])

    def test_rest_grant_without_gateway_env_absent(self):
        # Fail-closed: grants exist but SKQUAD_TOOL_GATEWAY_URL is unset.
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp, gateway_url="")
            self.assertEqual(config.tool_gateway_url, "")
            resources = [runtime_resource(rest_payload("r-1", "API", ["GET"], ["/x"]))]
            self.assertEqual(build_synthetic_tools(resources, config), [])

    def test_rest_resource_without_tools_array_absent(self):
        # A rest grant that publishes no tools array unlocks nothing.
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            bare = runtime_resource(
                {
                    "resource_type": "rest",
                    "resource_id": "r-bare",
                    "name": "Bare",
                    "manifest": {},
                }
            )
            self.assertEqual(build_synthetic_tools([bare], config), [])

    def test_grant_plus_env_single_tool_with_id_enum_and_method_union(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [
                runtime_resource(rest_payload("r-1", "GitHub", ["GET", "POST"], ["/repos/**"])),
                runtime_resource(rest_payload("r-2", "Linear", ["GET", "PUT"], ["/issues/**"])),
                runtime_resource(skill_payload()),
            ]
            tools = build_synthetic_tools(resources, config)
            self.assertEqual(len(tools), 1, "exactly ONE synthetic rest_call tool")
            schema = tools[0].tools()[0]["function"]
            self.assertEqual(schema["name"], "rest_call")
            params = schema["parameters"]
            self.assertEqual(params["properties"]["resource_id"]["enum"], ["r-1", "r-2"])
            # Union across the granted resources' published enums, no dupes.
            self.assertEqual(
                params["properties"]["method"]["enum"], ["GET", "POST", "PUT"]
            )
            self.assertEqual(
                params["required"], ["resource_id", "method", "path"]
            )
            self.assertIs(params["additionalProperties"], False)

    def test_load_bootstrap_config_tool_gateway_url(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            self.assertEqual(config.tool_gateway_url, GATEWAY_URL)
            config_no = gateway_config(tmp, gateway_url="")
            self.assertEqual(config_no.tool_gateway_url, "")


class TaskPathCompositionTest(unittest.TestCase):
    def _run_task(self, resources, capture):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)

            def completion(**kwargs):
                capture.append(kwargs)
                return {"choices": [{"message": {"content": "Done."}}]}

            handler = LiteLLMTaskHandler(resources=resources, completion=completion)
            task = _rt_module.RuntimeTask(
                id="task-1",
                squad_id="squad-1",
                title="T",
                description="",
                status="in-progress",
                assignee_agent_id="agent-1",
            )
            return handler.handle_task(task, config)

    def test_task_tool_list_contains_rest_call_exactly_once(self):
        calls: list = []
        resources = [
            runtime_resource(rest_payload("r-1", "GitHub", ["GET"], ["/repos/**"])),
            runtime_resource(rest_payload("r-2", "Linear", ["GET"], ["/issues/**"])),
            runtime_resource(skill_payload()),
        ]
        result = self._run_task(resources, calls)
        self.assertEqual(result.status, "in-review")
        tools = calls[0].get("tools") or []
        names = [t["function"]["name"] for t in tools]
        self.assertEqual(names.count("rest_call"), 1)

    def test_task_tool_list_without_rest_grant_has_no_rest_call(self):
        calls: list = []
        result = self._run_task([runtime_resource(skill_payload())], calls)
        self.assertEqual(result.status, "in-review")
        tools = calls[0].get("tools") or []
        names = [t["function"]["name"] for t in tools]
        self.assertNotIn("rest_call", names)


class ChatPathCompositionTest(unittest.TestCase):
    class FakeCP:
        def __init__(self, resources, fail=False) -> None:
            self._resources = resources
            self._fail = fail

        def list_resources(self):
            if self._fail:
                raise RuntimeError("control plane down")
            return self._resources

    def _chat_plugins(self, resources, fail=False):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            handler = LLMMessageHandler(client=self.FakeCP(resources, fail))
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

    def test_chat_path_registers_rest_call(self):
        plugins, tools = self._chat_plugins(
            [runtime_resource(rest_payload("r-1", "GitHub", ["GET"], ["/repos/**"]))]
        )
        names = [p.name for p in plugins]
        self.assertEqual(names.count("rest_call"), 1)
        schemas = [t["function"]["name"] for t in tools]
        self.assertIn("rest_call", schemas)

    def test_chat_path_fail_closed_on_resource_fetch_error(self):
        plugins, tools = self._chat_plugins(
            [runtime_resource(rest_payload("r-1", "GitHub", ["GET"], ["/repos/**"]))],
            fail=True,
        )
        names = [p.name for p in plugins]
        self.assertNotIn("rest_call", names)

    def test_chat_dispatch_routes_rest_call_to_synthetic_handler(self):
        # invoke_plugin_tool must route by name to the synthetic plugin.
        plugins, _ = self._chat_plugins(
            [runtime_resource(rest_payload("r-1", "GitHub", ["GET"], ["/repos/**"]))]
        )
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = CapturingOpener(
                _FakeResponse({"status": 200, "headers": {}, "bodyB64": "e30=", "truncated": False})
            )
            for p in plugins:
                if p.name == "rest_call":
                    p._opener = opener
            result = _rt_module.invoke_plugin_tool(
                ToolCall(id="c-1", name="rest_call", arguments={"resource_id": "r-1", "method": "GET", "path": "/repos/x"}),
                config,
                plugins,
            )
            self.assertTrue(result.ok)
            self.assertEqual(len(opener.calls), 1)


def make_tool(tmp: str, opener):
    config = gateway_config(tmp)
    resources = [
        runtime_resource(rest_payload("r-1", "GitHub", ["GET", "POST"], ["/repos/**"]))
    ]
    return rest_call_tool(resources, config, opener), config


class RestCallHandlerTest(unittest.TestCase):
    def _tool(self, tmp, opener):
        return make_tool(tmp, opener)

    def test_handler_sends_correct_request_and_maps_body(self):
        with tempfile.TemporaryDirectory() as tmp:
            body = json.dumps({"ok": True}).encode("utf-8")
            opener = CapturingOpener(
                _FakeResponse(
                    {
                        "status": 200,
                        "headers": {"content-type": "application/json"},
                        "bodyB64": base64.b64encode(body).decode(),
                        "truncated": False,
                    }
                )
            )
            tool, config = self._tool(tmp, opener)
            result = tool.invoke(
                ToolCall(
                    id="c-1",
                    name="rest_call",
                    arguments={
                        "resource_id": "r-1",
                        "method": "GET",
                        "path": "/repos/acme/x",
                        "headers": {"accept": "application/json"},
                    },
                ),
                config,
            )
            self.assertTrue(result.ok)
            self.assertIn("HTTP 200", result.content)
            self.assertIn('{"ok": true}', result.content)
            self.assertNotIn("[truncated]", result.content)
            req = opener.calls[0]
            self.assertEqual(req.full_url, f"{GATEWAY_URL}/v1/rest/r-1")
            self.assertEqual(req.get_header("Authorization"), f"Bearer {FAKE_CRED}")
            self.assertEqual(req.get_header("X-skquad-agent-id"), "agent-1")
            self.assertEqual(req.get_header("Content-type"), "application/json")
            sent = json.loads(req.data.decode("utf-8"))
            self.assertEqual(sent["method"], "GET")
            self.assertEqual(sent["path"], "/repos/acme/x")
            self.assertEqual(sent["headers"], {"accept": "application/json"})
            self.assertNotIn("body", sent, "unset keys must be omitted")

    def test_truncated_flag_surfaces_marker(self):
        with tempfile.TemporaryDirectory() as tmp:
            opener = CapturingOpener(
                _FakeResponse(
                    {
                        "status": 200,
                        "headers": {},
                        "bodyB64": base64.b64encode(b"partial").decode(),
                        "truncated": True,
                    }
                )
            )
            tool, config = self._tool(tmp, opener)
            result = tool.invoke(
                ToolCall(
                    id="c-1",
                    name="rest_call",
                    arguments={"resource_id": "r-1", "method": "GET", "path": "/repos/x"},
                ),
                config,
            )
            self.assertTrue(result.ok)
            self.assertIn("[truncated]", result.content)
            self.assertIn("partial", result.content)

    def test_unknown_resource_id_rejected_without_calling_gateway(self):
        with tempfile.TemporaryDirectory() as tmp:
            opener = ExplodingOpener()
            tool, config = self._tool(tmp, opener)
            result = tool.invoke(
                ToolCall(
                    id="c-1",
                    name="rest_call",
                    arguments={"resource_id": "r-evil", "method": "GET", "path": "/x"},
                ),
                config,
            )
            self.assertFalse(result.ok)
            self.assertIn("resource not granted", result.content)
            self.assertFalse(opener.called)

    def test_gateway_rejection_carries_status_and_error_body(self):
        with tempfile.TemporaryDirectory() as tmp:
            err_body = json.dumps(
                {"error": {"code": "denied", "message": "path not allowed"}}
            ).encode("utf-8")
            http_err = error.HTTPError(
                "http://gateway.test:8080/v1/rest/r-1",
                403,
                "Forbidden",
                {},  # type: ignore[arg-type]
                io.BytesIO(err_body),
            )

            def opener(req):
                raise http_err

            tool, config = self._tool(tmp, opener)
            result = tool.invoke(
                ToolCall(
                    id="c-1",
                    name="rest_call",
                    arguments={"resource_id": "r-1", "method": "GET", "path": "/admin/x"},
                ),
                config,
            )
            # The LLM must SEE the rejection: ok=True carrying status+body.
            self.assertTrue(result.ok)
            self.assertIn("403", result.content)
            self.assertIn("denied", result.content)
            self.assertIn("path not allowed", result.content)

    def test_transport_failure_is_not_ok(self):
        with tempfile.TemporaryDirectory() as tmp:
            def opener(req):
                raise OSError("connection refused")

            tool, config = self._tool(tmp, opener)
            with self.assertLogs("skquad_runtime.synthetic_tools", level="WARNING") as logs:
                result = tool.invoke(
                    ToolCall(
                        id="c-1",
                        name="rest_call",
                        arguments={"resource_id": "r-1", "method": "GET", "path": "/x"},
                    ),
                    config,
                )
            self.assertFalse(result.ok)
            self.assertNotIn(FAKE_CRED, result.content)
            self.assertNotIn(FAKE_CRED, "\n".join(logs.output))

    def test_malformed_gateway_response_is_not_ok(self):
        with tempfile.TemporaryDirectory() as tmp:
            class BadResponse(_FakeResponse):
                def read(self):
                    return b"not-json"

            tool, config = self._tool(tmp, CapturingOpener(BadResponse({})))
            result = tool.invoke(
                ToolCall(
                    id="c-1",
                    name="rest_call",
                    arguments={"resource_id": "r-1", "method": "GET", "path": "/x"},
                ),
                config,
            )
            self.assertFalse(result.ok)


class CredentialRedactionTest(unittest.TestCase):
    def test_credential_never_appears_in_captured_logs(self):
        with tempfile.TemporaryDirectory() as tmp:
            opener = CapturingOpener(
                _FakeResponse({"status": 200, "headers": {}, "bodyB64": "e30=", "truncated": False})
            )
            tool, config = make_tool(tmp, opener)
            with self.assertLogs("skquad_runtime", level="DEBUG") as logs:
                result = tool.invoke(
                    ToolCall(
                        id="c-1",
                        name="rest_call",
                        arguments={"resource_id": "r-1", "method": "GET", "path": "/repos/x"},
                    ),
                    config,
                )
            self.assertTrue(result.ok)
            self.assertNotIn(FAKE_CRED, "\n".join(logs.output))
            self.assertNotIn(FAKE_CRED, result.content)


class ResourcePromptLineTest(unittest.TestCase):
    def test_rest_line_renders_id_and_constraints(self):
        resource = runtime_resource(
            rest_payload("f07d5eb2-1234", "GitHub API - Ross", ["GET"], ["/repos/**"])
        )
        line = resource_prompt_line(resource)
        self.assertIn("rest | GitHub API - Ross", line)
        self.assertIn("id=f07d5eb2-1234", line)
        self.assertIn("methods=[GET]", line)
        self.assertIn("paths=[/repos/**]", line)

    def test_other_resource_types_unchanged(self):
        skill = runtime_resource(skill_payload())
        self.assertEqual(
            resource_prompt_line(skill),
            "- skill | My Skill | does things | package=builtin://myskill",
        )
        tool_res = RuntimeResource(
            resource_type="tool",
            resource_id="tool-1",
            name="echo",
            description="Echo messages",
            endpoint="plugin://echo",
            manifest={"package_ref": "builtin://echo"},
        )
        self.assertEqual(
            resource_prompt_line(tool_res),
            "- tool | echo | Echo messages | plugin://echo | package=builtin://echo",
        )

    def test_rest_line_without_constraints_still_renders_id(self):
        resource = runtime_resource(
            {
                "resource_type": "rest",
                "resource_id": "r-9",
                "name": "Sparse",
                "manifest": {},
                "tools": [{"name": "rest_call"}],
            }
        )
        line = resource_prompt_line(resource)
        self.assertIn("id=r-9", line)
        self.assertNotIn("methods=", line)
        self.assertNotIn("paths=", line)


if __name__ == "__main__":
    unittest.main()
