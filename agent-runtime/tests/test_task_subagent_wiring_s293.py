"""S-293 wiring: the task loop actually delegates, reviews and transitions.

These drive the real ``LiteLLMTaskHandler.handle_task`` with a fake
gateway and a real (throwaway) git workspace, so the flag-off/flag-on
behaviour, the I4 commit cross-check and the rework cap are observed
end-to-end rather than only in the unit tests.
"""

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

# Importing test_runtime installs the stubbed composed-prompt fetcher.
from test_runtime import fake_completion, s263_config  # noqa: F401

from skquad_runtime import git_workspace
from skquad_runtime import task_subagent as ts
from skquad_runtime.runtime import (
    LiteLLMTaskHandler,
    RuntimeSnapshot,
    RuntimeTask,
    bootstrap_status,
    runtime_metrics_text,
)
from skquad_runtime.workspace import WorkspaceHandle

RESULT_TEMPLATE = {
    "schema": "skquad.task_result.v1",
    "succeeded": True,
    "summary": "Implemented the thing; tests pass.",
    "changed_files": ["a.py"],
    "commit_sha": "",
    "branch": "skquad/agent-1/task-1",
    "tests": {"run": True, "command": "pytest", "passed": 9, "failed": 0, "skipped": 0},
    "proposed_status": "done",
    "review_recommended": False,
    "review_notes": "Covered by tests.",
    "concerns": [],
    "turns_used": 4,
    "artifacts": [],
}


def result_block(**overrides) -> str:
    payload = dict(RESULT_TEMPLATE)
    payload.update({k: v for k, v in overrides.items() if v is not None or k in overrides})
    return "Done.\n```task_result\n" + json.dumps(payload) + "\n```"


def scripted_completion(script):
    """Fake gateway returning scripted responses; records every call."""
    calls = []

    def completion(**kwargs):
        calls.append(kwargs)
        item = script[min(len(calls), len(script)) - 1]
        if isinstance(item, Exception):
            raise item
        return item

    completion.calls = calls
    return completion


