"""MIG-6 (S-312): synthetic ssh_exec tool tests.

Covers the wake-time registration contract (fail-closed), the schema
shape, gateway dispatch mapping (200 result passthrough, 202
pending-confirmation surfacing, HTTPError/transport handling), the
client-side grant check, and credential non-leakage. Fake tokens only.
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

from skquad_runtime.runtime import (  # noqa: E402
    ToolCall,
    load_bootstrap_config,
    runtime_resource,
)
from skquad_runtime.synthetic_tools import (  # noqa: E402
    SshExecTool,
    build_ssh_tools,
    build_synthetic_tools,
    granted_ssh_resources,
)

FAKE_CRED = "FAKE-AGENT-CRED"
GATEWAY_URL = "http://gateway.test:8080"


def ssh_payload(resource_id: str = "res-ssh-1") -> dict:
    """Mimics the agentRuntimeResource JSON the CP publishes for an
    ssh grant (ssh_tool_surface.go shape, trimmed)."""
    return {
        "resource_type": "ssh",
        "resource_id": resource_id,
        "name": "Lab Hosts",
        "description": "SSH diagnostic inventory",
        "manifest": {},
        "tools": [
            {
                "name": "ssh_exec",
                "description": "Run one command on an allowlisted host",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "resource_id": {"type": "string", "const": resource_id},
                        "host": {"type": "string"},
                        "command": {"type": "string"},
                        "timeout_seconds": {"type": "integer"},
                    },
                    "required": ["resource_id", "host", "command"],
                    "additionalProperties": False,
                },
                "constraints": {
                    "hosts_allow": ["lab-*"],
                    "hosts_deny": [],
                    "command_deny": ["rm -rf*"],
                },
            },
            {"name": "ssh_session_open", "description": "open a session"},
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


def ssh_tool(resources, config, opener=None) -> SshExecTool:
    tools = build_ssh_tools(resources, config)
    assert tools, "expected an ssh_exec tool to be built"
    tool = tools[0]
    if opener is not None:
        tool._opener = opener
    return tool


class SshToolRegistrationTest(unittest.TestCase):
    def test_no_ssh_grant_tool_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [runtime_resource(skill_payload())]
            self.assertEqual(build_synthetic_tools(resources, config), [])

    def test_ssh_grant_without_gateway_env_absent(self):
        # Fail-closed: grants exist but SKQUAD_TOOL_GATEWAY_URL is unset.
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp, gateway_url="")
            resources = [runtime_resource(ssh_payload())]
            self.assertEqual(build_synthetic_tools(resources, config), [])

    def test_ssh_surface_without_exec_tool_absent(self):
        # A ssh resource whose published surface lacks ssh_exec (e.g.
        # only apply tools) must not register the exec tool.
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            payload = ssh_payload()
            payload["tools"] = [{"name": "ssh_apply", "description": "apply"}]
            resources = [runtime_resource(payload)]
            self.assertEqual(build_ssh_tools(resources, config), [])

    def test_ssh_grant_registers_exec_tool(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [runtime_resource(ssh_payload("res-ssh-1"))]
            self.assertEqual(len(granted_ssh_resources(resources)), 1)
            names = [t.name for t in build_synthetic_tools(resources, config)]
            self.assertEqual(names, ["ssh_exec"])

    def test_multiple_ssh_grants_single_tool(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resources = [
                runtime_resource(ssh_payload("res-ssh-1")),
                runtime_resource(ssh_payload("res-ssh-2")),
            ]
            tools = build_ssh_tools(resources, config)
            self.assertEqual(len(tools), 1)
            self.assertEqual(tools[0].resource_ids, ("res-ssh-1", "res-ssh-2"))


class SshExecSchemaTest(unittest.TestCase):
    def test_schema_shape(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config)
            schema = tool.tools()[0]["function"]
            self.assertEqual(schema["name"], "ssh_exec")
            params = schema["parameters"]
            self.assertEqual(params["required"], ["resource_id", "host", "command"])
            self.assertFalse(params["additionalProperties"])
            self.assertEqual(params["properties"]["resource_id"]["enum"], ["res-ssh-1"])
            self.assertIn("timeout_seconds", params["properties"])


class SshExecInvokeTest(unittest.TestCase):
    def _call(self, **args) -> ToolCall:
        return ToolCall(id="c1", name="ssh_exec", arguments=args)

    def test_ungranted_resource_no_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            tool = ssh_tool(
                [runtime_resource(ssh_payload("res-ssh-1"))], config, ExplodingOpener()
            )
            result = tool.invoke(self._call(resource_id="other", host="h", command="ls"), config)
            self.assertFalse(result.ok)
            self.assertIn("not granted", result.content)

    def test_missing_host_or_command_no_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            tool = ssh_tool(
                [runtime_resource(ssh_payload("res-ssh-1"))], config, ExplodingOpener()
            )
            self.assertFalse(tool.invoke(self._call(resource_id="res-ssh-1", host="", command="ls"), config).ok)
            self.assertFalse(tool.invoke(self._call(resource_id="res-ssh-1", host="h", command=""), config).ok)

    def test_exec_success_passthrough(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resp = _FakeResponse(
                {
                    "stdout": " 02:20:11 up 3 days\n",
                    "stderr": "",
                    "exit_code": 0,
                    "recording_id": "rec-77",
                    "duration_ms": 120,
                }
            )
            opener = CapturingOpener(resp)
            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config, opener)
            result = tool.invoke(
                self._call(resource_id="res-ssh-1", host="lab-1", command="uptime", timeout_seconds=30),
                config,
            )
            self.assertTrue(result.ok)
            parsed = json.loads(result.content)
            self.assertEqual(parsed["exit_code"], 0)
            self.assertIn("up 3 days", parsed["stdout"])
            self.assertEqual(parsed["recording_id"], "rec-77")
            # Request wiring
            self.assertEqual(len(opener.calls), 1)
            req = opener.calls[0]
            self.assertEqual(req.full_url, f"{GATEWAY_URL}/v1/ssh/res-ssh-1")
            self.assertEqual(req.get_header("Authorization"), f"Bearer {FAKE_CRED}")
            self.assertEqual(req.get_header("X-skquad-agent-id"), "agent-1")
            sent = json.loads(req.data.decode("utf-8"))
            self.assertEqual(sent["tool"], "ssh_exec")
            self.assertEqual(sent["host"], "lab-1")
            self.assertEqual(sent["command"], "uptime")
            self.assertEqual(sent["timeout_seconds"], 30)

    def test_timeout_zero_not_forwarded(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            opener = CapturingOpener(_FakeResponse({"exit_code": 0}))
            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config, opener)
            tool.invoke(
                self._call(resource_id="res-ssh-1", host="lab-1", command="uptime", timeout_seconds=0),
                config,
            )
            sent = json.loads(opener.calls[0].data.decode("utf-8"))
            self.assertNotIn("timeout_seconds", sent)

    def test_pending_confirmation_surfaced(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            resp = _FakeResponse(
                {
                    "error": {"code": "pending_confirmation", "message": "owner approval required"},
                    "confirmation_id": "conf-42",
                    "retry_with": "X-Skquad-Confirmation-Id",
                },
                status=202,
            )
            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config, CapturingOpener(resp))
            result = tool.invoke(
                self._call(resource_id="res-ssh-1", host="lab-1", command="rm -rf /var/log"), config
            )
            self.assertTrue(result.ok)
            self.assertIn("PENDING OWNER CONFIRMATION", result.content)
            self.assertIn("conf-42", result.content)
            self.assertIn("was NOT executed", result.content)

    def test_http_error_carried_as_visible_result(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)
            body = json.dumps({"error": "denied", "message": "host_denied"}).encode("utf-8")

            def raise_403(req):
                raise error.HTTPError(
                    req.full_url, 403, "Forbidden", {}, io.BytesIO(body)
                )

            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config, raise_403)
            result = tool.invoke(
                self._call(resource_id="res-ssh-1", host="evil.host", command="uptime"), config
            )
            self.assertTrue(result.ok)  # visible to the model, not a tool crash
            self.assertIn("HTTP 403", result.content)
            self.assertIn("host_denied", result.content)
            self.assertNotIn(FAKE_CRED, result.content)

    def test_transport_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)

            def raise_conn(req):
                raise OSError("connection refused")

            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config, raise_conn)
            result = tool.invoke(
                self._call(resource_id="res-ssh-1", host="lab-1", command="uptime"), config
            )
            self.assertFalse(result.ok)
            self.assertIn("gateway unreachable", result.content)

    def test_malformed_response(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = gateway_config(tmp)

            class BadResp:
                status = 200

                def read(self):
                    return b"<html>not json"

                def getcode(self):
                    return 200

                def __enter__(self):
                    return self

                def __exit__(self, *exc):
                    return False

            tool = ssh_tool([runtime_resource(ssh_payload("res-ssh-1"))], config, lambda req: BadResp())
            result = tool.invoke(
                self._call(resource_id="res-ssh-1", host="lab-1", command="uptime"), config
            )
            self.assertFalse(result.ok)
            self.assertIn("malformed", result.content)


if __name__ == "__main__":
    unittest.main()
