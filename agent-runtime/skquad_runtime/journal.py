"""Crash-resume journal and resume detection (S-137).

When an agent re-claims a task it previously started (crash re-queue), the
durable per-task dir ``<base>/tasks/<task_id>/`` still holds whatever the
crashed run left behind. This module gives the runtime a tiny, best-effort
journal so the resumed run can tell the LLM what already happened instead
of starting from scratch.

Journal file: ``<task_dir>/.skquad-journal.json``::

    {
      "task_id": "task-1",
      "started_at": "2026-09-26T01:00:00+00:00",
      "resumed_at": ["2026-09-26T02:00:00+00:00"],
      "steps_done": ["clone", "patch"],
      "current_step": "test",
      "artifacts": ["report.txt"],
      "resume_note": "…rendered note for the LLM context…"
    }

Honesty note (documented per the card): **LLM inference cannot be replayed
mid-step.** A crash between LLM calls loses the in-flight reasoning. The
journal, the durable task-dir files, and the git branch state together give
a *practical* resume — the resumed agent sees what was done and picks up
from there — not an exact replay of the crashed process.

Every function here is best-effort: journal I/O failures are logged and
swallowed so they can never block or break a task run.
"""

from __future__ import annotations

import json
import logging
import os
import shutil
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

LOGGER = logging.getLogger(__name__)

JOURNAL_NAME = ".skquad-journal.json"
# Bounded budget for the rendered resume note injected into the LLM task
# context (follows the existing trim_text truncation pattern).
RESUME_NOTE_MAX_CHARS = 2000
# Listing caps so a pathological task dir cannot balloon the note.
ARTIFACT_LIST_MAX = 40
GIT_DIRTY_MAX = 15
GIT_COMMITS_MAX = 10


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def journal_path(task_dir: str | Path) -> Path:
    return Path(task_dir) / JOURNAL_NAME


def load_journal(task_dir: str | Path) -> dict[str, Any] | None:
    """Return the journal dict, or ``None`` when absent/unreadable/corrupt."""
    path = journal_path(task_dir)
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None
    except (OSError, json.JSONDecodeError) as exc:
        LOGGER.warning(
            "resume journal unreadable; treating as absent",
            extra={"path": str(path), "error": str(exc)},
        )
        return None
    if not isinstance(data, dict):
        return None
    return data


def save_journal(task_dir: str | Path, data: dict[str, Any]) -> bool:
    """Atomically write the journal; returns False on failure (never raises)."""
    path = journal_path(task_dir)
    try:
        tmp = path.with_name(f"{JOURNAL_NAME}.tmp")
        tmp.write_text(json.dumps(data, indent=2, sort_keys=True), encoding="utf-8")
        tmp.replace(path)
        return True
    except OSError as exc:
        LOGGER.warning(
            "resume journal write failed", extra={"path": str(path), "error": str(exc)}
        )
        return False


def _ensure_lists(data: dict[str, Any]) -> dict[str, Any]:
    for key in ("resumed_at", "steps_done", "artifacts"):
        if not isinstance(data.get(key), list):
            data[key] = []
    return data


def init_journal(task_dir: str | Path, task_id: str, resumed: bool) -> dict[str, Any]:
    """Write the initial journal entry for a claim.

    Fresh claim: sets ``started_at``. Resume: appends the current timestamp
    to ``resumed_at`` and preserves everything the crashed run recorded.
    """
    data = load_journal(task_dir)
    if data is None:
        data = {
            "task_id": task_id,
            "started_at": _now_iso(),
            "resumed_at": [],
            "steps_done": [],
            "current_step": "",
            "artifacts": [],
        }
    data = _ensure_lists(data)
    if resumed:
        data["resumed_at"].append(_now_iso())
    return data if save_journal(task_dir, data) else data


def journal_start_step(task_dir: str | Path, step: str) -> dict[str, Any]:
    """Mark ``step`` as in progress (handler-facing API)."""
    data = _ensure_lists(load_journal(task_dir) or {})
    data["current_step"] = step
    save_journal(task_dir, data)
    return data


