"""S-282 tests — the built-in session inspection tools.

sessions_list / sessions_history / session_status are read-only clients
over EXISTING agent-scoped control-plane endpoints. These tests stub
urlopen (no real network) and assert, for every round trip:

  * GET-only — no POST/PUT/DELETE ever leaves the tool,
  * correct URLs and the Bearer + X-Skquad-Agent-ID auth headers,
  * filters/limits behave,
  * sanitization: secret-looking values redacted, long bodies truncated
    with an explicit notice,
  * include_tools gating for tool-call payloads,
  * graceful omission of unavailable fields (session_status),
  * 403/404 surface as clean tool errors.
"""

import json
import pathlib
import tempfile
import unittest
from unittest import mock
from urllib import error

from skquad_runtime import builtin_tools as bt
from skquad_runtime.runtime import ToolCall

TASKS_PATH = "/api/v1/agents/me/tasks"
PEERS_PATH = "/api/v1/agents/me/peers"
HISTORY_PATH = "/api/v1/agents/me/messages/history"
MODEL_PATH = "/api/v1/agents/me/model"


def task(task_id="11111111-2222-3333-4444-555555555555", number=42, **overrides):
    payload = {
        "id": task_id,
        "board_id": "board-1",
        "squad_id": "squad-1",
        "title": "Fix the widget",
        "description": "Widget is broken.",
        "status": "in-progress",
        "assignee_agent_id": "agent-xyz",
        "task_number": number,
        "created_at": "2026-10-20T00:00:00Z",
        "updated_at": "2026-10-21T01:02:03Z",
    }
    payload.update(overrides)
    return payload


def message(msg_id="m-1", **overrides):
    payload = {
        "id": msg_id,
        "from_type": "user",
        "from_id": "user-1",
        "from_display": "Ross",
        "to_agent_id": "agent-xyz",
        "squad_id": "squad-1",
        "type": "chat",
        "payload": {"message": "hello there"},
        "status": "delivered",
        "created_at": "2026-10-21T02:00:00Z",
    }
    payload.update(overrides)
    return payload


def peer(name="Mary", **overrides):
    payload = {"id": "peer-1", "name": name, "role": "builder", "status": "running"}
    payload.update(overrides)
    return payload


def tc(name, args):
    return ToolCall(id="call-1", name=name, arguments=args)


class FakeHTTPResponse:
    def __init__(self, status, payload):
        self.status = status
        self._payload = payload
        self.headers = {}

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def read(self, amt=None):
        return self._payload if amt is None else self._payload[:amt]

    def getcode(self):
        return self.status


class SessionToolTestBase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.ctx = bt.BuiltinToolContext(
            workspace_dir=str(pathlib.Path(self._tmp.name) / "ws"),
            control_plane_url="http://control-plane",
            agent_credential="cred-1",
            agent_id="agent-xyz",
        )

    def make_faker(self, routes):
        """routes: {path: payload | Exception}. Records every request."""
        calls = []

        def fake_urlopen(req, timeout=None):
            calls.append(req)
            path = req.selector.split("?")[0]
            if path not in routes:
                raise AssertionError(f"unexpected request to {req.full_url}")
            outcome = routes[path]
            if isinstance(outcome, Exception):
                raise outcome
            return FakeHTTPResponse(200, json.dumps(outcome).encode())

        return fake_urlopen, calls

    def assert_get_only(self, calls):
        self.assertTrue(calls, "expected at least one request")
        for req in calls:
            self.assertEqual(req.get_method(), "GET", f"non-GET call: {req.get_method()} {req.full_url}")
            self.assertEqual(req.get_header("Authorization"), "Bearer cred-1")
            self.assertEqual(req.get_header("X-skquad-agent-id"), "agent-xyz")

    def invoke(self, tool, name, args, routes):
        faker, calls = self.make_faker(routes)
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=faker):
            result = tool.invoke(tc(name, args), None)
        return result, calls


