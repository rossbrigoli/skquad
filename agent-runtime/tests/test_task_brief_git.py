"""S-T6 (2026-11-04): the task brief carries the prepared workspace handle
and the gateway git guidance so the subagent inherits a WORKING git path
under the parent agent's identity (not a stale checkout)."""

from __future__ import annotations

import unittest
from unittest import mock

from skquad_runtime.runtime import LiteLLMTaskHandler, RuntimeResource, RuntimeTask


def _task():
    return RuntimeTask(
        id="task-1",
        squad_id="squad-1",
        title="fix the thing",
        description="",
        status="in-progress",
        assignee_agent_id="agent-1",
        task_number=6,
    )


def _git_resource(**constraints):
    return RuntimeResource(
        resource_type="git",
        resource_id="git-1",
        name="github",
        description="",
        endpoint="",
        manifest={},
        constraints=constraints,
    )


class TaskBriefGitTests(unittest.TestCase):
    def _handler(self):
        return LiteLLMTaskHandler(
            completion=lambda **kw: None, discover_resources=False
        )

    def test_prepared_workspace_fields_in_brief(self):
        handler = self._handler()
        handler._review_workspace = mock.Mock(
            path="/workspace/git/git-1",
            branch="skquad/agent-1/task-1",
            resource_id="git-1",
        )
        with mock.patch.dict(
            "os.environ",
            {"SKQUAD_TOOL_GATEWAY_URL": "http://gw:8080"},
            clear=False,
        ):
            brief = handler._task_brief(_task(), resources=[_git_resource(repos_allow=["rossbrigoli/skquad"], allow_push=True)])
        ws = brief.to_payload()["workspace"]
        self.assertEqual(ws["prepared"], True)
        self.assertEqual(ws["path"], "/workspace/git/git-1")
        self.assertEqual(ws["branch"], "skquad/agent-1/task-1")
        self.assertEqual(ws["branch_hint"], "skquad/agent-1/task-1")
        self.assertEqual(ws["resource_id"], "git-1")
        self.assertEqual(ws["git"]["gateway"], "http://gw:8080/git/git-1/")
        self.assertEqual(ws["git"]["repos_allow"], ["rossbrigoli/skquad"])
        self.assertEqual(ws["git"]["allow_push"], True)

    def test_git_guidance_without_prepared_workspace(self):
        handler = self._handler()
        with mock.patch.dict(
            "os.environ",
            {"SKQUAD_TOOL_GATEWAY_URL": "http://gw:8080"},
            clear=False,
        ):
            brief = handler._task_brief(
                _task(),
                resources=[_git_resource(repos_allow=["a/b", "c/d"], allow_push=False)],
            )
        ws = brief.to_payload()["workspace"]
        self.assertNotIn("prepared", ws)
        self.assertEqual(ws["git"]["repos_allow"], ["a/b", "c/d"])
        self.assertEqual(ws["git"]["allow_push"], False)

    def test_no_git_resource_no_guidance(self):
        handler = self._handler()
        brief = handler._task_brief(_task(), resources=[])
        self.assertEqual(brief.to_payload()["workspace"], {})

    def test_no_gateway_env_no_guidance(self):
        handler = self._handler()
        import os

        env = dict(os.environ)
        env.pop("SKQUAD_TOOL_GATEWAY_URL", None)
        with mock.patch.dict("os.environ", env, clear=True):
            brief = handler._task_brief(_task(), resources=[_git_resource(repos_allow=["a/b"])])
        self.assertEqual(brief.to_payload()["workspace"], {})


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
