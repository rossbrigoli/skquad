"""Git workspace operations for agent task workspaces.

v1 supports HTTPS-token auth only. The module is deliberately split so the
pure git operations (clone/checkout/commit/push) are decoupled from the
credential/URL handling, which lets the git operations be exercised against
a local bare repo in tests while production passes an HTTPS URL with an
injected token.

Security notes (v1):
- The token is injected into the clone/push URL and URL-encoded. It is NOT
  persisted: after clone we reset ``remote.origin.url`` to the clean URL so
  the token never lands in ``.git/config``.
- The token can appear briefly in the process argument list during clone/push.
  In a single-tenant agent pod this is an accepted v1 tradeoff; a hardened
  follow-up is a ``GIT_ASKPASS`` helper that never puts the token in argv.
"""

from __future__ import annotations

import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote, urlsplit, urlunsplit


class GitError(RuntimeError):
    """Raised when a git operation fails."""


@dataclass(frozen=True)
class WorkspaceResult:
    branch: str
    commit_sha: str
    pushed: bool
    changed: bool


def _run(args: list[str], cwd: Path | None = None, check: bool = True) -> subprocess.CompletedProcess:
    proc = subprocess.run(args, cwd=str(cwd) if cwd else None, capture_output=True, text=True)
    if check and proc.returncode != 0:
        raise GitError(
            f"git {' '.join(args[:2])} failed (code {proc.returncode}): {proc.stderr.strip()}"
        )
    return proc


def authed_https_url(repo_url: str, token: str, username: str = "") -> str:
    """Return ``repo_url`` with ``username:token`` injected into an HTTPS URL.

    Only ``https://`` URLs are supported for token auth in v1. Any existing
    credentials in the URL are replaced. The username defaults to
    ``x-access-token`` (works for GitHub/GitLab personal access tokens).
    """
    parts = urlsplit(repo_url)
    if parts.scheme != "https":
        raise GitError(f"only https:// workspaces are supported for token auth, got {parts.scheme!r}")
    if not token:
        raise GitError("git token is empty")
    user = username or "x-access-token"
    host = parts.hostname or ""
    if parts.port:
        host = f"{host}:{parts.port}"
    netloc = f"{quote(user, safe='')}:{quote(token, safe='')}@{host}"
    return urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))


def clean_url(repo_url: str) -> str:
    """Strip any userinfo (user:pass@) from a URL."""
    parts = urlsplit(repo_url)
    if "@" in parts.netloc:
        host = parts.netloc.rsplit("@", 1)[-1]
        return urlunsplit((parts.scheme, host, parts.path, parts.query, parts.fragment))
    return repo_url


def _configure_repo(
    dest: Path, clone_url: str, author_name: str, author_email: str
) -> None:
    """Scrub credentials from ``remote.origin.url`` and set authorship.

    Called after every clone/fetch so the token never lands in
    ``.git/config`` regardless of the path taken.
    """
    _run(["git", "remote", "set-url", "origin", clean_url(clone_url)], cwd=dest)
    _run(["git", "config", "user.name", author_name], cwd=dest)
    _run(["git", "config", "user.email", author_email], cwd=dest)