class SessionsListTests(SessionToolTestBase):
    def tool(self, policy=None):
        return bt.SessionsListTool(policy or {}, self.ctx)

    def test_default_lists_tasks_get_only(self):
        result, calls = self.invoke(
            self.tool(),
            "sessions_list",
            {},
            {TASKS_PATH: [task(), task(task_id="other-1", number=43, title="Other", status="todo")]},
        )
        self.assertTrue(result.ok, result.content)
        self.assert_get_only(calls)
        self.assertIn("T-42", result.content)
        self.assertIn("Fix the widget", result.content)
        self.assertIn("[in-progress]", result.content)
        self.assertIn("T-43", result.content)
        self.assertIn("[todo]", result.content)

    def test_status_filter(self):
        result, _ = self.invoke(
            self.tool(),
            "sessions_list",
            {"status": "todo"},
            {TASKS_PATH: [task(), task(task_id="other-1", number=43, status="todo", title="Second")]},
        )
        self.assertTrue(result.ok)
        self.assertIn("T-43", result.content)
        self.assertNotIn("T-42", result.content)

    def test_limit_caps_rows(self):
        tasks = [task(task_id=f"t{i}", number=i) for i in range(10)]
        result, _ = self.invoke(self.tool(), "sessions_list", {"limit": 3}, {TASKS_PATH: tasks})
        self.assertTrue(result.ok)
        self.assertEqual(result.content.count("id=t"), 3)
        self.assertIn("filtered out / over limit", result.content)

    def test_peers_kind(self):
        result, calls = self.invoke(
            self.tool(),
            "sessions_list",
            {"kind": "peers"},
            {PEERS_PATH: [peer(), peer(name="Groot", id="peer-2", role="admin")]},
        )
        self.assertTrue(result.ok)
        self.assert_get_only(calls)
        self.assertIn("Mary [running] role=builder", result.content)
        self.assertIn("Groot", result.content)
        self.assertNotIn("TASKS", result.content)

    def test_all_kind_hits_both_endpoints(self):
        result, calls = self.invoke(
            self.tool(),
            "sessions_list",
            {"kind": "all"},
            {TASKS_PATH: [task()], PEERS_PATH: [peer()]},
        )
        self.assertTrue(result.ok)
        self.assertIn("TASKS", result.content)
        self.assertIn("PEERS", result.content)
        paths = {req.selector.split("?")[0] for req in calls}
        self.assertEqual(paths, {TASKS_PATH, PEERS_PATH})
        self.assert_get_only(calls)

    def test_bad_kind_rejected_without_network(self):
        result, calls = self.invoke(self.tool(), "sessions_list", {"kind": "everything"}, {})
        self.assertFalse(result.ok)
        self.assertIn("kind must be one of", result.content)
        self.assertEqual(calls, [])

    def test_http_error_surfaces_cleanly(self):
        err = error.HTTPError(TASKS_PATH, 500, "boom", {}, None)
        result, _ = self.invoke(self.tool(), "sessions_list", {}, {TASKS_PATH: err})
        self.assertFalse(result.ok)
        self.assertIn("HTTP 500", result.content)


