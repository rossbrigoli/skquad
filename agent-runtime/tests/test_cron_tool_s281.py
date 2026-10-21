"""S-281 tests — the built-in `cron` tool (scheduled jobs client).

Mirrors the existing builtin-tool test style (unittest + fake urlopen,
no real network). Covers: add/list/remove/pause/resume round trips
against a stubbed control-plane, client-side pre-validation (bad kind,
out-of-range interval, missing fields, oversized message, bad cron
expression / timezone), maxActiveJobs enforcement, and registry
registration.
"""

import io
import json
import pathlib
import tempfile
import unittest
from unittest import mock
from urllib import error

from skquad_runtime import builtin_tools as bt
from skquad_runtime.runtime import ToolCall

JOBS_PATH = "/api/v1/agents/me/scheduled-jobs"


def job(job_id="job-1", enabled=True, kind="every", spec=None, **overrides):
    payload = {
        "id": job_id,
        "agent_id": "agent-xyz",
        "squad_id": "squad-1",
        "message": "do the thing",
        "schedule_kind": kind,
        "schedule_spec": spec if spec is not None else {"interval_seconds": 3600},
        "enabled": enabled,
        "next_run_at": "2026-10-22T09:00:00Z",
        "fail_count": 0,
        "created_at": "2026-10-21T09:00:00Z",
    }
    payload.update(overrides)
    return payload


def tc(args):
    return ToolCall(id="call-1", name="cron", arguments=args)


class CronToolTestBase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp_path = pathlib.Path(self._tmp.name)
        self.ctx = bt.BuiltinToolContext(
            workspace_dir=str(self.tmp_path / "ws"),
            control_plane_url="http://control-plane",
            agent_credential="cred-1",
            agent_id="agent-xyz",
        )

    def tool(self, policy=None):
        return bt.CronTool(policy or {}, self.ctx)


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


class CronAddRoundTripTests(CronToolTestBase):
    def _run(self, args, list_jobs=None, created=None):
        calls = []

        def fake_urlopen(req, timeout=None):
            calls.append(req)
            if req.get_method() == "GET":
                return FakeHTTPResponse(200, json.dumps({"jobs": list_jobs or []}).encode())
            return FakeHTTPResponse(201, json.dumps(created or job("job-new")).encode())

        with mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        ):
            result = self.tool().invoke(tc(args), None)
        return result, calls

    def test_add_every_posts_correct_body_and_auth(self):
        result, calls = self._run(
            {
                "action": "add",
                "message": "check disk usage",
                "title": "disk check",
                "schedule": {"kind": "every", "interval_seconds": 3600},
            }
        )
        self.assertTrue(result.ok, result.content)
        posts = [c for c in calls if c.get_method() == "POST"]
        self.assertEqual(len(posts), 1)
        self.assertEqual(posts[0].full_url, "http://control-plane" + JOBS_PATH)
        self.assertEqual(posts[0].get_header("Authorization"), "Bearer cred-1")
        self.assertEqual(posts[0].get_header("X-skquad-agent-id"), "agent-xyz")
        sent = json.loads(posts[0].data.decode())
        self.assertEqual(sent["message"], "check disk usage")
        self.assertEqual(sent["title"], "disk check")
        self.assertEqual(sent["schedule"], {"kind": "every", "interval_seconds": 3600})
        self.assertIn("job-new", result.content)
        self.assertIn("2026-10-22T09:00:00Z", result.content)

    def test_add_at_rfc3339(self):
        result, calls = self._run(
            {
                "action": "add",
                "message": "standup reminder",
                "schedule": {"kind": "at", "at": "2026-10-25T09:00:00+10:30"},
            }
        )
        self.assertTrue(result.ok, result.content)
        posts = [c for c in calls if c.get_method() == "POST"]
        sent = json.loads(posts[0].data.decode())
        self.assertEqual(sent["schedule"]["at"], "2026-10-25T09:00:00+10:30")

    def test_add_at_accepts_z_suffix(self):
        result, _ = self._run(
            {
                "action": "add",
                "message": "one-shot",
                "schedule": {"kind": "at", "at": "2026-10-25T09:00:00Z"},
            }
        )
        self.assertTrue(result.ok, result.content)

    def test_add_cron_expression_with_timezone(self):
        result, calls = self._run(
            {
                "action": "add",
                "message": "weekday report",
                "schedule": {
                    "kind": "cron",
                    "expression": "0 9 * * 1-5",
                    "timezone": "Australia/Adelaide",
                },
            }
        )
        self.assertTrue(result.ok, result.content)
        posts = [c for c in calls if c.get_method() == "POST"]
        sent = json.loads(posts[0].data.decode())
        self.assertEqual(sent["schedule"]["expression"], "0 9 * * 1-5")
        self.assertEqual(sent["schedule"]["timezone"], "Australia/Adelaide")


