import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from skquad_runtime import git_workspace
from skquad_runtime.git_workspace import (
    GitError,
    authed_https_url,
    clean_url,
    clone_workspace,
    commit_and_push,
    prepare_workspace,
    sync_workspace,
    work_branch_for,
)


def _git(args, cwd):
    proc = subprocess.run(["git", *args], cwd=str(cwd), capture_output=True, text=True)
    assert proc.returncode == 0, f"git {args} failed: {proc.stderr}"
    return proc.stdout.strip()


def _seed_bare_repo(parent: Path) -> Path:
    """Create a bare repo with one commit on branch 'main'."""
    bare = parent / "origin.git"
    subprocess.run(
        ["git", "init", "--bare", "-b", "main", str(bare)], capture_output=True, check=True
    )
    seed = parent / "seed"
    _git(["clone", str(bare), str(seed)], parent)
    (seed / "README.md").write_text("hello workspace\n", encoding="utf-8")
    _git(["add", "-A"], seed)
    _git(["-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "seed"], seed)
    _git(["push", "origin", "main"], seed)
    return bare


class AuthUrlTests(unittest.TestCase):
    # Assertions are structural (startswith/endswith/contains) so no inline
    # credential-looking literal appears in source that a secret-masking layer
    # could rewrite.

    def test_injects_default_user_and_token(self):
        token = "tk-" + "a" * 8
        url = authed_https_url("https://github.com/team/repo.git", token)
        self.assertTrue(url.startswith("https://x-access-token:"), url)
        self.assertTrue(url.endswith("@github.com/team/repo.git"), url)
        self.assertIn(token, url)

    def test_custom_username(self):
        token = "tk-" + "b" * 8
        url = authed_https_url("https://gitlab.com/g/p.git", token, username="robot")
        self.assertTrue(url.startswith("https://robot:"), url)
        self.assertTrue(url.endswith("@gitlab.com/g/p.git"), url)
        self.assertIn(token, url)

    def test_replaces_existing_creds(self):
        token = "tk-" + "c" * 8
        url = authed_https_url("https://old:***@host/repo.git", token)
        self.assertTrue(url.startswith("https://x-access-token:"), url)
        self.assertTrue(url.endswith("@host/repo.git"), url)
        self.assertIn(token, url)
        self.assertNotIn("old", url)

    def test_preserves_port(self):
        token = "tk-" + "d" * 8
        url = authed_https_url("https://host:8443/repo.git", token)
        self.assertTrue(url.endswith("@host:8443/repo.git"), url)

    def test_rejects_non_https(self):
        with self.assertRaises(GitError):
            authed_https_url("git@github.com:team/repo.git", "tk")

    def test_rejects_empty_token(self):
        with self.assertRaises(GitError):
            authed_https_url("https://host/repo.git", "")

    def test_clean_url_strips_userinfo(self):
        self.assertEqual(clean_url("https://u:***@host/repo.git"), "https://host/repo.git")
        self.assertEqual(clean_url("https://host/repo.git"), "https://host/repo.git")


class WorkBranchTests(unittest.TestCase):
    def test_branch_shape(self):
        self.assertEqual(work_branch_for("agent-1", "task-9"), "skquad/agent-1/task-9")

    def test_sanitizes_slashes(self):
        self.assertEqual(work_branch_for("a/b", "c d"), "skquad/a-b/c-d")


class GitOpsTests(unittest.TestCase):
    def test_prepare_clones_and_checks_out_work_branch(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            prepare_workspace(str(bare), "main", "skquad/a/t1", dest)
            self.assertEqual(_git(["rev-parse", "--abbrev-ref", "HEAD"], dest), "skquad/a/t1")
            self.assertTrue((dest / "README.md").exists())
            # remote is scrubbed to the clean (local) URL — no creds persisted.
            self.assertEqual(_git(["config", "--get", "remote.origin.url"], dest), str(bare))

    def test_commit_and_push_with_changes(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            prepare_workspace(str(bare), "main", "skquad/a/t2", dest)
            (dest / "newfile.txt").write_text("work product\n", encoding="utf-8")
            result = commit_and_push(dest, "skquad/a/t2", str(bare), "agent: did work")
            self.assertTrue(result.changed)
            self.assertTrue(result.pushed)
            self.assertEqual(len(result.commit_sha), 40)
            refs = _git(["ls-remote", "--heads", str(bare)], parent)
            self.assertIn("skquad/a/t2", refs)
            shown = _git(["show", f"{result.commit_sha}:newfile.txt"], bare)
            self.assertIn("work product", shown)

    def test_commit_and_push_no_changes(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            prepare_workspace(str(bare), "main", "skquad/a/t3", dest)
            result = commit_and_push(dest, "skquad/a/t3", str(bare), "noop")
            self.assertFalse(result.changed)
            self.assertFalse(result.pushed)
            self.assertEqual(len(result.commit_sha), 40)


class SyncWorkspaceTests(unittest.TestCase):
    def test_prepare_dispatches_to_sync_when_clone_exists(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            clone_workspace(str(bare), "main", "skquad/a/t1", dest)
            # A warm clone must never trigger a full re-clone.
            with mock.patch.object(
                git_workspace,
                "clone_workspace",
                side_effect=AssertionError("re-cloned a warm workspace"),
            ):
                prepare_workspace(str(bare), "main", "skquad/a/t2", dest)
            self.assertEqual(_git(["rev-parse", "--abbrev-ref", "HEAD"], dest), "skquad/a/t2")

    def test_sync_scrubs_token_after_fetch(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            clone_workspace(str(bare), "main", "skquad/a/t1", dest)
            # Simulate a leaked credential persisted in the remote config.
            _git(["remote", "set-url", "origin", "https://user:***@host/repo.git"], dest)
            sync_workspace(str(bare), "main", "skquad/a/t2", dest)
            self.assertEqual(_git(["config", "--get", "remote.origin.url"], dest), str(bare))

    def test_sync_recovers_existing_task_branch(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            clone_workspace(str(bare), "main", "skquad/a/t1", dest)
            (dest / "keep.txt").write_text("kept\n", encoding="utf-8")
            _git(["add", "-A"], dest)
            _git(["commit", "-m", "wip"], dest)
            # Same task branch on a warm wake: committed work is preserved.
            sync_workspace(str(bare), "main", "skquad/a/t1", dest)
            self.assertEqual(_git(["rev-parse", "--abbrev-ref", "HEAD"], dest), "skquad/a/t1")
            self.assertTrue((dest / "keep.txt").exists())

    def test_sync_discards_dirty_leftovers_when_switching(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            bare = _seed_bare_repo(parent)
            dest = parent / "ws"
            clone_workspace(str(bare), "main", "skquad/a/t1", dest)
            (dest / "dirty.txt").write_text("crash leftover\n", encoding="utf-8")
            # A new task must not be blocked by a dirty tree from a crash.
            sync_workspace(str(bare), "main", "skquad/a/t2", dest)
            self.assertEqual(_git(["rev-parse", "--abbrev-ref", "HEAD"], dest), "skquad/a/t2")
            self.assertFalse((dest / "dirty.txt").exists())

    def test_sync_requires_existing_clone(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(GitError):
                sync_workspace("https://h/r.git", "main", "skquad/a/t", Path(tmp) / "missing")


if __name__ == "__main__":
    unittest.main()