class SessionsHistoryTests(SessionToolTestBase):
    def tool(self, policy=None):
        return bt.SessionsHistoryTool(policy or {}, self.ctx)

    def test_task_id_reads_context(self):
        ctx_payload = {
            "task": task(description="Widget is broken.\nSecrets stay hidden."),
            "resources": [],
            "memory": [],
            "limits": {},
        }
        result, calls = self.invoke(
            self.tool(),
            "sessions_history",
            {"task_id": "11111111-2222-3333-4444-555555555555"},
            {"/api/v1/agents/me/tasks/11111111-2222-3333-4444-555555555555/context": ctx_payload},
        )
        self.assertTrue(result.ok, result.content)
        self.assert_get_only(calls)
        self.assertIn("TASK CONTEXT", result.content)
        self.assertIn("Fix the widget", result.content)
        self.assertIn("Widget is broken", result.content)

    def test_agent_id_reads_chat(self):
        result, calls = self.invoke(
            self.tool(),
            "sessions_history",
            {"agent_id": "peer-1"},
            {"/api/v1/agents/peer-1/chat": [message()]},
        )
        self.assertTrue(result.ok, result.content)
        self.assert_get_only(calls)
        self.assertIn("CHAT HISTORY — agent peer-1", result.content)
        self.assertIn("Ross (chat/delivered): hello there", result.content)

    def test_default_reads_own_history(self):
        result, calls = self.invoke(
            self.tool(),
            "sessions_history",
            {},
            {HISTORY_PATH: [message(), message(msg_id="m-2", type="consult")]},
        )
        self.assertTrue(result.ok, result.content)
        self.assert_get_only(calls)
        self.assertIn("YOUR MESSAGE HISTORY — 2 shown", result.content)
        self.assertIn("consult/delivered", result.content)

    def test_both_targets_rejected_without_network(self):
        result, calls = self.invoke(
            self.tool(),
            "sessions_history",
            {"task_id": "t", "agent_id": "a"},
            {},
        )
        self.assertFalse(result.ok)
        self.assertIn("at most one", result.content)
        self.assertEqual(calls, [])

    def test_sanitization_redacts_secret_keys_and_values(self):
        msgs = [
            message(
                payload={
                    "message": "token is sk-abcdefghijklmnop and kb_0123456789abcdef here",
                    "api_token": "supersecret",
                    "nested": {"password": "***", "safe": "visible"},
                }
            )
        ]
        result, _ = self.invoke(self.tool(), "sessions_history", {}, {HISTORY_PATH: msgs})
        self.assertTrue(result.ok)
        self.assertNotIn("sk-abcdefghijklmnop", result.content)
        self.assertNotIn("kb_0123456789abcdef", result.content)
        self.assertNotIn("supersecret", result.content)
        self.assertNotIn("hunter2", result.content)
        self.assertIn(bt.REDACTED, result.content)
        self.assertIn("visible", result.content)

    def test_long_body_truncated_with_notice(self):
        long_text = "x" * 2000
        result, _ = self.invoke(
            self.tool(), "sessions_history", {}, {HISTORY_PATH: [message(payload={"message": long_text})]}
        )
        self.assertTrue(result.ok)
        self.assertIn("truncated", result.content)
        self.assertIn("2000 chars total", result.content)
        self.assertNotIn(long_text, result.content)

    def test_include_tools_gating(self):
        msgs = [
            message(payload={"message": "working", "tool_calls": [{"name": "exec", "args": "ls"}]}),
        ]
        hidden, _ = self.invoke(self.tool(), "sessions_history", {}, {HISTORY_PATH: msgs})
        self.assertTrue(hidden.ok)
        self.assertNotIn("tool_calls", hidden.content)
        self.assertNotIn("exec", hidden.content)

        shown, _ = self.invoke(
            self.tool(), "sessions_history", {"include_tools": True}, {HISTORY_PATH: msgs}
        )
        self.assertTrue(shown.ok)
        self.assertIn("tool_calls", shown.content)
        self.assertIn("exec", shown.content)

    def test_tool_only_message_shows_hidden_notice(self):
        msgs = [message(payload={"tool_calls": [{"name": "exec"}]})]
        result, _ = self.invoke(self.tool(), "sessions_history", {}, {HISTORY_PATH: msgs})
        self.assertTrue(result.ok)
        self.assertIn("tool payload hidden", result.content)

    def test_limit_omits_with_notice(self):
        msgs = [message(msg_id=f"m{i}") for i in range(10)]
        result, _ = self.invoke(self.tool(), "sessions_history", {"limit": 4}, {HISTORY_PATH: msgs})
        self.assertTrue(result.ok)
        self.assertIn("6 more entries omitted (limit 4)", result.content)

    def test_403_surfaces_clean_error(self):
        err = error.HTTPError(
            "/api/v1/agents/me/tasks/nope/context",
            403,
            "forbidden",
            {"Content-Type": "application/json"},
            io_bytes(json.dumps({"error": "task not assigned to you"}).encode()),
        )
        result, calls = self.invoke(
            self.tool(), "sessions_history", {"task_id": "nope"}, {"/api/v1/agents/me/tasks/nope/context": err}
        )
        self.assertFalse(result.ok)
        self.assertIn("HTTP 403", result.content)
        self.assertIn("task not assigned to you", result.content)
        self.assert_get_only(calls)

    def test_404_surfaces_clean_error(self):
        err = error.HTTPError("/api/v1/agents/ghost/chat", 404, "not found", {}, io_bytes(b""))
        result, _ = self.invoke(
            self.tool(), "sessions_history", {"agent_id": "ghost"}, {"/api/v1/agents/ghost/chat": err}
        )
        self.assertFalse(result.ok)
        self.assertIn("HTTP 404", result.content)