class CronAddValidationTests(CronToolTestBase):
    def _assert_no_http(self, args, needle):
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = self.tool().invoke(tc(args), None)
        self.assertFalse(result.ok)
        self.assertIn(needle, result.content)
        urlopen.assert_not_called()

    def test_missing_message_rejected(self):
        self._assert_no_http(
            {"action": "add", "schedule": {"kind": "every", "interval_seconds": 3600}},
            "message is required",
        )

    def test_oversized_message_rejected(self):
        self._assert_no_http(
            {
                "action": "add",
                "message": "x" * (bt.CRON_MAX_MESSAGE_CHARS + 1),
                "schedule": {"kind": "every", "interval_seconds": 3600},
            },
            "exceeds 10000",
        )

    def test_missing_schedule_rejected(self):
        self._assert_no_http({"action": "add", "message": "hi"}, "schedule must be an object")

    def test_bad_kind_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "hourly"}},
            "unknown schedule kind",
        )

    def test_at_missing_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "at"}},
            "schedule.at is required",
        )

    def test_at_not_rfc3339_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "at", "at": "tomorrow 9am"}},
            "RFC3339",
        )

    def test_at_without_offset_rejected(self):
        self._assert_no_http(
            {
                "action": "add",
                "message": "hi",
                "schedule": {"kind": "at", "at": "2026-10-25T09:00:00"},
            },
            "RFC3339",
        )

    def test_every_missing_interval_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "every"}},
            "interval_seconds is required",
        )

    def test_every_interval_below_minimum_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "every", "interval_seconds": 30}},
            "out of range",
        )

    def test_every_interval_above_maximum_rejected(self):
        self._assert_no_http(
            {
                "action": "add",
                "message": "hi",
                "schedule": {"kind": "every", "interval_seconds": bt.CRON_MAX_INTERVAL_SECONDS + 1},
            },
            "out of range",
        )

    def test_every_non_integer_interval_rejected(self):
        self._assert_no_http(
            {
                "action": "add",
                "message": "hi",
                "schedule": {"kind": "every", "interval_seconds": "soon"},
            },
            "whole number",
        )

    def test_cron_missing_expression_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "cron"}},
            "expression is required",
        )

    def test_cron_wrong_field_count_rejected(self):
        self._assert_no_http(
            {"action": "add", "message": "hi", "schedule": {"kind": "cron", "expression": "0 9 *"}},
            "5 fields",
        )

    def test_cron_invalid_characters_rejected(self):
        self._assert_no_http(
            {
                "action": "add",
                "message": "hi",
                "schedule": {"kind": "cron", "expression": "0 9 * * %%%"},
            },
            "invalid characters",
        )

    def test_cron_invalid_timezone_rejected(self):
        self._assert_no_http(
            {
                "action": "add",
                "message": "hi",
                "schedule": {
                    "kind": "cron",
                    "expression": "0 9 * * 1-5",
                    "timezone": "Mars/Olympus",
                },
            },
            "not a valid IANA timezone",
        )

    def test_unknown_action_rejected(self):
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = self.tool().invoke(tc({"action": "fork"}), None)
        self.assertFalse(result.ok)
        self.assertIn("must be one of", result.content)
        urlopen.assert_not_called()