def clone_workspace(
    clone_url: str,
    base_branch: str,
    work_branch: str,
    dest: Path,
    author_name: str = "skquad-agent",
    author_email: str = "agent@skquad.local",
) -> Path:
    """Full clone of ``clone_url`` at ``base_branch`` into ``dest``, then
    check out ``work_branch``.

    ``dest`` is removed first if it exists so the workspace is deterministic.
    ``clone_url`` may already carry credentials (production) or be a local
    path (tests).
    """
    dest = Path(dest)
    if dest.exists():
        shutil.rmtree(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    _run(["git", "clone", "--branch", base_branch, "--single-branch", clone_url, str(dest)])
    # Never persist credentials in .git/config.
    _configure_repo(dest, clone_url, author_name, author_email)
    _run(["git", "checkout", "-b", work_branch], cwd=dest)
    return dest


def sync_workspace(
    clone_url: str,
    base_branch: str,
    work_branch: str,
    dest: Path,
    author_name: str = "skquad-agent",
    author_email: str = "agent@skquad.local",
) -> Path:
    """Fetch into an existing clone at ``dest`` and check out ``work_branch``.

    Used on warm PVC wakes (S-136) so the persistent clone under
    ``<base>/git/<resource_id>/`` is reused instead of re-cloning:

    - ``git fetch`` runs against the (possibly credential-bearing)
      ``clone_url`` passed explicitly on the command line; the persisted
      ``remote.origin.url`` is re-scrubbed afterwards, so the token still
      never lands in ``.git/config``.
    - Uncommitted leftovers from a crashed run are discarded
      (``reset --hard`` + ``clean -fd``) so branch switching is
      deterministic; committed work on task branches survives.
    - If the per-task branch already exists locally (crash recovery for
      the same task) it is checked out as-is; otherwise it is (re)created
      from the freshly fetched base branch.
    """
    dest = Path(dest)
    if not (dest / ".git").is_dir():
        raise GitError(f"sync_workspace requires an existing clone at {dest}")
    _run(
        [
            "git",
            "fetch",
            clone_url,
            f"+refs/heads/{base_branch}:refs/remotes/origin/{base_branch}",
        ],
        cwd=dest,
    )
    _configure_repo(dest, clone_url, author_name, author_email)
    # Discard uncommitted leftovers from a crashed run so the checkout below
    # cannot fail on a dirty tree.
    _run(["git", "reset", "--hard", "HEAD"], cwd=dest)
    _run(["git", "clean", "-fd"], cwd=dest)
    has_branch = (
        _run(
            ["git", "show-ref", "--verify", "--quiet", f"refs/heads/{work_branch}"],
            cwd=dest,
            check=False,
        ).returncode
        == 0
    )
    if has_branch:
        # Crash recovery: resume the task branch with its committed work intact.
        _run(["git", "checkout", work_branch], cwd=dest)
    else:
        _run(["git", "checkout", "-B", work_branch, f"refs/remotes/origin/{base_branch}"], cwd=dest)
    return dest


def prepare_workspace(
    clone_url: str,
    base_branch: str,
    work_branch: str,
    dest: Path,
    author_name: str = "skquad-agent",
    author_email: str = "agent@skquad.local",
) -> Path:
    """Prepare ``dest`` for a task: reuse-and-fetch when a clone already
    exists there (warm PVC, S-136), otherwise full clone.

    Dispatches to :func:`sync_workspace` when ``dest/.git`` exists, and to
    :func:`clone_workspace` otherwise (a stale non-clone directory at
    ``dest`` is removed first by the clone path).
    """
    dest = Path(dest)
    if (dest / ".git").is_dir():
        return sync_workspace(
            clone_url, base_branch, work_branch, dest, author_name=author_name, author_email=author_email
        )
    return clone_workspace(
        clone_url, base_branch, work_branch, dest, author_name=author_name, author_email=author_email
    )


def commit_and_push(
    repo_dir: Path,
    work_branch: str,
    push_url: str,
    message: str,
) -> WorkspaceResult:
    """Stage all changes, commit, and push ``HEAD`` to ``work_branch``.

    If there are no staged changes, nothing is committed or pushed and
    ``changed`` is False (the current HEAD sha is still returned).
    """
    repo_dir = Path(repo_dir)
    _run(["git", "add", "-A"], cwd=repo_dir)
    diff = _run(["git", "diff", "--cached", "--quiet"], cwd=repo_dir, check=False)
    changed = diff.returncode != 0
    head = _run(["git", "rev-parse", "HEAD"], cwd=repo_dir).stdout.strip()
    if not changed:
        return WorkspaceResult(branch=work_branch, commit_sha=head, pushed=False, changed=False)
    _run(["git", "commit", "-m", message], cwd=repo_dir)
    sha = _run(["git", "rev-parse", "HEAD"], cwd=repo_dir).stdout.strip()
    _run(["git", "push", push_url, f"HEAD:refs/heads/{work_branch}"], cwd=repo_dir)
    return WorkspaceResult(branch=work_branch, commit_sha=sha, pushed=True, changed=True)


def work_branch_for(agent_id: str, task_id: str) -> str:
    """Deterministic branch name: ``skquad/<agent>/<task>`` (sanitized)."""
    def sanitize(value: str) -> str:
        keep = []
        for ch in value:
            if ch.isalnum() or ch in ("-", "_", "."):
                keep.append(ch)
            else:
                keep.append("-")
        return "".join(keep).strip("-") or "x"

    return f"skquad/{sanitize(agent_id)}/{sanitize(task_id)}"