def io_bytes(data: bytes):
    import io

    return io.BytesIO(data)


class SessionStatusTests(SessionToolTestBase):
    def tool(self, policy=None):
        return bt.SessionStatusTool(policy or {}, self.ctx)

    def full_routes(self, **overrides):
        routes = {
            MODEL_PATH: {"model_name": "claude-test", "supports_vision": False, "supports_tools": True, "context_window": 200000},
            TASKS_PATH: [
                task(),
                task(task_id="t-2", number=43, status="todo", title="Queued"),
                task(task_id="t-3", number=44, status="done", title="Finished"),
            ],
            PEERS_PATH: [peer(), peer(name="Groot", id="peer-2")],
        }
        routes.update(overrides)
        return routes

    def test_status_card_aggregates_fields(self):
        result, calls = self.invoke(self.tool(), "session_status", {}, self.full_routes())
        self.assertTrue(result.ok, result.content)
        self.assert_get_only(calls)
        self.assertIn("agent_id: agent-xyz", result.content)
        self.assertIn("model: claude-test", result.content)
        self.assertIn("3 total", result.content)
        self.assertIn("in-progress=1", result.content)
        self.assertIn("active: T-42 Fix the widget", result.content)
        self.assertIn("visible_peers: 2", result.content)
        self.assertIn("wake_elapsed_seconds:", result.content)

    def test_model_unavailable_omitted_gracefully(self):
        result, _ = self.invoke(
            self.tool(),
            "session_status",
            {},
            self.full_routes(**{MODEL_PATH: error.HTTPError(MODEL_PATH, 500, "boom", {}, None)}),
        )
        self.assertTrue(result.ok, result.content)
        self.assertNotIn("model:", result.content)
        self.assertIn("agent_id: agent-xyz", result.content)

    def test_tasks_unavailable_noted_not_fabricated(self):
        result, _ = self.invoke(
            self.tool(),
            "session_status",
            {},
            self.full_routes(**{TASKS_PATH: error.URLError("down")}),
        )
        self.assertTrue(result.ok)
        self.assertIn("tasks: unavailable", result.content)
        self.assertNotIn("active:", result.content)

    def test_no_active_tasks_omits_active_line(self):
        routes = self.full_routes(**{TASKS_PATH: [task(status="todo")]})
        result, _ = self.invoke(self.tool(), "session_status", {}, routes)
        self.assertTrue(result.ok)
        self.assertNotIn("active:", result.content)

    def test_status_never_mutates(self):
        _, calls = self.invoke(self.tool(), "session_status", {}, self.full_routes())
        self.assert_get_only(calls)


class RegistryTests(unittest.TestCase):
    def test_registry_contains_session_tools(self):
        for name in ("sessions_list", "sessions_history", "session_status"):
            self.assertIn(name, bt._BUILTIN_REGISTRY)

    def test_build_builtin_tools_instantiates_enabled_session_tools(self):
        ctx = bt.BuiltinToolContext(
            workspace_dir="/tmp/unused",
            control_plane_url="http://control-plane",
            agent_credential="c",
            agent_id="a",
        )
        config = {
            "tools": [
                {"name": "sessions_list", "enabled": True, "policy": {}},
                {"name": "sessions_history", "enabled": True, "policy": {}},
                {"name": "session_status", "enabled": True, "policy": {}},
                {"name": "sessions_list", "enabled": False, "policy": {}},
            ]
        }
        tools = bt.build_builtin_tools(config, ctx)
        names = sorted(t.name for t in tools)
        self.assertEqual(names, ["session_status", "sessions_history", "sessions_list"])

    def test_schema_declarations(self):
        list_schema = bt.SessionsListTool({}, None).tools()[0]["function"]
        self.assertEqual(list_schema["name"], "sessions_list")
        props = list_schema["parameters"]["properties"]
        self.assertEqual(set(props), {"kind", "status", "limit"})
        hist_schema = bt.SessionsHistoryTool({}, None).tools()[0]["function"]
        self.assertEqual(
            set(hist_schema["parameters"]["properties"]),
            {"task_id", "agent_id", "include_tools", "limit"},
        )
        status_schema = bt.SessionStatusTool({}, None).tools()[0]["function"]
        self.assertEqual(status_schema["parameters"]["properties"], {})


if __name__ == "__main__":
    unittest.main()