class CronMaxActiveJobsTests(CronToolTestBase):
    def _add_with_existing(self, existing_jobs, policy=None):
        calls = []

        def fake_urlopen(req, timeout=None):
            calls.append(req.get_method())
            if req.get_method() == "GET":
                return FakeHTTPResponse(200, json.dumps({"jobs": existing_jobs}).encode())
            return FakeHTTPResponse(201, json.dumps(job("job-new")).encode())

        with mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        ):
            result = self.tool(policy).invoke(
                tc(
                    {
                        "action": "add",
                        "message": "one more",
                        "schedule": {"kind": "every", "interval_seconds": 3600},
                    }
                ),
                None,
            )
        return result, calls

    def test_default_cap_10_refuses_eleventh_active_job(self):
        existing = [job(f"job-{i}", enabled=True) for i in range(10)]
        result, calls = self._add_with_existing(existing)
        self.assertFalse(result.ok)
        self.assertIn("maxActiveJobs=10", result.content)
        self.assertNotIn("POST", calls)  # refused BEFORE hitting the server

    def test_paused_jobs_do_not_count_toward_cap(self):
        existing = [job(f"job-{i}", enabled=(i < 9)) for i in range(12)]  # 9 enabled
        result, calls = self._add_with_existing(existing)
        self.assertTrue(result.ok, result.content)
        self.assertIn("POST", calls)

    def test_policy_override_cap(self):
        existing = [job(f"job-{i}", enabled=True) for i in range(2)]
        result, calls = self._add_with_existing(existing, policy={"maxActiveJobs": 2})
        self.assertFalse(result.ok)
        self.assertIn("maxActiveJobs=2", result.content)
        self.assertNotIn("POST", calls)

    def test_cap_check_failure_falls_through_to_server(self):
        calls = []

        def fake_urlopen(req, timeout=None):
            calls.append(req.get_method())
            if req.get_method() == "GET":
                raise error.URLError("boom")
            return FakeHTTPResponse(201, json.dumps(job("job-new")).encode())

        with mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        ):
            result = self.tool().invoke(
                tc(
                    {
                        "action": "add",
                        "message": "one more",
                        "schedule": {"kind": "every", "interval_seconds": 3600},
                    }
                ),
                None,
            )
        # Best-effort guard: a failed cap check must not block the add;
        # the server remains authoritative.
        self.assertTrue(result.ok, result.content)
        self.assertIn("POST", calls)


class CronListTests(CronToolTestBase):
    def _list(self, jobs):
        def fake_urlopen(req, timeout=None):
            assert req.get_method() == "GET"
            return FakeHTTPResponse(200, json.dumps({"jobs": jobs}).encode())

        with mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        ):
            return self.tool().invoke(tc({"action": "list"}), None)

    def test_list_empty(self):
        result = self._list([])
        self.assertTrue(result.ok)
        self.assertIn("no scheduled jobs", result.content)

    def test_list_renders_all_kinds_and_state(self):
        result = self._list(
            [
                job("j1", kind="every", spec={"interval_seconds": 600}),
                job("j2", enabled=False, kind="at", spec={"at": "2026-11-01T00:00:00Z"}),
                job(
                    "j3",
                    kind="cron",
                    spec={"expression": "0 9 * * 1-5", "timezone": "Australia/Adelaide"},
                    last_run_status="failed",
                    fail_count=2,
                    title="morning sweep",
                ),
            ]
        )
        self.assertTrue(result.ok)
        lines = result.content.split("\n")
        self.assertEqual(len(lines), 3)
        self.assertIn("j1 [enabled] every 600s", result.content)
        self.assertIn("j2 [paused] at 2026-11-01T00:00:00Z", result.content)
        self.assertIn("j3 [enabled] cron 0 9 * * 1-5 (Australia/Adelaide)", result.content)
        self.assertIn("last=failed", result.content)
        self.assertIn("fails=2", result.content)
        self.assertIn("'morning sweep'", result.content)

    def test_list_surfaces_disabled_reason(self):
        result = self._list(
            [job("j4", enabled=False, disabled_reason="auto-disabled after 5 consecutive failures")]
        )
        self.assertIn("auto-disabled after 5 consecutive failures", result.content)

    def test_list_http_error_surfaces_detail(self):
        def fake_urlopen(req, timeout=None):
            raise error.HTTPError(
                "http://control-plane" + JOBS_PATH,
                403,
                "forbidden",
                {},
                io.BytesIO(json.dumps({"error": {"code": "cron_disabled", "message": "the cron built-in tool is disabled by platform policy"}}).encode()),
            )

        with mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        ):
            result = self.tool().invoke(tc({"action": "list"}), None)
        self.assertFalse(result.ok)
        self.assertIn("HTTP 403", result.content)
        self.assertIn("disabled by platform policy", result.content)