def journal_complete_step(
    task_dir: str | Path, step: str, artifacts: list[str] | None = None
) -> dict[str, Any]:
    """Mark ``step`` done and record its artifacts (handler-facing API)."""
    data = _ensure_lists(load_journal(task_dir) or {})
    if step not in data["steps_done"]:
        data["steps_done"].append(step)
    if data.get("current_step") == step:
        data["current_step"] = ""
    for artifact in artifacts or []:
        if artifact not in data["artifacts"]:
            data["artifacts"].append(artifact)
    save_journal(task_dir, data)
    return data


def prior_artifacts(task_dir: str | Path) -> list[str]:
    """Names of prior files in the task dir, excluding the journal itself.

    A dir containing only the journal is NOT prior state — that is what a
    fresh claim just created.
    """
    directory = Path(task_dir)
    if not directory.is_dir():
        return []
    try:
        return sorted(
            entry.name
            for entry in directory.iterdir()
            if entry.name != JOURNAL_NAME and not entry.name.endswith(".tmp")
        )
    except OSError as exc:
        LOGGER.warning(
            "task dir listing failed",
            extra={"path": str(directory), "error": str(exc)},
        )
        return []


def task_dir_has_prior_state(task_dir: str | Path) -> bool:
    """True when the task dir already holds artifacts from a previous run."""
    return bool(prior_artifacts(task_dir))


def git_resume_info(repo_dir: str | Path) -> dict[str, Any] | None:
    """Cheap git snapshot for the resume note.

    Captures the current branch, commits that exist only locally (not on
    any remote — the pushed-work-preservation signal), and a dirty-file
    summary. Call this BEFORE ``sync_workspace`` runs so uncommitted
    leftovers are surfaced in the note rather than silently discarded.
    Returns ``None`` when the repo is not usable at all.
    """
    import subprocess

    repo = Path(repo_dir)

    def run(*args: str) -> str | None:
        try:
            proc = subprocess.run(
                ["git", *args], cwd=str(repo), capture_output=True, text=True
            )
        except OSError:
            return None
        if proc.returncode != 0:
            return None
        return proc.stdout

    branch = run("rev-parse", "--abbrev-ref", "HEAD")
    if branch is None:
        return None
    local_count = (run("rev-list", "--count", "HEAD", "--not", "--remotes") or "0").strip()
    oneline = run("log", "--oneline", "HEAD", "--not", "--remotes") or ""
    dirty = run("status", "--porcelain") or ""
    return {
        "branch": branch.strip(),
        "local_commits": int(local_count or 0),
        "local_commits_oneline": [
            line for line in oneline.splitlines() if line.strip()
        ][:GIT_COMMITS_MAX],
        "dirty_files": [line for line in dirty.splitlines() if line.strip()][
            :GIT_DIRTY_MAX
        ],
    }


def build_resume_note(
    task_dir: str | Path,
    journal: dict[str, Any] | None = None,
    git_info: dict[str, Any] | None = None,
    max_chars: int = RESUME_NOTE_MAX_CHARS,
) -> str:
    """Render the compact resume note injected into the LLM task context."""
    artifacts = prior_artifacts(task_dir)
    if journal is None:
        journal = load_journal(task_dir)
    steps_done = list((journal or {}).get("steps_done") or [])
    current_step = str((journal or {}).get("current_step") or "")
    if not artifacts and not steps_done and not git_info:
        return ""

    lines: list[str] = [
        "[skquad resume] This task was previously started and is being "
        "resumed after a crash/restart. Reuse the existing state below "
        "instead of starting over."
    ]
    if artifacts:
        shown = ", ".join(artifacts[:ARTIFACT_LIST_MAX])
        more = len(artifacts) - ARTIFACT_LIST_MAX
        suffix = f" (+{more} more)" if more > 0 else ""
        lines.append(f"Existing files in the task dir: {shown}{suffix}")
    if steps_done:
        lines.append(f"Journal — steps already completed: {', '.join(steps_done)}")
    if current_step:
        lines.append(
            f"Journal — interrupted during step: {current_step!r}. "
            "Treat it as unfinished."
        )
    if git_info:
        lines.append(f"Git workspace branch: {git_info.get('branch', '?')}")
        local = git_info.get("local_commits", 0)
        if local:
            lines.append(
                f"Local commits NOT yet on the remote: {local} "
                "(these are preserved — never assume they are lost)"
            )
            for commit in git_info.get("local_commits_oneline") or []:
                lines.append(f"  {commit}")
        dirty = git_info.get("dirty_files") or []
        if dirty:
            lines.append(
                f"Uncommitted changes present before sync ({len(dirty)} entries; "
                "the workspace sync may have discarded them — re-check before "
                "relying on them):"
            )
            for entry in dirty:
                lines.append(f"  {entry}")
    lines.append(
        "Note: LLM inference cannot be replayed mid-step; durable files + "
        "git state + this journal are the resume basis, not an exact replay."
    )
    note = "\n".join(lines)
    if max_chars > 0 and len(note) > max_chars:
        note = note[:max_chars].rstrip() + "\n[truncated]"
    return note


