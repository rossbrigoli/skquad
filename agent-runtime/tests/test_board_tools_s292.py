"""S-292 (ADR-0016 D1) tests — board.* builtin tools for agent-driven board work.

Covers: schema presence, exact endpoint routing (mocked HTTP), CP error
mapping (409/400/429 surfaced with the machine code, never swallowed),
the complete review-decision default (review_needed=true), the fencing
token never leaking into tool output, and registry registration of all
seven verbs. Same opener-mock style as test_builtin_tools.py.
"""

import io
import json
import unittest
from unittest import mock
from urllib import error

from skquad_runtime import builtin_tools as bt
from skquad_runtime.runtime import ToolCall

CP = "http://cp.test"


class FakeResponse:
    def __init__(self, status, payload):
        self.status = status
        self._payload = json.dumps(payload).encode("utf-8")

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def read(self, amt=None):
        return self._payload if amt is None else self._payload[:amt]


def http_error(code, err_code=None, message=""):
    body = {"error": {"code": err_code, "message": message}} if err_code else b"nope"
    payload = body if isinstance(body, bytes) else json.dumps(body).encode("utf-8")
    return error.HTTPError("http://cp.test", code, "boom", {}, io.BytesIO(payload))


def make_context():
    return bt.BuiltinToolContext(
        workspace_dir="/tmp/s292-ws",
        control_plane_url=CP,
        agent_credential="cred-1",
        agent_id="agent-1",
    )


class _Recorder:
    """Captures (method, url, body) of every outbound request."""

    def __init__(self, responses):
        self.calls = []
        self._responses = responses  # list of FakeResponse | Exception

    def __call__(self, req, timeout=None):
        body = req.data.decode("utf-8") if req.data else ""
        self.calls.append((req.get_method(), req.full_url, body, dict(req.header_items())))
        item = self._responses[min(len(self.calls) - 1, len(self._responses) - 1)]
        if isinstance(item, Exception):
            raise item
        return item


def tool(name):
    cls = bt._BUILTIN_REGISTRY[name]
    return cls({}, make_context())


def call(name, **arguments):
    return ToolCall(id="c1", name=name, arguments=arguments)


class BoardToolRegistrationTests(unittest.TestCase):
    EXPECTED = [
        "board.create_task",
        "board.assign",
        "board.claim",
        "board.complete",
        "board.block",
        "board.reopen",
        "board.request_changes",
        # S-294 (ADR-0016 D5/D8): leader approval of others' tasks.
        "board.approve",
    ]

    def test_all_board_verbs_registered_with_exact_names(self):
        for name in self.EXPECTED:
            self.assertIn(name, bt._BUILTIN_REGISTRY, f"{name} missing from registry")

    def test_build_builtin_tools_instantiates_all_board_verbs(self):
        config = {"tools": [{"name": n, "enabled": True, "policy": {}} for n in self.EXPECTED]}
        built = bt.build_builtin_tools(config, make_context())
        self.assertEqual(sorted(t.name for t in built), sorted(self.EXPECTED))

    def test_disabled_entries_are_skipped(self):
        config = {"tools": [{"name": "board.claim", "enabled": False, "policy": {}}]}
        self.assertEqual(bt.build_builtin_tools(config, make_context()), [])

    def test_schemas_present_with_required_fields(self):
        required = {
            "board.create_task": ["title"],
            "board.assign": ["task_id", "assignee_agent_id"],
            "board.claim": ["task_id"],
            "board.complete": ["task_id", "summary"],
            "board.block": ["task_id", "reason"],
            "board.reopen": ["task_id", "reason"],
            "board.request_changes": ["task_id", "comment"],
            "board.approve": ["task_id"],
        }
        for name, req in required.items():
            schemas = tool(name).tools()
            self.assertEqual(len(schemas), 1, f"{name}: exactly one schema")
            fn = schemas[0]["function"]
            self.assertEqual(fn["name"], name)
            self.assertEqual(fn["parameters"]["required"], req)
            self.assertTrue(fn["description"].strip(), f"{name}: description present")