class CronJobActionTests(CronToolTestBase):
    def _action(self, action, args=None, status=200, payload=None):
        calls = []

        def fake_urlopen(req, timeout=None):
            calls.append(req)
            if status >= 400:
                raise error.HTTPError(
                    req.full_url, status, "err", {}, io.BytesIO(json.dumps(payload or {}).encode())
                )
            body = json.dumps(payload).encode() if payload is not None else b""
            return FakeHTTPResponse(status, body)

        arguments = dict(args or {})
        arguments["action"] = action
        with mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        ):
            result = self.tool().invoke(tc(arguments), None)
        return result, calls

    def test_remove_deletes_job(self):
        result, calls = self._action("remove", {"job_id": "j9"}, status=204)
        self.assertTrue(result.ok, result.content)
        self.assertEqual(calls[0].get_method(), "DELETE")
        self.assertEqual(calls[0].full_url, "http://control-plane" + JOBS_PATH + "/j9")
        self.assertIn("cancelled", result.content)

    def test_pause_posts_pause_endpoint(self):
        result, calls = self._action("pause", {"job_id": "j9"}, payload=job("j9", enabled=False))
        self.assertTrue(result.ok, result.content)
        self.assertEqual(calls[0].get_method(), "POST")
        self.assertTrue(calls[0].full_url.endswith("/j9/pause"))
        self.assertIn("paused", result.content)

    def test_resume_posts_resume_endpoint(self):
        result, calls = self._action("resume", {"job_id": "j9"}, payload=job("j9", enabled=True))
        self.assertTrue(result.ok, result.content)
        self.assertTrue(calls[0].full_url.endswith("/j9/resume"))
        self.assertIn("resumed", result.content)
        self.assertIn("(enabled)", result.content)

    def test_missing_job_id_rejected_without_http(self):
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            for action in ("remove", "pause", "resume"):
                result = self.tool().invoke(tc({"action": action}), None)
                self.assertFalse(result.ok)
                self.assertIn("job_id is required", result.content)
        urlopen.assert_not_called()

    def test_unknown_job_404_surfaces_error(self):
        result, _ = self._action(
            "pause",
            {"job_id": "ghost"},
            status=404,
            payload={"error": {"code": "not_found", "message": "scheduled job not found"}},
        )
        self.assertFalse(result.ok)
        self.assertIn("HTTP 404", result.content)
        self.assertIn("not found", result.content)


class CronRegistryTests(unittest.TestCase):
    def test_cron_registered_in_builtin_registry(self):
        self.assertIs(bt._BUILTIN_REGISTRY["cron"], bt.CronTool)

    def test_build_builtin_tools_instantiates_enabled_cron(self):
        ctx = bt.BuiltinToolContext(
            workspace_dir="/tmp/unused",
            control_plane_url="http://control-plane",
            agent_credential="c",
            agent_id="a",
        )
        cfg = {"tools": [{"name": "cron", "enabled": True, "policy": {"maxActiveJobs": 3}}]}
        tools = bt.build_builtin_tools(cfg, ctx)
        self.assertEqual(len(tools), 1)
        self.assertIsInstance(tools[0], bt.CronTool)
        self.assertEqual(tools[0].max_active_jobs, 3)

    def test_schema_shape(self):
        tool = bt.CronTool({}, None)
        schema = tool.tools()[0]["function"]
        self.assertEqual(schema["name"], "cron")
        params = schema["parameters"]
        self.assertEqual(params["required"], ["action"])
        self.assertEqual(
            params["properties"]["action"]["enum"],
            ["add", "list", "remove", "pause", "resume"],
        )
        self.assertIn("schedule", params["properties"])
        self.assertIn("job_id", params["properties"])


if __name__ == "__main__":
    unittest.main()