def set_resume_note(task_dir: str | Path, note: str) -> None:
    """Persist the rendered resume note in the journal (runtime-side)."""
    data = _ensure_lists(load_journal(task_dir) or {})
    data["resume_note"] = note
    save_journal(task_dir, data)


def read_resume_note(task_dir: str | Path) -> str:
    """Read the persisted resume note (handler-side); '' when absent."""
    data = load_journal(task_dir)
    if not data:
        return ""
    return str(data.get("resume_note") or "")


def resolve_task_dir(workspace_base: str = "", task_id: str = "") -> Path | None:
    """Resolve the per-task dir for the current run.

    Prefers ``SKQUAD_TASK_DIR`` (exported by the runtime at claim time);
    falls back to computing it from the workspace base so handlers can
    find the journal even when invoked outside the runtime flow.
    """
    env_dir = os.environ.get("SKQUAD_TASK_DIR")
    if env_dir:
        return Path(env_dir)
    if not task_id:
        return None
    # Imported lazily: workspace.py is the layout owner; journal.py must be
    # importable without it for handler-side use only.
    from .workspace import resolve_workspace_base, task_dir_for

    base = resolve_workspace_base(workspace_base or None)
    return task_dir_for(base, task_id)


def gc_task_dirs(
    base: str | Path,
    keep_task_id: str = "",
    ttl_days: float | None = None,
    now: datetime | None = None,
) -> list[str]:
    """Best-effort TTL garbage collection of old ``<base>/tasks/<id>/`` dirs.

    A task dir is expired when its newest mtime (the dir or its top-level
    entries) is older than ``ttl_days``. ``keep_task_id`` is never
    removed. ``ttl_days`` defaults to ``SKQUAD_TASK_DIR_TTL_DAYS`` (env),
    else 7. Never raises; removal failures are logged and skipped.
    Returns the names of removed dirs.
    """
    if ttl_days is None:
        raw = os.environ.get("SKQUAD_TASK_DIR_TTL_DAYS", "")
        try:
            ttl_days = float(raw) if raw.strip() else 7.0
        except ValueError:
            LOGGER.warning(
                "invalid SKQUAD_TASK_DIR_TTL_DAYS; using default",
                extra={"value": raw, "default": 7.0},
            )
            ttl_days = 7.0
    if ttl_days <= 0:
        return []
    cutoff = (now or datetime.now(timezone.utc)).timestamp() - ttl_days * 86400.0
    tasks_root = Path(base) / "tasks"
    removed: list[str] = []
    if not tasks_root.is_dir():
        return removed
    try:
        children = sorted(tasks_root.iterdir())
    except OSError as exc:
        LOGGER.warning("task GC listing failed", extra={"path": str(tasks_root), "error": str(exc)})
        return removed
    for child in children:
        if not child.is_dir() or child.name == keep_task_id:
            continue
        try:
            newest = child.stat().st_mtime
            for entry in child.iterdir():
                newest = max(newest, entry.stat().st_mtime)
        except OSError:
            continue
        if newest >= cutoff:
            continue
        try:
            shutil.rmtree(child)
            removed.append(child.name)
        except OSError as exc:
            LOGGER.warning(
                "task dir GC removal failed",
                extra={"path": str(child), "error": str(exc)},
            )
    if removed:
        LOGGER.info(
            "task dir GC removed expired task dirs",
            extra={"removed": removed, "ttl_days": ttl_days},
        )
    return removed