class BoardToolHTTPTests(unittest.TestCase):
    def setUp(self):
        bt._BOARD_FENCES.clear()

    # -- board.create_task --------------------------------------------------

    def test_create_task_posts_to_agent_create_endpoint(self):
        rec = _Recorder([FakeResponse(201, {"id": "t-9", "status": "backlog"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.create_task").invoke(
                call("board.create_task", title="Do it", description="Details"), None
            )
        self.assertTrue(res.ok)
        method, url, body, headers = rec.calls[0]
        self.assertEqual(method, "POST")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks")
        sent = json.loads(body)
        self.assertEqual(sent["title"], "Do it")
        self.assertEqual(sent["description"], "Details")
        self.assertNotIn("ready", sent)
        self.assertEqual(headers["Authorization"], "Bearer cred-1")
        self.assertIn("created task t-9", res.content)

    def test_create_task_ready_true_surfaces_403_capability(self):
        rec = _Recorder([http_error(403, "leader_capability_required", "nope")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.create_task").invoke(
                call("board.create_task", title="Urgent", ready=True), None
            )
        self.assertFalse(res.ok)
        self.assertIn("403", res.content)
        self.assertIn("leader_capability_required", res.content)
        self.assertEqual(json.loads(rec.calls[0][2])["ready"], True)

    def test_create_task_requires_title(self):
        rec = _Recorder([FakeResponse(201, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.create_task").invoke(call("board.create_task", title="  "), None)
        self.assertFalse(res.ok)
        self.assertIn("title is required", res.content)
        self.assertEqual(rec.calls, [])

    # -- board.assign -------------------------------------------------------

    def test_assign_patches_assignee_endpoint(self):
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "assignee_agent_id": "a-2"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.assign").invoke(
                call("board.assign", task_id="t-1", assignee_agent_id="a-2"), None
            )
        self.assertTrue(res.ok)
        method, url, body, _ = rec.calls[0]
        self.assertEqual(method, "PATCH")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks/t-1/assignee")
        self.assertEqual(json.loads(body), {"assignee_agent_id": "a-2"})

    def test_assign_surfaces_403_leader_capability(self):
        rec = _Recorder([http_error(403, "leader_capability_required", "not leader")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.assign").invoke(
                call("board.assign", task_id="t-1", assignee_agent_id="a-9"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("403", res.content)
        self.assertIn("leader_capability_required", res.content)

    # -- board.claim ----------------------------------------------------------

    def test_claim_posts_start_and_hides_fencing_token(self):
        rec = _Recorder([
            FakeResponse(200, {
                "id": "t-1",
                "execution_id": "exec-77",
                "fencing_token": "sekrit-fence-999",
                "lease_expires_at": "2026-10-24T02:00:00Z",
            })
        ])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.claim").invoke(call("board.claim", task_id="t-1"), None)
        self.assertTrue(res.ok)
        method, url, _, _ = rec.calls[0]
        self.assertEqual(method, "POST")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks/t-1/start")
        self.assertIn("exec-77", res.content)
        self.assertIn("lease active", res.content)
        # The fencing token must NEVER appear in tool output.
        self.assertNotIn("sekrit-fence-999", res.content)
        # ...but the runtime-side registry holds it for terminal verbs.
        self.assertEqual(bt._board_fence("t-1"), ("exec-77", "sekrit-fence-999"))

    def test_claim_without_fence_reports_not_established(self):
        rec = _Recorder([FakeResponse(200, {"id": "t-1"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.claim").invoke(call("board.claim", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("claim not established", res.content)
        self.assertIsNone(bt._board_fence("t-1"))

    def test_claim_surfaces_backlog_409(self):
        rec = _Recorder([http_error(409, "task_in_backlog", "move it out first")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.claim").invoke(call("board.claim", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("409", res.content)
        self.assertIn("task_in_backlog", res.content)

    # -- board.complete -------------------------------------------------------

    def _claim(self, task_id="t-1"):
        bt._remember_board_fence(task_id, "exec-1", "fence-1")

    def test_complete_defaults_review_needed_true(self):
        self._claim()
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "status": "in-review"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.complete").invoke(
                call("board.complete", task_id="t-1", summary="All good"), None
            )
        self.assertTrue(res.ok)
        sent = json.loads(rec.calls[0][2])
        self.assertEqual(sent["review_needed"], True)
        self.assertEqual(sent["summary"], "All good")
        self.assertEqual(sent["execution_id"], "exec-1")
        self.assertEqual(sent["fencing_token"], "fence-1")
        self.assertIn("IN-REVIEW", res.content)
        # Fence is dropped after the terminal transition.
        self.assertIsNone(bt._board_fence("t-1"))

    def test_complete_false_passes_justification(self):
        self._claim()
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "status": "done"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.complete").invoke(
                call(
                    "board.complete",
                    task_id="t-1",
                    review_needed=False,
                    summary="Trivial rename, verified",
                    justification="typo fix, fully verified, no judgment needed",
                ),
                None,
            )
        self.assertTrue(res.ok)
        sent = json.loads(rec.calls[0][2])
        self.assertEqual(sent["review_needed"], False)
        self.assertEqual(sent["justification"], "typo fix, fully verified, no judgment needed")
        self.assertIn("DONE", res.content)

    def test_complete_false_without_justification_fails_locally(self):
        self._claim()
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.complete").invoke(
                call("board.complete", task_id="t-1", review_needed=False, summary="x"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("justification", res.content)
        self.assertEqual(rec.calls, [])  # no HTTP round trip

    def test_complete_without_claim_fails_locally(self):
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.complete").invoke(
                call("board.complete", task_id="t-unknown", summary="x"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("claim it first", res.content)
        self.assertEqual(rec.calls, [])

    def test_complete_surfaces_self_approval_disabled_409(self):
        self._claim()
        rec = _Recorder([http_error(409, "self_approval_disabled", "policy says no")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.complete").invoke(
                call(
                    "board.complete",
                    task_id="t-1",
                    review_needed=False,
                    summary="x",
                    justification="because",
                ),
                None,
            )
        self.assertFalse(res.ok)
        self.assertIn("409", res.content)
        self.assertIn("self_approval_disabled", res.content)
        # Fence retained after a failed terminal call so the retry can proceed.
        self.assertIsNotNone(bt._board_fence("t-1"))

    def test_complete_rejects_non_boolean_review_needed(self):
        self._claim()
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.complete").invoke(
                call("board.complete", task_id="t-1", summary="x", review_needed="maybe"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("boolean", res.content)
        self.assertEqual(rec.calls, [])

    # -- board.block ----------------------------------------------------------

    def test_block_posts_reason_as_summary_with_fence(self):
        self._claim()
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "status": "blocked"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.block").invoke(
                call("board.block", task_id="t-1", reason="waiting on credentials"), None
            )
        self.assertTrue(res.ok)
        method, url, body, _ = rec.calls[0]
        self.assertEqual(method, "POST")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks/t-1/block")
        sent = json.loads(body)
        self.assertEqual(sent["summary"], "waiting on credentials")
        self.assertEqual(sent["fencing_token"], "fence-1")
        self.assertIsNone(bt._board_fence("t-1"))

    def test_block_requires_reason(self):
        self._claim()
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.block").invoke(call("board.block", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("reason is required", res.content)
        self.assertEqual(rec.calls, [])

    def test_block_without_claim_fails_locally(self):
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.block").invoke(
                call("board.block", task_id="t-x", reason="why"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("claim it first", res.content)
        self.assertEqual(rec.calls, [])

    # -- board.reopen -----------------------------------------------------------

    def test_reopen_posts_reason_and_feedback_and_rotates_fence(self):
        self._claim()
        rec = _Recorder([
            FakeResponse(200, {
                "id": "t-1",
                "status": "in-progress",
                "execution_id": "exec-2",
                "fencing_token": "fence-2",
            })
        ])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.reopen").invoke(
                call(
                    "board.reopen",
                    task_id="t-1",
                    reason="reviewer found a gap",
                    reviewer_feedback="the tests do not cover X",
                ),
                None,
            )
        self.assertTrue(res.ok)
        method, url, body, _ = rec.calls[0]
        self.assertEqual(method, "POST")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks/t-1/reopen")
        sent = json.loads(body)
        self.assertEqual(sent["reason"], "reviewer found a gap")
        self.assertEqual(sent["reviewer_feedback"], "the tests do not cover X")
        # The NEW attempt's fence replaces the old one, token hidden from output.
        self.assertEqual(bt._board_fence("t-1"), ("exec-2", "fence-2"))
        self.assertNotIn("fence-2", res.content)
        self.assertIn("IN PROGRESS", res.content)

    def test_reopen_surfaces_cap_429(self):
        rec = _Recorder([http_error(429, "reopen_cap_exceeded", "cap reached")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.reopen").invoke(
                call("board.reopen", task_id="t-1", reason="again"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("429", res.content)
        self.assertIn("reopen_cap_exceeded", res.content)

    def test_reopen_requires_reason(self):
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.reopen").invoke(call("board.reopen", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("reason is required", res.content)
        self.assertEqual(rec.calls, [])

    # -- board.request_changes ---------------------------------------------------

    def test_request_changes_posts_comment(self):
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "status": "todo", "assignee_agent_id": ""})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.request_changes").invoke(
                call("board.request_changes", task_id="t-1", comment="needs error handling"), None
            )
        self.assertTrue(res.ok)
        method, url, body, _ = rec.calls[0]
        self.assertEqual(method, "POST")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks/t-1/request-changes")
        self.assertEqual(json.loads(body), {"comment": "needs error handling"})
        self.assertIn("TO DO", res.content)

    def test_request_changes_surfaces_wrong_status_409(self):
        rec = _Recorder([http_error(409, "request_changes_wrong_status", "not in-review")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.request_changes").invoke(
                call("board.request_changes", task_id="t-1", comment="x"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("409", res.content)
        self.assertIn("request_changes_wrong_status", res.content)

    def test_request_changes_surfaces_not_assignee_403(self):
        rec = _Recorder([http_error(403, "not_assignee", "not yours")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.request_changes").invoke(
                call("board.request_changes", task_id="t-1", comment="x"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("403", res.content)
        self.assertIn("not_assignee", res.content)

    def test_request_changes_requires_comment(self):
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.request_changes").invoke(
                call("board.request_changes", task_id="t-1"), None
            )
        self.assertFalse(res.ok)
        self.assertIn("comment is required", res.content)
        self.assertEqual(rec.calls, [])

    # -- board.approve (S-294) ----------------------------------------------

    def test_approve_posts_to_approve_endpoint(self):
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "status": "done"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(
                call("board.approve", task_id="t-1", comment="looks good"), None
            )
        self.assertTrue(res.ok)
        method, url, body, _ = rec.calls[0]
        self.assertEqual(method, "POST")
        self.assertEqual(url, CP + "/api/v1/agents/me/tasks/t-1/approve")
        self.assertEqual(json.loads(body), {"comment": "looks good"})
        self.assertIn("DONE", res.content)

    def test_approve_without_comment_sends_empty_body(self):
        rec = _Recorder([FakeResponse(200, {"id": "t-1", "status": "done"})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(call("board.approve", task_id="t-1"), None)
        self.assertTrue(res.ok)
        self.assertEqual(json.loads(rec.calls[0][2]), {})

    def test_approve_surfaces_leader_capability_403(self):
        rec = _Recorder([http_error(403, "leader_capability_required", "no grant")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(call("board.approve", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("403", res.content)
        self.assertIn("leader_capability_required", res.content)

    def test_approve_surfaces_approval_disabled_403(self):
        rec = _Recorder([http_error(403, "leader_approval_disabled", "knob off")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(call("board.approve", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("leader_approval_disabled", res.content)

    def test_approve_surfaces_self_approval_denial_403(self):
        rec = _Recorder([http_error(403, "leader_approval_self_not_allowed", "own card")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(call("board.approve", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("leader_approval_self_not_allowed", res.content)

    def test_approve_surfaces_wrong_status_409(self):
        rec = _Recorder([http_error(409, "approve_wrong_status", "not in-review")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(call("board.approve", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("409", res.content)
        self.assertIn("approve_wrong_status", res.content)

    def test_approve_requires_task_id(self):
        rec = _Recorder([FakeResponse(200, {})])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.approve").invoke(call("board.approve", task_id="  "), None)
        self.assertFalse(res.ok)
        self.assertIn("task_id is required", res.content)
        self.assertEqual(rec.calls, [])

    # -- transport errors -------------------------------------------------------

    def test_transport_error_surfaced_not_swallowed(self):
        rec = _Recorder([error.URLError("down")])
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen", side_effect=rec):
            res = tool("board.claim").invoke(call("board.claim", task_id="t-1"), None)
        self.assertFalse(res.ok)
        self.assertIn("control-plane request failed", res.content)


if __name__ == "__main__":
    unittest.main()
