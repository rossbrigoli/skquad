import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from skquad_runtime import workspace as ws
from skquad_runtime import git_workspace


def _git(args, cwd):
    proc = subprocess.run(["git", *args], cwd=str(cwd), capture_output=True, text=True)
    assert proc.returncode == 0, f"git {args} failed: {proc.stderr}"
    return proc.stdout.strip()


def _seed_bare_repo(parent: Path) -> Path:
    bare = parent / "origin.git"
    subprocess.run(["git", "init", "--bare", "-b", "main", str(bare)], capture_output=True, check=True)
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


class FindWorkspaceTests(unittest.TestCase):
    def test_finds_git_workspace(self):
        found = ws.find_git_workspace([_resource(rid="abc", endpoint="https://h/r.git", branch="dev")])
        self.assertEqual(found, ("abc", "https://h/r.git", "dev"))

    def test_ignores_non_workspace(self):
        self.assertIsNone(ws.find_git_workspace([{"resource_type": "skill", "resource_id": "s"}]))

    def test_ignores_non_git_kind(self):
        self.assertIsNone(ws.find_git_workspace([_resource(kind="svn")]))

    def test_none_resources(self):
        self.assertIsNone(ws.find_git_workspace(None))

    def test_default_branch_fallback(self):
        found = ws.find_git_workspace(
            [{"resource_type": "project_workspace", "resource_id": "r", "endpoint": "https://h/r.git", "manifest": {"kind": "git"}}]
        )
        self.assertEqual(found[2], "main")


