"""S-137 crash-resume semantics: journal, resume detection, worktree hygiene, TTL GC."""

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from dataclasses import replace
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

from skquad_runtime import git_workspace, journal, workspace as ws
from skquad_runtime.runtime import (
    LiteLLMTaskHandler,
    TaskResult,
    load_bootstrap_config,
    run_task_once,
)

# Reuse the fakes from the runtime test module.
from tests.test_runtime import (
    FakeControlPlaneClient,
    StaticTaskHandler,
    fake_completion,
    fake_task,
)


class WorkspaceGrantClient(FakeControlPlaneClient):
    """Fake control plane that grants a git workspace resource."""

    def __init__(self, claimed_task, resources):
        super().__init__(claimed_task)
        self._resources = resources

    def task_context(self, task_id):
        from skquad_runtime.runtime import RuntimeTaskContext

        return RuntimeTaskContext(
            task=fake_task(task_id),
            resources=self._resources,
            memory=[],
            limits={},
        )


def _git(args, cwd):
    proc = subprocess.run(["git", *args], cwd=str(cwd), capture_output=True, text=True)
    assert proc.returncode == 0, f"git {args} failed: {proc.stderr}"
    return proc.stdout.strip()


def _seed_bare_repo(parent: Path) -> Path:
    bare = parent / "origin.git"
    subprocess.run(
        ["git", "init", "--bare", "-b", "main", str(bare)], capture_output=True, check=True
    )
    seed = parent / "seed"
    _git(["clone", str(bare), str(seed)], parent)
    (seed / "README.md").write_text("base\n", encoding="utf-8")
    _git(["add", "-A"], seed)
    _git(["-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "seed"], seed)
    _git(["push", "origin", "main"], seed)
    return bare


def _resource(rid="ws-1", endpoint="https://x/repo.git", kind="git", branch="main"):
    return {
        "resource_type": "project_workspace",
        "resource_id": rid,
        "endpoint": endpoint,
        "manifest": {"kind": kind, "default_branch": branch},
    }


def _grant(parent: Path) -> Path:
    wsdir = parent / "creds"
    (wsdir / "ws-1").mkdir(parents=True)
    (wsdir / "ws-1" / "token").write_text("tok", encoding="utf-8")
    return wsdir


class JournalRoundTripTests(unittest.TestCase):
    def test_fresh_init_sets_started_at_and_empty_lists(self):
        with tempfile.TemporaryDirectory() as tmp:
            data = journal.init_journal(tmp, "task-1", resumed=False)
            self.assertEqual(data["task_id"], "task-1")
            self.assertTrue(data["started_at"])
            self.assertEqual(data["resumed_at"], [])
            self.assertEqual(data["steps_done"], [])
            on_disk = json.loads((Path(tmp) / journal.JOURNAL_NAME).read_text(encoding="utf-8"))
            self.assertEqual(on_disk["task_id"], "task-1")

    def test_resume_init_appends_resumed_at_and_preserves_steps(self):
        with tempfile.TemporaryDirectory() as tmp:
            journal.init_journal(tmp, "task-1", resumed=False)
            journal.journal_start_step(tmp, "step-a")
            journal.journal_complete_step(tmp, "step-a", artifacts=["out.txt"])
            data = journal.init_journal(tmp, "task-1", resumed=True)
            self.assertEqual(data["steps_done"], ["step-a"])
            self.assertEqual(data["artifacts"], ["out.txt"])
            self.assertEqual(len(data["resumed_at"]), 1)
            # A second resume appends again.
            data = journal.init_journal(tmp, "task-1", resumed=True)
            self.assertEqual(len(data["resumed_at"]), 2)

    def test_start_and_complete_step_roundtrip(self):
        with tempfile.TemporaryDirectory() as tmp:
            journal.journal_start_step(tmp, "build")
            data = journal.load_journal(tmp)
            self.assertEqual(data["current_step"], "build")
            journal.journal_complete_step(tmp, "build", artifacts=["app.bin", "app.bin"])
            data = journal.load_journal(tmp)
            self.assertEqual(data["steps_done"], ["build"])
            self.assertEqual(data["current_step"], "")
            # Artifacts are de-duplicated.
            self.assertEqual(data["artifacts"], ["app.bin"])

    def test_corrupt_journal_treated_as_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / journal.JOURNAL_NAME).write_text("{not json", encoding="utf-8")
            self.assertIsNone(journal.load_journal(tmp))
            # Completing a step still works (rebuilds a fresh journal).
            data = journal.journal_complete_step(tmp, "x")
            self.assertEqual(data["steps_done"], ["x"])

    def test_journal_write_failure_is_swallowed(self):
        with tempfile.TemporaryDirectory() as tmp:
            with mock.patch.object(Path, "write_text", side_effect=OSError("disk full")):
                self.assertFalse(journal.save_journal(tmp, {"a": 1}))


class PriorStateTests(unittest.TestCase):
    def test_journal_only_dir_is_not_prior_state(self):
        with tempfile.TemporaryDirectory() as tmp:
            journal.init_journal(tmp, "task-1", resumed=False)
            self.assertFalse(journal.task_dir_has_prior_state(tmp))

    def test_missing_dir_is_not_prior_state(self):
        self.assertFalse(journal.task_dir_has_prior_state("/nonexistent-skquad-task-dir"))

    def test_files_present_is_prior_state(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "script.sh").write_text("echo hi", encoding="utf-8")
            self.assertTrue(journal.task_dir_has_prior_state(tmp))


class ResumeNoteTests(unittest.TestCase):
    def test_note_lists_artifacts_steps_and_current_step(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "script.sh").write_text("echo hi", encoding="utf-8")
            journal.journal_start_step(tmp, "deploy")
            journal.journal_complete_step(tmp, "build", artifacts=["dist.tar"])
            note = journal.build_resume_note(tmp)
            self.assertIn("[skquad resume]", note)
            self.assertIn("script.sh", note)
            self.assertIn("build", note)
            self.assertIn("'deploy'", note)
            self.assertIn("cannot be replayed mid-step", note)

    def test_note_includes_git_branch_and_local_commits(self):
        git_info = {
            "branch": "skquad/agent-1/task-1",
            "local_commits": 2,
            "local_commits_oneline": ["abc1234 work one", "def5678 work two"],
            "dirty_files": ["M src/app.py", "?? notes.md"],
        }
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "x.txt").write_text("x", encoding="utf-8")
            note = journal.build_resume_note(tmp, git_info=git_info)
            self.assertIn("skquad/agent-1/task-1", note)
            self.assertIn("Local commits NOT yet on the remote: 2", note)
            self.assertIn("abc1234 work one", note)
            self.assertIn("M src/app.py", note)

    def test_note_is_bounded(self):
        with tempfile.TemporaryDirectory() as tmp:
            for i in range(100):
                (Path(tmp) / f"file_{i}.txt").write_text("x" * 50, encoding="utf-8")
            note = journal.build_resume_note(tmp, max_chars=300)
            self.assertLessEqual(len(note), 300 + len("\n[truncated]"))
            self.assertTrue(note.endswith("[truncated]"))

    def test_empty_note_when_nothing_to_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(journal.build_resume_note(tmp), "")

    def test_resume_note_roundtrip_via_journal(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "a.txt").write_text("a", encoding="utf-8")
            self.assertEqual(journal.read_resume_note(tmp), "")
            journal.set_resume_note(tmp, "the note")
            self.assertEqual(journal.read_resume_note(tmp), "the note")


