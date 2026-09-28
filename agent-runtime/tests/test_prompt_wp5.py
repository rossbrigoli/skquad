"""S-PROMPT WP5 tests — run-audit prompt sha reported to the control plane.

Covers the WP5 runtime contract:
  * ``ControlPlaneClient.start_task`` carries ``prompt_sha`` in the body.
  * ``run_task_once`` resolves the wake's prompt sha right after the
    claim and reports it via the start call (server-side audit,
    ADR-0011 D5) — before any handler/LLM work.
  * S-147 (WP6): the legacy env path and the fetch flag are removed —
    every wake reports the composed sha; a stale flag value is inert.
  * A fetch failure blocks the task and never reaches the start call.
"""

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from skquad_runtime import prompt_fetch as pf
from skquad_runtime.journal import load_journal
from skquad_runtime.runtime import (
    ControlPlaneClient,
    LiteLLMTaskHandler,
    RuntimeTask,
    run_task_once,
)

from test_prompt_wp3 import (
    SHA,
    FakeHTTPResponse,
    _SilentCP,
    _init_fetcher,
    opener_ok,
    wp3_config,
)

# BT-RUNTIME: unittest discover (CI) skips conftest.py — see test_runtime.py.
os.environ.setdefault("SKQUAD_BUILTIN_TOOLS_ENABLED", "false")


class StartTaskPayloadTest(unittest.TestCase):
    def test_start_task_sends_prompt_sha_in_body(self):
        calls = []

        def opener(req):
            calls.append(req)
            return FakeHTTPResponse(
                200,
                b'{"id":"task-1","squad_id":"squad-1","title":"T","description":"",'
                b'"status":"in-progress","assignee_agent_id":"agent-1"}',
            )

        client = ControlPlaneClient(
            "http://control-plane", "agent-1", "credential", opener=opener
        )
        task = client.start_task("task-1", SHA)

        self.assertEqual(task.id, "task-1")
        self.assertEqual(
            calls[0].full_url, "http://control-plane/api/v1/agents/me/tasks/task-1/start"
        )
        body = json.loads(calls[0].data.decode("utf-8"))
        self.assertEqual(body["prompt_sha"], SHA)

    def test_start_task_without_sha_sends_no_body(self):
        calls = []

        def opener(req):
            calls.append(req)
            return FakeHTTPResponse(
                200,
                b'{"id":"task-1","squad_id":"squad-1","title":"T","description":"",'
                b'"status":"in-progress","assignee_agent_id":"agent-1"}',
            )

        client = ControlPlaneClient(
            "http://control-plane", "agent-1", "credential", opener=opener
        )
        client.start_task("task-1")
        self.assertIsNone(calls[0].data)


def _task_payload():
    return RuntimeTask(
        id="task-1",
        squad_id="squad-1",
        title="T",
        description="D",
        status="in-progress",
        assignee_agent_id="agent-1",
        execution_id="exec-1",
        worker_id="w-1",
        fencing_token="f-1",
        lease_expires_at="",
    )


class RunTaskOnceReportsSHA(unittest.TestCase):
    """run_task_once must report the sha via start before handler work.

    S-147 (WP6): restores the real fetcher (same pattern as
    WP3EnabledMixin) because these tests force openers through it."""

    def setUp(self):
        super().setUp()
        import skquad_runtime.runtime as rt

        real = getattr(rt.PromptedRuntime, "_REAL_FETCHER_INSTANCE", None)
        if real is not None:
            previous = rt.PromptedRuntime._fetcher_instance
            self.addCleanup(setattr, rt.PromptedRuntime, "_fetcher_instance", previous)
            rt.PromptedRuntime._fetcher_instance = real

    def _run(self, *, fetch_enabled, opener, workspace_enabled=True):
        opener_forced = opener
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "ws"
            env_over = {
                "SKQUAD_WORKSPACE_ENABLED": "true" if workspace_enabled else "false",
                "SKQUAD_WORKSPACE_BASE": str(base),
            }
            config = wp3_config(tmp, **env_over)
            llm_calls = []

            def completion(**kwargs):
                llm_calls.append(kwargs)
                return {"choices": [{"message": {"content": "SKQUAD_STATUS: done\nok"}}]}

            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            fake_cp = _SilentCP()
            with mock.patch(
                "skquad_runtime.runtime.ControlPlaneClient.from_bootstrap",
                return_value=fake_cp,
            ), mock.patch.dict(
                "os.environ",
                {"SKQUAD_PROMPT_FETCH_ENABLED": "true" if fetch_enabled else "false"},
            ), mock.patch.object(
                pf.PromptFetcher,
                "__init__",
                autospec=True,
                side_effect=lambda self_, base_url, agent_id, credential, opener=None, cache=None: _init_fetcher(
                    # Force the test opener: from_runtime() never passes one.
                    self_,
                    base_url,
                    agent_id,
                    credential,
                    opener_forced,
                    cache,
                ),
            ):
                run_task_once(config, handler)
            # Read the journal INSIDE the tempdir context — it is gone after.
            journal = load_journal(base / "tasks" / "task-1")
            return fake_cp, llm_calls, journal

    def test_composed_path_reports_composed_sha(self):
        fake_cp, llm_calls, journal = self._run(fetch_enabled=True, opener=opener_ok)
        self.assertEqual(fake_cp.started, [("task-1", SHA)])
        self.assertTrue(llm_calls, "handler must still run after a successful report")
        self.assertEqual(journal["prompt_sha"], SHA)

    def test_old_fetch_flag_is_inert(self):
        # S-147 (WP6): flag off no longer means env_legacy — the composed
        # sha is reported regardless.
        fake_cp, llm_calls, _ = self._run(
            fetch_enabled=False, opener=opener_ok, workspace_enabled=False
        )
        self.assertEqual(fake_cp.started, [("task-1", SHA)])
        self.assertTrue(llm_calls)

    def test_composed_path_with_workspace_journals_sha(self):
        fake_cp, _, journal = self._run(fetch_enabled=False, opener=opener_ok)
        self.assertEqual(fake_cp.started, [("task-1", SHA)])
        self.assertEqual(journal["prompt_sha"], SHA)

    def test_fetch_failure_blocks_before_start(self):
        from urllib import error

        def opener_bad(req):
            raise error.HTTPError(req.full_url, 500, "boom", {}, None)

        fake_cp, llm_calls, _ = self._run(
            fetch_enabled=True, opener=opener_bad, workspace_enabled=False
        )
        self.assertEqual(fake_cp.started, [], "no start report after a failed fetch")
        self.assertEqual(llm_calls, [], "no LLM call after a failed fetch")
        self.assertTrue(
            any("prompt fetch failed" in s for s in fake_cp.blocked_summaries)
        )


if __name__ == "__main__":
    unittest.main()
