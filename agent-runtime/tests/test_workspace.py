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
        self.assertEqual(found, ("abc", "https://h/r.git", "dev", "project_workspace"))

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


class GitTypeWorkspaceFindTests(unittest.TestCase):
    """S-T6: TG-series git grants feed the workspace pipeline via the
    gateway proxy when the grant names exactly one repo."""

    def _git_resource(self, repos, rid="git-1", constraints_extra=None):
        constraints = {"repos_allow": repos, "allow_push": True}
        constraints.update(constraints_extra or {})
        return {
            "resource_type": "git",
            "resource_id": rid,
            "endpoint": "",
            "manifest": {},
            "constraints": constraints,
        }

    def test_single_repo_grant_matches(self):
        found = ws.find_git_workspace([self._git_resource(["rossbrigoli/skquad"])])
        self.assertEqual(found, ("git-1", "rossbrigoli/skquad", "main", "git"))

    def test_multi_repo_grant_is_ambiguous(self):
        self.assertIsNone(
            ws.find_git_workspace(
                [self._git_resource(["rossbrigoli/skquad", "rossbrigoli/k3s-cluster"])]
            )
        )

    def test_glob_repo_is_ambiguous(self):
        # Sherlock's real grant shape: repos_allow: ["rossbrigoli/*"] —
        # one entry, many repos. Must NOT auto-prepare a literal glob URL.
        self.assertIsNone(ws.find_git_workspace([self._git_resource(["rossbrigoli/*"])]))

    def test_empty_repos_allow_skipped(self):
        self.assertIsNone(ws.find_git_workspace([self._git_resource([])]))

    def test_legacy_still_wins_order(self):
        resources = [
            self._git_resource(["a/b"], rid="git-9"),
            _resource(rid="pw-1", endpoint="https://h/r.git"),
        ]
        found = ws.find_git_workspace(resources)
        self.assertEqual(found, ("git-9", "a/b", "main", "git"))


class GitTypePrepareTests(unittest.TestCase):
    """S-T6: git-type prepare builds the gateway proxy clone URL and
    needs no mounted workspace token (the credential helper authenticates
    the agent under its own identity)."""

    def _resource(self):
        return {
            "resource_type": "git",
            "resource_id": "git-1",
            "endpoint": "",
            "manifest": {},
            "constraints": {"repos_allow": ["rossbrigoli/skquad"], "allow_push": True},
        }

    def test_prepare_uses_gateway_clone_url_without_token(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = {
                "SKQUAD_TOOL_GATEWAY_URL": "http://skquad-tool-gateway.skquad-system.svc.cluster.local:8080/"
            }
            with mock.patch.dict(os.environ, env, clear=False), mock.patch.object(
                ws.git_workspace, "prepare_workspace"
            ) as prep:
                handle = ws.prepare_task_workspace(
                    [self._resource()],
                    "agent1",
                    "task1",
                    workspaces_dir=Path(tmp) / "creds",  # intentionally empty
                    base_dest=Path(tmp) / "work",
                )
            self.assertIsNotNone(handle)
            clone_url = prep.call_args[0][0]
            self.assertEqual(
                clone_url,
                "http://skquad-tool-gateway.skquad-system.svc.cluster.local:8080"
                "/git/git-1/rossbrigoli/skquad.git",
            )
            self.assertEqual(handle.push_url, clone_url)
            self.assertEqual(handle.branch, "skquad/agent1/task1")

    def test_prepare_without_gateway_env_returns_none(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = dict(os.environ)
            env.pop("SKQUAD_TOOL_GATEWAY_URL", None)
            with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(
                ws.git_workspace, "prepare_workspace"
            ) as prep:
                handle = ws.prepare_task_workspace(
                    [self._resource()],
                    "agent1",
                    "task1",
                    workspaces_dir=Path(tmp) / "creds",
                    base_dest=Path(tmp) / "work",
                )
            self.assertIsNone(handle)
            prep.assert_not_called()


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
            self.assertIsNone(task_dir)
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


class CheckFreeSpaceTest(unittest.TestCase):
    """S-139: best-effort free-space probe for the workspace filesystem."""

    def test_passes_when_free_above_floor(self):
        usage = mock.Mock(free=5 * 1024 * 1024 * 1024)
        with mock.patch.object(ws.shutil, "disk_usage", return_value=usage):
            ok, free = ws.check_free_space("/whatever", 1000)
        self.assertTrue(ok)
        self.assertEqual(free, 5 * 1024 * 1024 * 1024)

    def test_fails_when_free_below_floor(self):
        usage = mock.Mock(free=999)
        with mock.patch.object(ws.shutil, "disk_usage", return_value=usage):
            ok, free = ws.check_free_space("/whatever", 1000)
        self.assertFalse(ok)
        self.assertEqual(free, 999)

    def test_stat_failure_proceeds_with_warning(self):
        with mock.patch.object(
            ws.shutil, "disk_usage", side_effect=OSError("weird fs")
        ):
            ok, free = ws.check_free_space("/weird", 1000)
        self.assertTrue(ok)
        self.assertIsNone(free)

    def test_default_floor_is_100_mib(self):
        self.assertEqual(ws.DEFAULT_MIN_FREE_BYTES, 104_857_600)


if __name__ == "__main__":
    unittest.main()