def make_git_repo(tmp: str) -> tuple[Path, str]:
    repo = Path(tmp) / "repo"
    repo.mkdir(parents=True, exist_ok=True)

    def run(*args):
        subprocess.run(
            ["git", *args],
            cwd=repo,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    run("init", "-q")
    run("config", "user.email", "agent@skquad.local")
    run("config", "user.name", "skquad/agent-1")
    (repo / "seed.txt").write_text("seed", encoding="utf-8")
    run("add", "-A")
    run("commit", "-q", "-m", "seed")
    return repo, git_workspace.head_sha(repo)


def handle_env(**extra) -> dict:
    env = {"SKQUAD_TASK_SUBAGENT_ENABLED": "true"}
    env.update(extra)
    return env


def task(task_id="task-1", **overrides) -> RuntimeTask:
    base = dict(
        id=task_id,
        squad_id="squad-1",
        title="Do the thing",
        description="Detailed description",
        status="in-progress",
        assignee_agent_id="agent-1",
        execution_id=f"exec-{task_id}",
        worker_id="worker-1",
        fencing_token=f"fence-{task_id}",
        lease_expires_at="2026-10-24T02:00:00Z",
        task_number=293,
        reviewer_feedback="",
    )
    base.update(overrides)
    return RuntimeTask(**base)


class FlagOffTest(unittest.TestCase):
    def test_legacy_loop_runs_and_records_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            ts.reset_task_subagent_metrics()
            config = s263_config(tmp)
            completion = scripted_completion([fake_completion("SKQUAD_STATUS: done\nLegacy path.")])
            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)

            result = handler.handle_task(task(), config)

            self.assertEqual(result.status, "done")
            self.assertIsNone(result.review_needed)
            self.assertEqual(ts.task_subagent_metrics()["runs"], {})

    def test_flag_off_is_the_default(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertFalse(ts.task_subagent_enabled())


class FlagOnHappyPathTest(unittest.TestCase):
    def test_self_approves_when_commit_verified_in_workspace(self):
        with tempfile.TemporaryDirectory() as tmp:
            ts.reset_task_subagent_metrics()
            repo, sha = make_git_repo(tmp)
            config = s263_config(tmp)
            completion = scripted_completion([fake_completion(result_block(commit_sha=sha))])
            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            handler.attach_review_workspace(
                WorkspaceHandle(
                    resource_id="repo-1",
                    path=repo,
                    branch="skquad/agent-1/task-1",
                    push_url="https://example.invalid/repo.git",
                )
            )

            with mock.patch.dict(os.environ, handle_env(), clear=False):
                result = handler.handle_task(task(), config)

            self.assertEqual(result.status, "done")
            self.assertFalse(result.review_needed)
            self.assertIn("9 passed", result.justification)
            self.assertIn("verified in workspace", result.justification)
            # exactly one subagent run, no rework
            metrics = ts.task_subagent_metrics()
            self.assertEqual(metrics["reworks"], 0)
            self.assertEqual(sum(metrics["runs"].values()), 1)

    def test_brief_carries_reviewer_feedback_verbatim(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo, sha = make_git_repo(tmp)
            config = s263_config(tmp)
            feedback = "Ross: rate limit must be per-IP, not global."
            completion = scripted_completion([fake_completion(result_block(commit_sha=sha))])
            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            handler.attach_review_workspace(
                WorkspaceHandle(
                    resource_id="repo-1",
                    path=repo,
                    branch="b",
                    push_url="https://example.invalid/repo.git",
                )
            )

            with mock.patch.dict(os.environ, handle_env(), clear=False):
                handler.handle_task(task(reviewer_feedback=feedback), config)

            user_message = completion.calls[0]["messages"][1]["content"]
            self.assertIn(feedback, user_message)
            self.assertIn("skquad.task_brief.v1", user_message)
            self.assertIn("293", user_message)  # task_number reached the brief

    def test_subagent_calls_carry_task_metering(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo, sha = make_git_repo(tmp)
            config = s263_config(tmp)
            completion = scripted_completion([fake_completion(result_block(commit_sha=sha))])
            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            handler.attach_review_workspace(
                WorkspaceHandle(
                    resource_id="r", path=repo, branch="b", push_url="https://x.invalid/r.git"
                )
            )

            with mock.patch.dict(os.environ, handle_env(), clear=False):
                handler.handle_task(task(), config)

            metadata = completion.calls[0]["extra_body"]["litellm_metadata"]
            self.assertTrue(metadata["skquad_subagent"])
            self.assertEqual(metadata["skquad_task_id"], "task-1")
            self.assertEqual(metadata["skquad_agent_id"], "agent-1")


class FlagOnReviewTest(unittest.TestCase):
    def _run(self, tmp, block_kwargs, *, sha_override=None, repo_dirty=False):
        repo, sha = make_git_repo(tmp)
        if repo_dirty:
            (repo / "loose.txt").write_text("uncommitted", encoding="utf-8")
        config = s263_config(tmp)
        completion = scripted_completion(
            [fake_completion(result_block(commit_sha=sha_override if sha_override is not None else sha, **block_kwargs))]
        )
        handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
        handler.attach_review_workspace(
            WorkspaceHandle(resource_id="r", path=repo, branch="b", push_url="https://x.invalid/r.git")
        )
        with mock.patch.dict(os.environ, handle_env(), clear=False):
            return handler.handle_task(task(), config)

    def test_review_recommended_goes_to_review(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self._run(tmp, {"review_recommended": True})
            self.assertEqual(result.status, "in-review")
            self.assertTrue(result.review_needed)

    def test_sha_mismatch_forces_review(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self._run(tmp, {}, sha_override="deadbeefdeadbeef")
            self.assertEqual(result.status, "in-review")
            self.assertTrue(result.review_needed)
            self.assertIn("does not match workspace HEAD", result.summary)

    def test_missing_sha_forces_review(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self._run(tmp, {}, sha_override="")
            self.assertEqual(result.status, "in-review")
            self.assertIn("no commit_sha", result.summary)

    def test_uncommitted_work_surfaces_as_concern(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self._run(tmp, {}, repo_dirty=True)
            self.assertIn("uncommitted", result.summary)
            self.assertEqual(result.status, "in-review")


class FlagOnReworkTest(unittest.TestCase):
    def test_rework_cap_then_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            ts.reset_task_subagent_metrics()
            repo, sha = make_git_repo(tmp)
            config = s263_config(tmp)
            failing = fake_completion(
                result_block(
                    commit_sha=sha,
                    succeeded=False,
                    failure_class="tool_error",
                    proposed_status="blocked",
                    summary="test runner missing",
                )
            )
            completion = scripted_completion([failing])
            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            handler.attach_review_workspace(
                WorkspaceHandle(resource_id="r", path=repo, branch="b", push_url="https://x/i.git")
            )

            with mock.patch.dict(
                os.environ, handle_env(SKQUAD_TASK_REWORK_MAX="2"), clear=False
            ):
                result = handler.handle_task(task(), config)

            self.assertEqual(result.status, "blocked")
            # 1 initial + 2 reworks = 3 subagent runs, and never a 4th.
            self.assertEqual(len(completion.calls), 3)
            metrics = ts.task_subagent_metrics()
            self.assertEqual(metrics["reworks"], 2)

    def test_rework_brief_carries_prior_result_and_fixes(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo, sha = make_git_repo(tmp)
            config = s263_config(tmp)
            failing = fake_completion(
                result_block(
                    commit_sha=sha,
                    succeeded=False,
                    failure_class="tool_error",
                    proposed_status="blocked",
                    summary="missing test",
                )
            )
            completion = scripted_completion(
                [failing, fake_completion(result_block(commit_sha=sha))]
            )
            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            handler.attach_review_workspace(
                WorkspaceHandle(resource_id="r", path=repo, branch="b", push_url="https://x/i.git")
            )

            with mock.patch.dict(os.environ, handle_env(), clear=False):
                result = handler.handle_task(task(), config)

            self.assertEqual(result.status, "done")
            second_brief = completion.calls[1]["messages"][1]["content"]
            self.assertIn("REWORK attempt", second_brief)
            self.assertIn("prior_result", second_brief)
            self.assertIn("missing test", second_brief)


class MetricsTest(unittest.TestCase):
    def test_metrics_text_exposes_task_subagent_rollups(self):
        with tempfile.TemporaryDirectory() as tmp:
            ts.reset_task_subagent_metrics()
            ts.record_task_subagent_run(outcome="complete", failure_class="", turns_used=7)
            ts.record_task_subagent_run(outcome="block", failure_class="turn_budget", turns_used=48)
            ts.note_rework()
            config = s263_config(tmp)
            text = runtime_metrics_text(bootstrap_status(config), RuntimeSnapshot())
            self.assertIn("skquad_task_subagent_runs_total", text)
            self.assertIn('outcome="block",failure_class="turn_budget"', text)
            self.assertIn("skquad_task_rework_total", text)
            self.assertIn("skquad_task_subagent_turns_used_sum", text)
            self.assertIn("55", text)
            ts.reset_task_subagent_metrics()


if __name__ == "__main__":
    unittest.main()