class GitResumeInfoTests(unittest.TestCase):
    def test_snapshot_reports_branch_local_commits_and_dirty(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            clone = parent / "clone"
            _git(["clone", str(bare), str(clone)], parent)
            _git(["checkout", "-b", "work"], clone)
            (clone / "new.txt").write_text("committed locally\n", encoding="utf-8")
            _git(["add", "-A"], clone)
            _git(["-c", "user.email=a@a", "-c", "user.name=a", "commit", "-m", "local only"], clone)
            (clone / "dirty.txt").write_text("uncommitted\n", encoding="utf-8")

            info = journal.git_resume_info(clone)
            self.assertEqual(info["branch"], "work")
            self.assertEqual(info["local_commits"], 1)
            self.assertIn("local only", info["local_commits_oneline"][0])
            self.assertIn("?? dirty.txt", " ".join(info["dirty_files"]))

    def test_snapshot_none_for_non_repo(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(journal.git_resume_info(tmp))


class WorktreeHygieneTests(unittest.TestCase):
    def test_corrupt_clone_moved_aside_and_recloned(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = _grant(parent)
            pvc = parent / "pvc"
            pvc.mkdir()
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            with mock.patch.object(
                git_workspace, "authed_https_url", lambda url, token, username="": url
            ):
                first = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc
                )
                self.assertIsNotNone(first)
                # Corrupt the clone: .git dir exists but the repo is broken.
                shutil.rmtree(first.path / ".git" / "objects")

                self.assertFalse(ws.clone_is_healthy(first.path))
                second = ws.prepare_task_workspace(
                    resources, "agent1", "task2", workspaces_dir=wsdir, base_dest=pvc
                )
            self.assertIsNotNone(second)
            # A fresh healthy clone took its place at the canonical path.
            self.assertEqual(second.path, pvc / "git" / "ws-1")
            self.assertTrue(ws.clone_is_healthy(second.path))
            self.assertTrue((second.path / "README.md").exists())
            # The corrupt clone was renamed aside, never deleted.
            corrupt_dirs = list((pvc / "git").glob("ws-1.corrupt.*"))
            self.assertEqual(len(corrupt_dirs), 1)
            self.assertTrue(corrupt_dirs[0].exists())

    def test_healthy_clone_is_not_moved_aside(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = _grant(parent)
            pvc = parent / "pvc"
            pvc.mkdir()
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            with mock.patch.object(
                git_workspace, "authed_https_url", lambda url, token, username="": url
            ):
                first = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc
                )
                second = ws.prepare_task_workspace(
                    resources, "agent1", "task2", workspaces_dir=wsdir, base_dest=pvc
                )
            self.assertIsNotNone(second)
            self.assertEqual(list((pvc / "git").glob("*.corrupt.*")), [])
            self.assertTrue(ws.clone_is_healthy(first.path))

    def test_resume_snapshot_captured_before_sync_discards(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = _grant(parent)
            pvc = parent / "pvc"
            pvc.mkdir()
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            with mock.patch.object(
                git_workspace, "authed_https_url", lambda url, token, username="": url
            ):
                first = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc
                )
                # Crash leaves a committed local commit on the task branch
                # plus an uncommitted dirty file.
                (first.path / "work.txt").write_text("done\n", encoding="utf-8")
                ws.finalize_task_workspace(first, "skquad: task1 wip")
                (first.path / "leftover.txt").write_text("crash leftover\n", encoding="utf-8")

                # Simulate the crash re-queue of the SAME task.
                (first.task_dir / "scratch.py").write_text("print(1)\n", encoding="utf-8")
                second = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc,
                    resumed=True,
                )
            self.assertIsNotNone(second)
            self.assertTrue(second.resumed)
            # The pre-sync snapshot surfaced the local commit and the dirty file.
            self.assertEqual(second.pre_sync_git["branch"], "skquad/agent1/task1")
            self.assertGreaterEqual(second.pre_sync_git["local_commits"], 1)
            self.assertIn("leftover.txt", " ".join(second.pre_sync_git["dirty_files"]))
            # The committed work survived the sync; the resume note says so.
            self.assertIn("Local commits NOT yet on the remote", second.resume_note)
            self.assertIn("scratch.py", second.resume_note)
            # Task branch commit is still present after sync.
            _git(["rev-parse", "--verify", "refs/heads/skquad/agent1/task1"], second.path)

    def test_fresh_prepare_has_no_resume_note(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = _grant(parent)
            pvc = parent / "pvc"
            pvc.mkdir()
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            with mock.patch.object(
                git_workspace, "authed_https_url", lambda url, token, username="": url
            ):
                handle = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc
                )
            self.assertIsNotNone(handle)
            self.assertFalse(handle.resumed)
            self.assertEqual(handle.resume_note, "")
            self.assertIsNone(handle.pre_sync_git)


class TtlGcTests(unittest.TestCase):
    def _make_task_dir(self, base: Path, name: str, age_days: float) -> Path:
        d = base / "tasks" / name
        d.mkdir(parents=True)
        (d / "artifact.txt").write_text("x", encoding="utf-8")
        stamp = (datetime.now(timezone.utc) - timedelta(days=age_days)).timestamp()
        os.utime(d / "artifact.txt", (stamp, stamp))
        os.utime(d, (stamp, stamp))
        return d

    def test_removes_old_spares_recent_and_current(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            old = self._make_task_dir(base, "old-task", age_days=10)
            recent = self._make_task_dir(base, "recent-task", age_days=2)
            current = self._make_task_dir(base, "current-task", age_days=30)

            removed = journal.gc_task_dirs(base, keep_task_id="current-task", ttl_days=7)

            self.assertEqual(removed, ["old-task"])
            self.assertFalse(old.exists())
            self.assertTrue(recent.exists())
            self.assertTrue(current.exists())

    def test_default_ttl_from_env(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            old = self._make_task_dir(base, "old-task", age_days=3)
            with mock.patch.dict(os.environ, {"SKQUAD_TASK_DIR_TTL_DAYS": "1"}):
                removed = journal.gc_task_dirs(base)
            self.assertEqual(removed, ["old-task"])
            self.assertFalse(old.exists())

    def test_invalid_ttl_env_uses_default(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            old = self._make_task_dir(base, "old-task", age_days=3)
            with mock.patch.dict(os.environ, {"SKQUAD_TASK_DIR_TTL_DAYS": "not-a-number"}):
                removed = journal.gc_task_dirs(base)
            # Default 7 days: a 3-day-old dir survives.
            self.assertEqual(removed, [])
            self.assertTrue(old.exists())

    def test_zero_ttl_disables_gc(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            old = self._make_task_dir(base, "old-task", age_days=999)
            self.assertEqual(journal.gc_task_dirs(base, ttl_days=0), [])
            self.assertTrue(old.exists())

    def test_missing_tasks_root_is_noop(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(journal.gc_task_dirs(Path(tmp) / "nope"), [])

    def test_removal_failure_is_swallowed(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            self._make_task_dir(base, "old-task", age_days=10)
            with mock.patch.object(
                journal.shutil, "rmtree", side_effect=OSError("busy")
            ):
                removed = journal.gc_task_dirs(base, ttl_days=7)
            self.assertEqual(removed, [])


class RunTaskResumeIntegrationTests(unittest.TestCase):
    """End-to-end through run_task_once: claim -> journal -> resume note."""

    def _config(self, tmp: str):
        credential = Path(tmp) / "agent"
        credential.write_text("credential", encoding="utf-8")
        return load_bootstrap_config(
            {
                "SKQUAD_AGENT_ID": "agent-1",
                "SKQUAD_SQUAD_ID": "squad-1",
                "SKQUAD_AGENT_CREDENTIAL_PATH": str(credential),
                "SKQUAD_TASK_LOOP_ENABLED": "false",
            }
        )

    def test_fresh_claim_writes_journal_and_no_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = replace(self._config(tmp), workspace_base=str(Path(tmp) / "pvc"))
            client = FakeControlPlaneClient(claimed_task=fake_task("task-1"))
            handler = StaticTaskHandler(TaskResult(status="done"))
            with mock.patch.dict(os.environ, {}, clear=False):
                os.environ.pop("SKQUAD_TASK_DIR", None)
                run_task_once(config, handler, client)
                self.assertEqual(os.environ.get("SKQUAD_TASK_RESUMED"), "0")
            task_dir = Path(tmp) / "pvc" / "tasks" / "task-1"
            data = journal.load_journal(task_dir)
            self.assertIsNotNone(data)
            self.assertEqual(data["task_id"], "task-1")
            self.assertEqual(data["resumed_at"], [])
            self.assertEqual(journal.read_resume_note(task_dir), "")

    def test_crash_requeue_resumes_with_note_in_context(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = replace(self._config(tmp), workspace_base=str(Path(tmp) / "pvc"))
            task_dir = Path(tmp) / "pvc" / "tasks" / "task-1"
            # Simulate the crashed run's leftovers.
            task_dir.mkdir(parents=True)
            (task_dir / "half_done.sh").write_text("#!/bin/sh\n", encoding="utf-8")
            journal.journal_start_step(task_dir, "analyze")
            journal.journal_complete_step(task_dir, "fetch")
            # A prior resume is already recorded, so the git snapshot is
            # captured before sync (first resume of a fresh clone has no
            # meaningful pre-sync state).
            prior = journal.init_journal(task_dir, "task-1", resumed=True)
            prior_resumes = len(prior["resumed_at"])
            # The crashed run left a committed-but-unpushed change on the
            # task branch in the persistent clone.
            wsdir = _grant(Path(tmp))
            with mock.patch.object(
                git_workspace, "authed_https_url", lambda url, token, username="": url
            ):
                ws.prepare_task_workspace(
                    [
                        _resource(
                            rid="ws-1",
                            endpoint=str(_seed_bare_repo(Path(tmp))),
                            branch="main",
                        )
                    ],
                    "agent-1",
                    "task-1",
                    workspaces_dir=wsdir,
                    base_dest=Path(tmp) / "pvc",
                )
            clone = Path(tmp) / "pvc" / "git" / "ws-1"
            (clone / "wip.txt").write_text("committed before crash\n", encoding="utf-8")
            _git(["add", "-A"], clone)
            _git(
                [
                    "-c",
                    "user.email=a@skquad.local",
                    "-c",
                    "user.name=skquad",
                    "commit",
                    "-m",
                    "wip before crash",
                ],
                clone,
            )

            client = WorkspaceGrantClient(
                fake_task("task-1"),
                [_resource(rid="ws-1", endpoint=str(Path(tmp) / "origin.git"), branch="main")],
            )
            config = replace(
                self._config(tmp),
                workspace_base=str(Path(tmp) / "pvc"),
                workspaces_dir=str(wsdir),
            )
            seen = {}

            class CapturingHandler:
                def handle_task(self, task, config):
                    seen["resumed_env"] = os.environ.get("SKQUAD_TASK_RESUMED")
                    # The note the LLM would see: read from the journal the
                    # runtime just prepared (SKQUAD_TASK_DIR is exported).
                    task_dir_now = journal.resolve_task_dir(config.workspace_base, task.id)
                    seen["note"] = journal.read_resume_note(task_dir_now)
                    return TaskResult(status="done")

            with mock.patch.dict(os.environ, {}, clear=False):
                os.environ.pop("SKQUAD_TASK_DIR", None)
                with mock.patch.object(
                    git_workspace, "authed_https_url", lambda url, token, username="": url
                ):
                    run_task_once(config, CapturingHandler(), client)
                self.assertEqual(seen["resumed_env"], "1")
            data = journal.load_journal(task_dir)
            self.assertEqual(len(data["resumed_at"]), prior_resumes + 1)
            self.assertIn("fetch", data["steps_done"])
            # The handler-visible note carries the prior artifacts and steps.
            self.assertIn("half_done.sh", seen["note"])
            self.assertIn("fetch", seen["note"])
            self.assertIn("'analyze'", seen["note"])
            # The resume note was persisted in the journal by the runtime.
            self.assertIn("[skquad resume]", data["resume_note"])
            self.assertIn("skquad/agent-1/task-1", data["resume_note"])
            # The pre-sync git snapshot is visible in the note too.
            self.assertIn("Git workspace branch", data["resume_note"])

    def test_gc_runs_at_task_start_and_spares_current(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = replace(self._config(tmp), workspace_base=str(Path(tmp) / "pvc"))
            tasks = Path(tmp) / "pvc" / "tasks"
            old = tasks / "ancient-task"
            old.mkdir(parents=True)
            (old / "x.txt").write_text("x", encoding="utf-8")
            stamp = (datetime.now(timezone.utc) - timedelta(days=30)).timestamp()
            os.utime(old / "x.txt", (stamp, stamp))
            os.utime(old, (stamp, stamp))

            client = FakeControlPlaneClient(claimed_task=fake_task("task-new"))
            handler = StaticTaskHandler(TaskResult(status="done"))
            with mock.patch.dict(os.environ, {"SKQUAD_TASK_DIR_TTL_DAYS": "7"}, clear=False):
                os.environ.pop("SKQUAD_TASK_DIR", None)
                run_task_once(config, handler, client)
            self.assertFalse(old.exists())
            self.assertTrue((tasks / "task-new").is_dir())


class ResumeNoteInLlmPromptTests(unittest.TestCase):
    def _handler_config(self, tmp: str):
        virtual_key = Path(tmp) / "llm-gateway"
        virtual_key.write_text("virtual-key", encoding="utf-8")
        return load_bootstrap_config(
            {
                "SKQUAD_AGENT_ID": "agent-1",
                "SKQUAD_SQUAD_ID": "squad-1",
                "SKQUAD_AGENT_CREDENTIAL_PATH": str(Path(tmp) / "agent"),
                "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH": str(virtual_key),
                "SKQUAD_LLM_GATEWAY_URL": "http://gateway",
                "SKQUAD_DEFAULT_MODEL": "model-1",
            }
        )

    def test_resume_note_appended_to_user_prompt(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = self._handler_config(tmp)
            task_dir = Path(tmp) / "tasks" / "task-1"
            task_dir.mkdir(parents=True)
            (task_dir / "prior.txt").write_text("x", encoding="utf-8")
            journal.set_resume_note(task_dir, "[skquad resume] prior work exists")
            calls = []

            def completion(**kwargs):
                calls.append(kwargs)
                return fake_completion("SKQUAD_STATUS: done")

            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            with mock.patch.dict(os.environ, {"SKQUAD_TASK_DIR": str(task_dir)}):
                result = handler.handle_task(fake_task("task-1"), config)
            self.assertEqual(result.status, "done")
            user_prompt = calls[0]["messages"][1]["content"]
            self.assertIn("Task: Task", user_prompt)
            self.assertIn("[skquad resume] prior work exists", user_prompt)

    def test_fresh_task_prompt_has_no_resume_section(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = self._handler_config(tmp)
            calls = []

            def completion(**kwargs):
                calls.append(kwargs)
                return fake_completion("SKQUAD_STATUS: done")

            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            with mock.patch.dict(os.environ, {}, clear=False):
                os.environ.pop("SKQUAD_TASK_DIR", None)
                # Point the base at an empty dir so no journal is found.
                config = replace(config, workspace_base=str(Path(tmp) / "empty-base"))
                result = handler.handle_task(fake_task("fresh-task"), config)
            self.assertEqual(result.status, "done")
            self.assertNotIn("[skquad resume]", calls[0]["messages"][1]["content"])


if __name__ == "__main__":
    unittest.main()
