"""Task git-workspace coordination.

Given a task's granted resources, find a git ``project_workspace``, prepare a
per-task working clone on a ``skquad/<agent>/<task>`` branch, and — on
success — commit, push, and report the refs back to the control plane.

The token is read from a Secret the operator mounts at
``<workspaces_dir>/<workspace_resource_id>/token``. If no git workspace is
granted, or the token is not mounted, the task proceeds without a workspace
(workspace is best-effort; it must never block normal task execution).
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping, Sequence

from . import git_workspace
from .git_workspace import GitError
from .journal import build_resume_note, git_resume_info

LOGGER = logging.getLogger(__name__)

DEFAULT_WORKSPACES_DIR = "/var/run/skquad/workspaces"
DEFAULT_WORKSPACE_BASE = "/tmp/skquad-workspaces"
# Where the operator mounts the per-agent workspace PVC (S-135/S-136).
DEFAULT_PVC_MOUNT_PATH = "/workspace"


@dataclass(frozen=True)
class WorkspaceHandle:
    resource_id: str
    path: Path
    branch: str
    push_url: str
    task_dir: Path | None = None
    # S-137: True when this claim is resuming a crashed run of the same
    # task (prior artifacts existed in the task dir).
    resumed: bool = False
    # Snapshot of the clone taken BEFORE sync discarded anything: branch,
    # local-commits-not-on-remote count, dirty-file summary.
    pre_sync_git: Mapping[str, Any] | None = None
    # Rendered resume note (also persisted in the task journal).
    resume_note: str = ""


def resolve_workspace_base(base_dest: str | Path | None = None) -> Path:
    """Resolve the writable workspace base, PVC-aware (S-136).

    Precedence:

    1. explicit ``base_dest`` (tests / overrides),
    2. ``SKQUAD_WORKSPACE_BASE`` env var,
    3. the per-agent PVC mount (``SKQUAD_WORKSPACE_MOUNT_PATH``, default
       ``/workspace``) when it exists and is writable,
    4. the ephemeral ``/tmp`` base — storage-disabled agents keep the
       pre-PVC behaviour exactly.
    """
    if base_dest:
        return Path(base_dest)
    env_base = os.environ.get("SKQUAD_WORKSPACE_BASE")
    if env_base:
        return Path(env_base)
    mount = Path(os.environ.get("SKQUAD_WORKSPACE_MOUNT_PATH", DEFAULT_PVC_MOUNT_PATH))
    if mount.is_dir() and os.access(mount, os.W_OK):
        return mount
    return Path(DEFAULT_WORKSPACE_BASE)


def git_clone_dir(base: str | Path, resource_id: str) -> Path:
    """Persistent clone location on the PVC: ``<base>/git/<resource_id>``."""
    return Path(base) / "git" / resource_id


def task_dir_for(base: str | Path, task_id: str) -> Path:
    """Crash-survivable per-task directory: ``<base>/tasks/<task_id>``."""
    return Path(base) / "tasks" / task_id


def scratch_dir(base: str | Path) -> Path:
    """General durable scratch: ``<base>/scratch``."""
    return Path(base) / "scratch"


def clone_is_healthy(dest: str | Path) -> bool:
    """Cheap worktree sanity check: ``git rev-parse --git-dir`` succeeds."""
    import subprocess

    repo = Path(dest)
    if not (repo / ".git").is_dir():
        return False
    try:
        proc = subprocess.run(
            ["git", "rev-parse", "--git-dir"], cwd=str(repo), capture_output=True, text=True
        )
    except OSError:
        return False
    return proc.returncode == 0


def move_aside_corrupt_clone(dest: str | Path) -> Path | None:
    """Rename a corrupt clone to ``<name>.corrupt.<utc-stamp>`` so the
    caller can full re-clone. Returns the new path, or ``None`` on failure
    (never raises — hygiene must not block the task)."""
    repo = Path(dest)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    aside = repo.with_name(f"{repo.name}.corrupt.{stamp}")
    try:
        repo.rename(aside)
    except OSError as exc:
        LOGGER.error(
            "failed to move aside corrupt clone",
            extra={"path": str(repo), "error": str(exc)},
        )
        return None
    LOGGER.warning(
        "corrupt git clone moved aside for re-clone",
        extra={"path": str(repo), "moved_to": str(aside)},
    )
    return aside


def ensure_task_dirs(
    base_dest: str | Path | None = None, task_id: str | None = None
) -> tuple[Path, Path | None]:
    """Create the durable scratch dir (and per-task dir when ``task_id`` is
    given) under the resolved base. Returns ``(base, task_dir)``."""
    base = resolve_workspace_base(base_dest)
    scratch_dir(base).mkdir(parents=True, exist_ok=True)
    task_dir = task_dir_for(base, task_id) if task_id else None
    if task_dir is not None:
        task_dir.mkdir(parents=True, exist_ok=True)
    return base, task_dir


def _field(res: object, name: str, default: object = "") -> object:
    if isinstance(res, Mapping):
        return res.get(name, default)
    return getattr(res, name, default)


def find_git_workspace(resources: Sequence[object] | None):
    """Return ``(resource_id, repo_url, default_branch)`` for the first granted
    active git workspace, or ``None``."""
    for res in resources or []:
        if str(_field(res, "resource_type", "")) != "project_workspace":
            continue
        manifest = _field(res, "manifest", {}) or {}
        if not isinstance(manifest, Mapping):
            continue
        if str(manifest.get("kind", "")).lower() != "git":
            continue
        endpoint = str(_field(res, "endpoint", "") or "")
        rid = str(_field(res, "resource_id", "") or "")
        default_branch = str(manifest.get("default_branch") or "main")
        if endpoint and rid:
            return rid, endpoint, default_branch
    return None


def read_workspace_token(resource_id: str, workspaces_dir: str | Path | None = None) -> str | None:
    d = Path(workspaces_dir or os.environ.get("SKQUAD_WORKSPACES_DIR", DEFAULT_WORKSPACES_DIR))
    token_file = d / resource_id / "token"
    if token_file.is_file():
        value = token_file.read_text(encoding="utf-8").strip()
        return value or None
    return None


def prepare_task_workspace(
    resources: Sequence[object] | None,
    agent_id: str,
    task_id: str,
    workspaces_dir: str | Path | None = None,
    base_dest: str | Path | None = None,
    resumed: bool = False,
) -> WorkspaceHandle | None:
    """Prepare a per-task git workspace, or return ``None`` if not applicable.

    S-137 crash-resume additions: when ``resumed`` is set and a clone
    already exists, a pre-sync git snapshot is captured (before
    ``sync_workspace`` discards uncommitted leftovers) so the resume note
    can surface it; a corrupt clone is moved aside so the dispatch below
    takes the full re-clone path. Pushed/committed work is never removed —
    only the corrupt clone directory itself is renamed, not deleted."""
    found = find_git_workspace(resources)
    if not found:
        return None
    rid, endpoint, default_branch = found
    token = read_workspace_token(rid, workspaces_dir)
    if not token:
        LOGGER.warning(
            "git workspace token not mounted; skipping workspace",
            extra={"workspace_resource_id": rid},
        )
        return None
    branch = git_workspace.work_branch_for(agent_id, task_id)
    base = resolve_workspace_base(base_dest)
    # PVC layout (S-136): persistent clone per granted workspace, durable
    # per-task dir, and a general scratch area. All survive pod restarts
    # when the base is the mounted PVC.
    dest = git_clone_dir(base, rid)
    task_dir = task_dir_for(base, task_id)
    scratch_dir(base).mkdir(parents=True, exist_ok=True)
    task_dir.mkdir(parents=True, exist_ok=True)
    # Per-agent commit authorship: the full agent id rides in the email so
    # any commit maps back to the control-plane agent; the short name keeps
    # git log readable. The shared workspace token cannot distinguish agents,
    # so git-history attribution is done here (see ADR-0009).
    author_name = f"skquad/{agent_id[:8]}"
    author_email = f"{agent_id}@skquad.local"
    # Worktree hygiene (S-137): a clone whose rev-parse fails is moved
    # aside (renamed, never deleted) so prepare_workspace re-clones cleanly.
    pre_sync_git: Mapping[str, Any] | None = None
    if dest.exists() and not clone_is_healthy(dest):
        move_aside_corrupt_clone(dest)
    elif resumed and clone_is_healthy(dest):
        # Snapshot BEFORE sync discards uncommitted leftovers, so the
        # resume note can surface them instead of losing them silently.
        pre_sync_git = git_resume_info(dest)
    try:
        push_url = git_workspace.authed_https_url(endpoint, token)
        git_workspace.prepare_workspace(
            push_url,
            default_branch,
            branch,
            dest,
            author_name=author_name,
            author_email=author_email,
        )
    except GitError as exc:
        LOGGER.error(
            "failed to prepare git workspace",
            extra={"workspace_resource_id": rid, "error": str(exc)},
        )
        return None
    LOGGER.info(
        "git workspace prepared",
        extra={
            "workspace_resource_id": rid,
            "branch": branch,
            "path": str(dest),
            "resumed": resumed,
        },
    )
    resume_note = (
        build_resume_note(task_dir, git_info=pre_sync_git) if resumed else ""
    )
    return WorkspaceHandle(
        resource_id=rid,
        path=dest,
        branch=branch,
        push_url=push_url,
        task_dir=task_dir,
        resumed=resumed,
        pre_sync_git=pre_sync_git,
        resume_note=resume_note,
    )


def finalize_task_workspace(handle: WorkspaceHandle, message: str):
    """Commit + push the workspace. Returns a WorkspaceResult or ``None`` on error."""
    try:
        result = git_workspace.commit_and_push(handle.path, handle.branch, handle.push_url, message)
        LOGGER.info(
            "git workspace finalized",
            extra={
                "workspace_resource_id": handle.resource_id,
                "branch": result.branch,
                "commit_sha": result.commit_sha,
                "changed": result.changed,
                "pushed": result.pushed,
            },
        )
        return result
    except GitError as exc:
        LOGGER.error(
            "failed to finalize git workspace",
            extra={"workspace_resource_id": handle.resource_id, "error": str(exc)},
        )
        return None