class TokenTests(unittest.TestCase):
    def test_reads_token(self):
        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "ws-1").mkdir()
            (d / "ws-1" / "token").write_text("  secret-tok  \n", encoding="utf-8")
            self.assertEqual(ws.read_workspace_token("ws-1", d), "secret-tok")

    def test_missing_token(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(ws.read_workspace_token("ws-x", Path(tmp)))


class PrepareFinalizeTests(unittest.TestCase):
    def test_prepare_and_finalize_against_local_bare(self):
        # authed_https_url requires https; for a local bare repo we bypass it by
        # patching it to return the endpoint unchanged.
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = parent / "creds"
            (wsdir / "ws-1").mkdir(parents=True)
            (wsdir / "ws-1" / "token").write_text("tok", encoding="utf-8")
            basedest = parent / "work"
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]

            with mock.patch.object(git_workspace, "authed_https_url", lambda url, token, username="": url):
                handle = ws.prepare_task_workspace(resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=basedest)
                self.assertIsNotNone(handle)
                self.assertEqual(handle.branch, "skquad/agent1/task1")
                self.assertEqual(handle.resource_id, "ws-1")
                self.assertTrue((handle.path / "README.md").exists())
                # Make a change and finalize.
                (handle.path / "result.txt").write_text("done\n", encoding="utf-8")
                result = ws.finalize_task_workspace(handle, "skquad: task1")
            self.assertTrue(result.changed)
            self.assertTrue(result.pushed)
            refs = _git(["ls-remote", "--heads", str(bare)], parent)
            self.assertIn("skquad/agent1/task1", refs)

    def test_commit_author_is_per_agent(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = parent / "creds"
            (wsdir / "ws-1").mkdir(parents=True)
            (wsdir / "ws-1" / "token").write_text("tok", encoding="utf-8")
            basedest = parent / "work"
            agent_id = "11111111-2222-3333-4444-555555555555"
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]

            with mock.patch.object(git_workspace, "authed_https_url", lambda url, token, username="": url):
                handle = ws.prepare_task_workspace(resources, agent_id, "taskA", workspaces_dir=wsdir, base_dest=basedest)
                self.assertIsNotNone(handle)
                (handle.path / "work.txt").write_text("x\n", encoding="utf-8")
                result = ws.finalize_task_workspace(handle, "skquad: taskA")
            self.assertTrue(result.pushed)

            author = _git(
                ["log", "-1", "--format=%an <%ae>", f"refs/heads/{handle.branch}"],
                bare,
            )
            self.assertEqual(author, f"skquad/{agent_id[:8]} <{agent_id}@skquad.local>")

    def test_no_token_returns_none(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            wsdir = parent / "creds"  # empty, no token
            resources = [_resource(rid="ws-1", endpoint="https://h/r.git")]
            handle = ws.prepare_task_workspace(resources, "a", "t", workspaces_dir=wsdir, base_dest=parent / "w")
            self.assertIsNone(handle)

    def test_no_workspace_returns_none(self):
        handle = ws.prepare_task_workspace([], "a", "t", workspaces_dir="/tmp", base_dest="/tmp")
        self.assertIsNone(handle)


class ResolveBaseTests(unittest.TestCase):
    def test_explicit_base_dest_wins(self):
        env = {"SKQUAD_WORKSPACE_BASE": "/env/base", "SKQUAD_WORKSPACE_MOUNT_PATH": "/mnt/pvc"}
        with mock.patch.dict(os.environ, env):
            self.assertEqual(ws.resolve_workspace_base("/explicit"), Path("/explicit"))

    def test_env_base_beats_mount(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = {"SKQUAD_WORKSPACE_BASE": str(Path(tmp) / "envbase"), "SKQUAD_WORKSPACE_MOUNT_PATH": tmp}
            with mock.patch.dict(os.environ, env):
                self.assertEqual(ws.resolve_workspace_base(), Path(tmp) / "envbase")

    def test_writable_mount_is_used(self):
        with tempfile.TemporaryDirectory() as tmp:
            with mock.patch.dict(os.environ, {"SKQUAD_WORKSPACE_MOUNT_PATH": tmp}):
                os.environ.pop("SKQUAD_WORKSPACE_BASE", None)
                self.assertEqual(ws.resolve_workspace_base(), Path(tmp))

    def test_absent_mount_falls_back_to_tmp(self):
        with mock.patch.dict(os.environ, {"SKQUAD_WORKSPACE_MOUNT_PATH": "/nonexistent-skquad-pvc"}):
            os.environ.pop("SKQUAD_WORKSPACE_BASE", None)
            self.assertEqual(ws.resolve_workspace_base(), Path("/tmp/skquad-workspaces"))


class EnsureTaskDirsTests(unittest.TestCase):
    def test_creates_task_and_scratch_dirs(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            resolved, task_dir = ws.ensure_task_dirs(base, "task-7")
            self.assertEqual(resolved, base)
            self.assertEqual(task_dir, base / "tasks" / "task-7")
            self.assertTrue(task_dir.is_dir())
            self.assertTrue((base / "scratch").is_dir())

    def test_scratch_only_without_task_id(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "pvc"
            resolved, task_dir = ws.ensure_task_dirs(base)
            self.assertEqual(task_dir, None)
            self.assertTrue((base / "scratch").is_dir())


class PvcPrepareTests(unittest.TestCase):
    def _grant(self, parent: Path) -> Path:
        wsdir = parent / "creds"
        (wsdir / "ws-1").mkdir(parents=True)
        (wsdir / "ws-1").joinpath("token").write_text("tok", encoding="utf-8")
        return wsdir

    def test_fresh_pvc_layout(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = self._grant(parent)
            pvc = parent / "pvc"
            pvc.mkdir()
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            with mock.patch.object(git_workspace, "authed_https_url", lambda url, token, username="": url):
                handle = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc
                )
            self.assertIsNotNone(handle)
            # Layout: persistent clone, per-task dir, durable scratch.
            self.assertEqual(handle.path, pvc / "git" / "ws-1")
            self.assertEqual(handle.task_dir, pvc / "tasks" / "task1")
            self.assertTrue((handle.path / ".git").is_dir())
            self.assertTrue(handle.task_dir.is_dir())
            self.assertTrue((pvc / "scratch").is_dir())
            self.assertTrue((handle.path / "README.md").exists())
            self.assertEqual(handle.branch, "skquad/agent1/task1")

    def test_warm_pvc_uses_fetch_not_clone(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = self._grant(parent)
            pvc = parent / "pvc"
            pvc.mkdir()
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            with mock.patch.object(git_workspace, "authed_https_url", lambda url, token, username="": url):
                first = ws.prepare_task_workspace(
                    resources, "agent1", "task1", workspaces_dir=wsdir, base_dest=pvc
                )
                self.assertIsNotNone(first)
                (first.path / "a.txt").write_text("x\n", encoding="utf-8")
                ws.finalize_task_workspace(first, "skquad: task1")
                # Warm wake: the persistent clone must be fetched, never re-cloned.
                with mock.patch.object(
                    git_workspace,
                    "clone_workspace",
                    side_effect=AssertionError("clone must not run on a warm PVC"),
                ):
                    second = ws.prepare_task_workspace(
                        resources, "agent1", "task2", workspaces_dir=wsdir, base_dest=pvc
                    )
            self.assertIsNotNone(second)
            self.assertEqual(second.path, first.path)
            self.assertEqual(second.branch, "skquad/agent1/task2")
            # The earlier task's committed branch survived in the persistent clone.
            _git(["rev-parse", "--verify", "refs/heads/skquad/agent1/task1"], second.path)

    def test_fallback_to_tmp_when_pvc_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            wsdir = self._grant(parent)
            resources = [_resource(rid="ws-1", endpoint=str(bare), branch="main")]
            env = {"SKQUAD_WORKSPACE_MOUNT_PATH": str(parent / "no-pvc-mounted-here")}
            with mock.patch.dict(os.environ, env):
                os.environ.pop("SKQUAD_WORKSPACE_BASE", None)
                self.addCleanup(shutil.rmtree, "/tmp/skquad-workspaces", True)
                with mock.patch.object(git_workspace, "authed_https_url", lambda url, token, username="": url):
                    handle = ws.prepare_task_workspace(
                        resources, "agent1", "task1", workspaces_dir=wsdir
                    )
            self.assertIsNotNone(handle)
            self.assertEqual(handle.path, Path("/tmp/skquad-workspaces") / "git" / "ws-1")
            self.assertEqual(handle.task_dir, Path("/tmp/skquad-workspaces") / "tasks" / "task1")
            self.assertTrue((handle.path / "README.md").exists())


if __name__ == "__main__":
    unittest.main()
