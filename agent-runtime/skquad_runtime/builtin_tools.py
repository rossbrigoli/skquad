"""Built-in platform tools per ADR-0012: exec, web_fetch, web_search.

Implements the RuntimePlugin protocol (``name``, ``tools()``,
``invoke(call, config)``) so the chat (S-122) and task tool loops work
unchanged. Stdlib only. Tool results are returned raw here; the runtime
wraps them as untrusted data (S-PROMPT WP3 ``wrap_untrusted``) at the
handler layer.
"""

from __future__ import annotations

import html.parser
import base64
import json
import logging
import os
import re
import signal
import subprocess
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Mapping
from urllib import error, request
from urllib.parse import quote

from .runtime import A2A_CORRELATION, ToolCall, ToolResult

logger = logging.getLogger(__name__)

# Seed denylist applied to exec when the platform policy omits
# ``deniedPatterns`` (ADR-0012 §3). Regex strings matched against the
# FULL command string before execution.
DEFAULT_DENY_PATTERNS: list[str] = [
    r"rm\s+-[rf]{1,2}\s+/",          # rm -rf / (and variants)
    r"\bmkfs(\.\w+)?\b",             # mkfs / mkfs.ext4 ...
    r"\bdd\b.*\bof=/dev/",           # dd writing to raw devices
    r":\(\)\s*\{\s*:\|:&\s*\}\s*;:",  # classic fork bomb
    r"\b(shutdown|reboot|halt)\b",   # power operations
    r"\b(curl|wget)\b.*\|\s*(ba|z|k)?sh\b",  # curl|wget piped to a shell
]

# Shared MIME type for control-plane proxy calls (S1192).
JSON_CONTENT_TYPE = "application/json"

# Shared literals (S1192).
EMPTY_DISPLAY = "(none)"
PEERS_PATH = "/api/v1/agents/me/peers"
TASKS_PATH = "/api/v1/agents/me/tasks"
PATH_DESC_WORKSPACE_RELATIVE = "File path relative to the workspace."
DIFF_ADD_PREFIX = "*** Add File: "
DIFF_UPDATE_PREFIX = "*** Update File: "
DIFF_DELETE_PREFIX = "*** Delete File: "
TOOL_CREATE_TASK = "board.create_task"
TOOL_ASSIGN = "board.assign"
TOOL_CLAIM = "board.claim"
TOOL_COMPLETE = "board.complete"
TOOL_BLOCK = "board.block"
TOOL_REOPEN = "board.reopen"
TOOL_REQUEST_CHANGES = "board.request_changes"
TOOL_APPROVE = "board.approve"


@dataclass
class BuiltinToolContext:
    """Static context a built-in tool needs to run inside an agent pod."""

    workspace_dir: str
    control_plane_url: str
    agent_credential: str
    agent_id: str = ""
    # S-283: tool-gateway base URL (SKQUAD_TOOL_GATEWAY_URL, operator-
    # injected). Empty is legal: the browser builtin then fails closed.
    tool_gateway_url: str = ""
    # S-283: resource id of the granted managed browser resource (an
    # mcp-typed grant whose endpoint_config declares driver: "browser").
    # Empty when the agent holds no such grant — the builtin stays inert.
    browser_resource_id: str = ""


def _scrubbed_env() -> dict[str, str]:
    """Copy of os.environ minus secret-shaped keys (ADR-0012 §3 exec policy)."""
    return {
        key: value
        for key, value in os.environ.items()
        if not key.startswith("SKQUAD_")
        and not key.endswith(("_API_KEY", "_TOKEN", "_SECRET"))
    }


class _TextExtractor(html.parser.HTMLParser):
    """Stdlib html->text: strips tags, script and style content."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self._chunks: list[str] = []
        self._skip_depth = 0

    def handle_starttag(self, tag: str, attrs) -> None:  # noqa: ANN001
        if tag in ("script", "style"):
            self._skip_depth += 1
        elif tag in ("br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6"):
            self._chunks.append("\n")

    def handle_endtag(self, tag: str) -> None:
        if tag in ("script", "style") and self._skip_depth > 0:
            self._skip_depth -= 1

    def handle_data(self, data: str) -> None:
        if self._skip_depth == 0:
            self._chunks.append(data)

    def text(self) -> str:
        raw = "".join(self._chunks)
        lines = [line.strip() for line in raw.splitlines()]
        return "\n".join(line for line in lines if line)


class ExecTool:
    """Run terminal commands in the agent workspace with a denylist + env scrub."""

    name = "exec"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "exec",
                    "description": (
                        "Execute a shell command in the agent's task workspace. "
                        "Returns combined stdout/stderr and the exit code."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "command": {
                                "type": "string",
                                "description": "Shell command to run.",
                            },
                            "background": {
                                "type": "boolean",
                                "description": (
                                    "S-280: start the command detached in "
                                    "the background with stdout/stderr "
                                    "captured to a log. Returns a session "
                                    "id you control with the process tool "
                                    "(poll/log/write/kill). If the command "
                                    "finishes before yieldMs elapses, its "
                                    "result is returned normally instead."
                                ),
                            },
                            "yieldMs": {
                                "type": "integer",
                                "description": (
                                    "S-280 background only: how long to "
                                    "wait (milliseconds, default 10000) "
                                    "for a quick finish before returning "
                                    "the session id."
                                ),
                            },
                        },
                        "required": ["command"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        command = str(call.arguments.get("command", ""))
        if not command.strip():
            return ToolResult(content="exec: empty command", ok=False)

        patterns = self.policy.get("deniedPatterns") or DEFAULT_DENY_PATTERNS
        for pattern in patterns:
            try:
                denied = re.search(pattern, command)
            except re.error:
                logger.warning("exec: invalid deniedPattern %r skipped", pattern)
                continue
            if denied:
                return ToolResult(
                    content=f"exec: command denied by policy (matched pattern: {pattern!r})",
                    ok=False,
                )

        timeout = int(self.policy.get("timeoutSeconds", 60))
        max_bytes = int(self.policy.get("maxOutputBytes", 65536))
        # The working directory may not exist yet: chat runs (and agents
        # with no git workspace) never go through the task-workspace setup
        # that creates per-task dirs, so a missing base here killed the
        # shell with ENOENT before any command could run. Create it lazily.
        try:
            Path(self.context.workspace_dir).mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            return ToolResult(
                content=(
                    f"exec failed to start: cannot create workspace dir "
                    f"{self.context.workspace_dir!r}: {exc}"
                ),
                ok=False,
            )

        if bool(call.arguments.get("background", False)):
            return self._invoke_background(call, command, max_bytes)

        try:
            proc = subprocess.run(
                ["/bin/sh", "-c", command],
                cwd=self.context.workspace_dir,
                timeout=timeout,
                capture_output=True,
                env=_scrubbed_env(),
                check=False,
            )
        except subprocess.TimeoutExpired:
            return ToolResult(content=f"exec timed out after {timeout}s", ok=False)
        except OSError as exc:
            return ToolResult(content=f"exec failed to start: {exc}", ok=False)

        stdout = proc.stdout.decode("utf-8", errors="replace")
        stderr = proc.stderr.decode("utf-8", errors="replace")
        combined = stdout + (("\n" + stderr) if stderr else "")
        if len(combined.encode("utf-8")) > max_bytes:
            combined = combined.encode("utf-8")[:max_bytes].decode("utf-8", errors="replace")
            combined += "\n[output truncated]"
        return ToolResult(
            content=f"exit code: {proc.returncode}\n{combined}",
            ok=proc.returncode == 0,
        )

    def _invoke_background(self, call: ToolCall, command: str, max_bytes: int) -> ToolResult:
        """S-280: detached start with yieldMs early-return on fast exit."""
        try:
            yield_ms = int(self.policy.get("yieldMs", DEFAULT_YIELD_MS))
        except (TypeError, ValueError):
            yield_ms = DEFAULT_YIELD_MS
        raw_yield = call.arguments.get("yieldMs")
        if raw_yield is not None:
            try:
                yield_ms = int(raw_yield)
            except (TypeError, ValueError):
                return ToolResult(content="exec: yieldMs must be an integer", ok=False)
        if yield_ms < 0:
            yield_ms = 0
        proc_tool = ProcessTool(self.policy, self.context)
        session, err = _start_background_session(
            self.context.workspace_dir,
            command,
            proc_tool.max_sessions,
        )
        if err:
            return ToolResult(content=f"exec background: {err}", ok=False)
        sid = session["id"]
        proc = _PROCESS_HANDLES.get(sid)
        # Give a quick command its normal foreground-style result.
        rc = None
        if proc is not None:
            try:
                rc = proc.wait(timeout=max(yield_ms / 1000.0, 0.05))
            except subprocess.TimeoutExpired:
                rc = None
        if rc is None:
            _refresh_session(session)
        else:
            session["status"] = "exited"
            session["exit_code"] = rc
            _PROCESS_HANDLES.pop(sid, None)
            sessions = _load_sessions(self.context.workspace_dir)
            sessions[sid] = session
            _save_sessions(self.context.workspace_dir, sessions)
        if rc is not None:
            text, truncated = _read_capped(
                Path(self.context.workspace_dir) / session["log_path"], max_bytes
            )
            notice = "\n[output truncated]" if truncated else ""
            return ToolResult(content=f"exit code: {rc}\n{text}{notice}", ok=rc == 0)
        return ToolResult(
            content=(
                f"started background session {sid} (pid {session['pid']}). "
                f"Use process(action=poll/log/write/kill, sessionId={sid}) "
                f"to control it. Log: {session['log_path']}"
            ),
            ok=True,
        )


# ---------------------------------------------------------------------------
# S-280: background process management (exec background + process tool).
#
# A background session is a command started detached (start_new_session=True,
# i.e. its own process group) inside the agent's task workspace, with
# stdout/stderr captured to a log file under <workspace>/.skquad-process/.
# The session registry is a JSON file in that same directory — the same
# durable-file pattern as the crash-resume journal (.skquad-journal.json) —
# so sessions survive across tool calls and wakes. Kill uses os.killpg so
# the whole process group dies, children included. All paths stay inside
# the workspace jail; the log cap (maxLogBytes) kills the process when the
# captured output exceeds it so a runaway loop cannot fill the PVC.
# ---------------------------------------------------------------------------

PROCESS_STATE_DIRNAME = ".skquad-process"
PROCESS_REGISTRY_NAME = "sessions.json"
DEFAULT_MAX_SESSIONS = 16
DEFAULT_MAX_LOG_BYTES = 5 * 1024 * 1024  # 5 MiB per session
DEFAULT_YIELD_MS = 10_000
MAX_POLL_WAIT_SECONDS = 30

# In-process handle to still-running Popen objects (stdin writes and fast
# status polling). The registry JSON is the durable source of truth; this
# map is best-effort liveness within the current wake.
_PROCESS_HANDLES: dict[str, subprocess.Popen] = {}
_PROCESS_LOCK = threading.Lock()


def _process_state_dir(workspace_dir: str) -> Path:
    d = Path(workspace_dir) / PROCESS_STATE_DIRNAME
    d.mkdir(parents=True, exist_ok=True)
    return d


def _registry_path(workspace_dir: str) -> Path:
    return _process_state_dir(workspace_dir) / PROCESS_REGISTRY_NAME


def _load_sessions(workspace_dir: str) -> dict[str, dict]:
    path = _registry_path(workspace_dir)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        return {}
    except (OSError, json.JSONDecodeError):
        logger.warning("process: registry unreadable at %s; treating as empty", path)
        return {}
    if not isinstance(data, dict):
        return {}
    return {k: v for k, v in data.items() if isinstance(v, dict)}


def _save_sessions(workspace_dir: str, sessions: dict[str, dict]) -> None:
    path = _registry_path(workspace_dir)
    tmp = path.with_suffix(".json.tmp")
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(sessions, fh)
    os.replace(tmp, path)


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _refresh_session(session: dict) -> None:
    """Update a running session's status from the live process (no wait).

    With a held Popen handle we poll cheaply and reap properly. Without one
    (a later wake in the same pod is still the parent, so waitpid works;
    ECHILD means the process was reaped elsewhere or was never ours) we
    fall back to os.waitpid(WNOHANG) and finally to liveness probing.
    """
    if session.get("status") != "running":
        return
    sid = str(session.get("id", ""))
    proc = _PROCESS_HANDLES.get(sid)
    if proc is not None:
        rc = proc.poll()
        if rc is not None:
            session["status"] = "killed" if session.get("kill_requested") else "exited"
            session["exit_code"] = rc
            _PROCESS_HANDLES.pop(sid, None)
        return
    pid = int(session.get("pid", -1))
    if pid <= 0:
        session["status"] = "exited"
        session["exit_code"] = None
        return
    try:
        waited, status = os.waitpid(pid, os.WNOHANG)
        if waited == pid:
            session["status"] = "killed" if session.get("kill_requested") else "exited"
            session["exit_code"] = os.waitstatus_to_exitcode(status)
            return
    except ChildProcessError:
        # Not our child anymore (or never was): fall back to liveness only.
        pass
    except OSError:
        pass
    if not _pid_alive(pid):
        session["status"] = "exited"
        session["exit_code"] = session.get("exit_code")


def _running_count(sessions: dict[str, dict]) -> int:
    return sum(1 for s in sessions.values() if s.get("status") == "running")


def _new_session_id() -> str:
    return "p" + os.urandom(4).hex()


def _start_background_session(
    workspace_dir: str, command: str, max_sessions: int
) -> tuple[dict | None, str]:
    """Start ``command`` detached; returns (session, error_message)."""
    with _PROCESS_LOCK:
        sessions = _load_sessions(workspace_dir)
        for s in sessions.values():
            _refresh_session(s)
        if _running_count(sessions) >= max_sessions:
            return None, (
                f"cannot start: {max_sessions} background sessions already running "
                "(cap reached; kill or clear some first)"
            )
        sid = _new_session_id()
        log_path = _process_state_dir(workspace_dir) / f"{sid}.log"
        try:
            log_fh = open(log_path, "ab")
        except OSError as exc:
            return None, f"cannot open session log: {exc}"
        try:
            proc = subprocess.Popen(
                ["/bin/sh", "-c", command],
                cwd=workspace_dir,
                stdin=subprocess.PIPE,
                stdout=log_fh,
                stderr=subprocess.STDOUT,
                env=_scrubbed_env(),
                start_new_session=True,
            )
        except OSError as exc:
            log_fh.close()
            return None, f"cannot start background command: {exc}"
        log_fh.close()
        session = {
            "id": sid,
            "command": command,
            "pid": proc.pid,
            "pgid": os.getpgid(proc.pid),
            "started_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            "status": "running",
            "exit_code": None,
            "log_path": str(log_path.relative_to(Path(workspace_dir))),
        }
        _PROCESS_HANDLES[sid] = proc
        sessions[sid] = session
        _save_sessions(workspace_dir, sessions)
        return session, ""


def _kill_session(workspace_dir: str, session: dict) -> str:
    """SIGTERM the process group, then SIGKILL after a short grace."""
    pgid = int(session.get("pgid", session.get("pid", -1)))
    if pgid <= 0:
        return "no process group to kill"
    try:
        os.killpg(pgid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    except PermissionError as exc:
        return f"kill failed: {exc}"
    deadline = time.monotonic() + 3.0
    while time.monotonic() < deadline:
        sid = str(session.get("id", ""))
        proc = _PROCESS_HANDLES.get(sid)
        if proc is not None and proc.poll() is not None:
            break
        if not _pgid_alive(pgid):
            break
        time.sleep(0.1)
    else:
        try:
            os.killpg(pgid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    session["kill_requested"] = True
    _refresh_session(session)
    if session.get("status") == "running":
        session["status"] = "killed"
    sessions = _load_sessions(workspace_dir)
    if session.get("id") in sessions:
        sessions[session["id"]] = session
        _save_sessions(workspace_dir, sessions)
    return ""


def _pgid_alive(pgid: int) -> bool:
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _enforce_log_cap(workspace_dir: str, session: dict, max_log_bytes: int) -> bool:
    """Kill a running session whose captured log exceeded the cap."""
    if session.get("status") != "running":
        return False
    try:
        size = os.path.getsize(Path(workspace_dir) / str(session.get("log_path", "")))
    except OSError:
        return False
    if size <= max_log_bytes:
        return False
    _kill_session(workspace_dir, session)
    session["note"] = f"killed: log exceeded maxLogBytes={max_log_bytes}"
    sessions = _load_sessions(workspace_dir)
    if session.get("id") in sessions:
        sessions[session["id"]] = session
        _save_sessions(workspace_dir, sessions)
    return True


def _read_capped(path: Path, max_bytes: int) -> tuple[str, bool]:
    try:
        with open(path, "rb") as fh:
            data = fh.read(max_bytes + 1)
    except OSError as exc:
        return f"[cannot read log: {exc}]", False
    truncated = len(data) > max_bytes
    return data[:max_bytes].decode("utf-8", errors="replace"), truncated


class ProcessTool:
    """S-280: manage background exec sessions started by this agent task.

    Actions: list, poll, log, write (stdin), kill (process group), clear.
    Sessions live in <workspace>/.skquad-process/sessions.json with one
    captured log per session; kill uses os.killpg so children die too.
    """

    name = "process"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    @property
    def max_sessions(self) -> int:
        try:
            return int(self.policy.get("maxSessions", DEFAULT_MAX_SESSIONS))
        except (TypeError, ValueError):
            return DEFAULT_MAX_SESSIONS

    @property
    def max_log_bytes(self) -> int:
        try:
            return int(self.policy.get("maxLogBytes", DEFAULT_MAX_LOG_BYTES))
        except (TypeError, ValueError):
            return DEFAULT_MAX_LOG_BYTES

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "process",
                    "description": (
                        "Manage background commands started with "
                        "exec(background=true). Actions: list (all "
                        "sessions for this task), poll (status + exit "
                        "code, optionally wait up to 30s for completion), "
                        "log (captured stdout/stderr, with line "
                        "offset/limit), write (send data to the "
                        "session's stdin), kill (terminate the whole "
                        "process group), clear (remove a finished "
                        "session record)."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "action": {
                                "type": "string",
                                "enum": ["list", "poll", "log", "write", "kill", "clear"],
                                "description": "Management action to perform.",
                            },
                            "sessionId": {
                                "type": "string",
                                "description": (
                                    "Session id returned by "
                                    "exec(background=true). Required for "
                                    "poll/log/write/kill/clear."
                                ),
                            },
                            "timeoutSeconds": {
                                "type": "integer",
                                "description": (
                                    "poll only: wait up to this many "
                                    "seconds (max 30) for the session "
                                    "to finish."
                                ),
                            },
                            "data": {
                                "type": "string",
                                "description": "write only: text sent to stdin.",
                            },
                            "offset": {
                                "type": "integer",
                                "description": "log only: first line (1-indexed).",
                            },
                            "limit": {
                                "type": "integer",
                                "description": "log only: max lines to return.",
                            },
                        },
                        "required": ["action"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        action = str(call.arguments.get("action", "")).strip().lower()
        if action == "list":
            return self._list()
        sid = str(call.arguments.get("sessionId", "")).strip()
        if not sid:
            return ToolResult(content=f"process {action}: sessionId is required", ok=False)
        sessions = _load_sessions(self.context.workspace_dir)
        session = sessions.get(sid)
        if session is None:
            return ToolResult(content=f"process {action}: unknown session {sid!r}", ok=False)
        _refresh_session(session)
        if action == "poll":
            return self._poll(session, call)
        if action == "log":
            return self._log(session, call)
        if action == "write":
            return self._write(session, call)
        if action == "kill":
            return self._kill(session)
        if action == "clear":
            return self._clear(sessions, session)
        return ToolResult(
            content="process: action must be one of list/poll/log/write/kill/clear", ok=False
        )

    def _list(self) -> ToolResult:
        sessions = _load_sessions(self.context.workspace_dir)
        for s in sessions.values():
            _refresh_session(s)
            _enforce_log_cap(self.context.workspace_dir, s, self.max_log_bytes)
        if sessions:
            _save_sessions(self.context.workspace_dir, sessions)
        if not sessions:
            return ToolResult(content="process: no background sessions", ok=True)
        lines = []
        for s in sessions.values():
            rc = s.get("exit_code")
            rc_text = f" exit={rc}" if rc is not None else ""
            note = f" ({s.get('note')})" if s.get("note") else ""
            lines.append(
                f"{s['id']} [{s['status']}]{rc_text}{note} pid={s['pid']} "
                f"started={s['started_at']} :: {s['command'][:120]}"
            )
        return ToolResult(content="\n".join(lines), ok=True)

    def _poll(self, session: dict, call: ToolCall) -> ToolResult:
        try:
            wait = min(int(call.arguments.get("timeoutSeconds", 0) or 0), MAX_POLL_WAIT_SECONDS)
        except (TypeError, ValueError):
            wait = 0
        if wait < 0:
            wait = 0
        if wait > 0 and session.get("status") == "running":
            deadline = time.monotonic() + wait
            while time.monotonic() < deadline and session.get("status") == "running":
                _refresh_session(session)
                if session.get("status") != "running":
                    break
                if _enforce_log_cap(self.context.workspace_dir, session, self.max_log_bytes):
                    break
                time.sleep(0.2)
        _enforce_log_cap(self.context.workspace_dir, session, self.max_log_bytes)
        sessions = _load_sessions(self.context.workspace_dir)
        sessions[session["id"]] = session
        _save_sessions(self.context.workspace_dir, sessions)
        rc = session.get("exit_code")
        rc_text = f" exit={rc}" if rc is not None else ""
        note = f" ({session.get('note')})" if session.get("note") else ""
        return ToolResult(
            content=f"{session['id']} [{session['status']}]{rc_text}{note}", ok=True
        )

    def _log(self, session: dict, call: ToolCall) -> ToolResult:
        log_file = Path(self.context.workspace_dir) / str(session.get("log_path", ""))
        # Defence in depth: the log must stay inside the workspace jail.
        try:
            log_file = _resolve_in_workspace(self.context.workspace_dir, str(session.get("log_path", "")))
        except WorkspaceEscapeError as exc:
            return ToolResult(content=f"process log: {exc}", ok=False)
        if not log_file.is_file():
            return ToolResult(content=f"process log: no output captured for {session['id']}", ok=True)
        text, truncated = _read_capped(log_file, self.max_log_bytes)
        lines = text.split("\n")
        offset = call.arguments.get("offset")
        limit = call.arguments.get("limit")
        start = 0
        if offset is not None:
            try:
                start = max(int(offset) - 1, 0)
            except (TypeError, ValueError):
                return ToolResult(content="process log: offset must be an integer", ok=False)
        end = len(lines)
        if limit is not None:
            try:
                end = min(start + max(int(limit), 0), len(lines))
            except (TypeError, ValueError):
                return ToolResult(content="process log: limit must be an integer", ok=False)
        content = "\n".join(lines[start:end])
        notices = []
        if start > 0 or end < len(lines):
            notices.append(f"showing lines {start + 1}-{end} of {len(lines)}")
        if truncated:
            notices.append(f"log truncated at maxLogBytes={self.max_log_bytes}")
        if notices:
            content += "\n[" + "; ".join(notices) + "]"
        return ToolResult(content=content, ok=True)

    def _write(self, session: dict, call: ToolCall) -> ToolResult:
        if session.get("status") != "running":
            return ToolResult(
                content=f"process write: session {session['id']} is not running", ok=False
            )
        data = call.arguments.get("data")
        if data is None:
            return ToolResult(content="process write: data is required", ok=False)
        proc = _PROCESS_HANDLES.get(str(session.get("id", "")))
        if proc is None or proc.stdin is None:
            return ToolResult(
                content=(
                    f"process write: stdin not attached for {session['id']} "
                    "(session started in a previous wake)"
                ),
                ok=False,
            )
        try:
            proc.stdin.write(str(data).encode("utf-8"))
            proc.stdin.flush()
        except OSError as exc:
            return ToolResult(content=f"process write: failed: {exc}", ok=False)
        return ToolResult(content=f"wrote {len(str(data))} chars to {session['id']} stdin", ok=True)

    def _kill(self, session: dict) -> ToolResult:
        if session.get("status") != "running":
            return ToolResult(
                content=f"process kill: session {session['id']} already {session.get('status')}",
                ok=True,
            )
        err = _kill_session(self.context.workspace_dir, session)
        if err:
            return ToolResult(content=f"process kill: {err}", ok=False)
        return ToolResult(
            content=f"killed session {session['id']} (process group {session.get('pgid')})", ok=True
        )

    def _clear(self, sessions: dict, session: dict) -> ToolResult:
        if session.get("status") == "running":
            return ToolResult(
                content=f"process clear: session {session['id']} is still running; kill it first",
                ok=False,
            )
        sessions.pop(session["id"], None)
        _save_sessions(self.context.workspace_dir, sessions)
        try:
            log_file = Path(self.context.workspace_dir) / str(session.get("log_path", ""))
            if log_file.is_file():
                log_file.unlink()
        except OSError:
            pass
        return ToolResult(content=f"cleared session {session['id']}", ok=True)


class WebFetchTool:
    """Fetch a URL via the control-plane proxy (BT-6).

    Agent pods run under a default-deny egress NetworkPolicy (only DNS +
    skquad-system are reachable), so the runtime cannot fetch arbitrary
    URLs itself. The guarded fetch (SSRF dial guard, redirect cap, size
    cap, timeout) executes in the control plane — the same proxy pattern
    as web_search. This side keeps only scheme validation and the
    html->text extraction.
    """

    name = "web_fetch"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "web_fetch",
                    "description": (
                        "Fetch a URL and return readable text extracted from the page."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "url": {
                                "type": "string",
                                "description": "http(s) URL to fetch.",
                            }
                        },
                        "required": ["url"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        url = str(call.arguments.get("url", ""))
        if not url:
            return ToolResult(content="web_fetch: missing url", ok=False)

        scheme = url.split(":", 1)[0].lower() if ":" in url else ""
        if scheme not in ("http", "https"):
            return ToolResult(
                content=f"web_fetch: unsupported scheme {scheme!r} (http/https only)",
                ok=False,
            )

        timeout = int(self.policy.get("timeoutSeconds", 30))
        proxy_url = self.context.control_plane_url.rstrip("/") + "/api/v1/tools/web_fetch"
        payload = json.dumps({"url": url}).encode("utf-8")
        # Same auth header pattern as the web_search proxy call.
        req = request.Request(
            proxy_url,
            data=payload,
            method="POST",
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        try:
            with request.urlopen(req, timeout=timeout) as response:
                data = json.loads(response.read().decode("utf-8"))
        except error.HTTPError as exc:
            return ToolResult(content=f"web_fetch: HTTP {exc.code}", ok=False)
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"web_fetch: proxy request failed: {exc}", ok=False)

        status = int(data.get("status", 0))
        final_url = str(data.get("url", url))
        if status >= 400:
            return ToolResult(content=f"web_fetch: HTTP {status} for {final_url}", ok=False)
        try:
            body = base64.b64decode(data.get("bodyB64", "").encode("ascii"))
        except Exception as exc:  # noqa: BLE001 — malformed proxy payload
            return ToolResult(content=f"web_fetch: malformed proxy response: {exc}", ok=False)

        content_type = str(data.get("contentType", ""))
        text = body.decode("utf-8", errors="replace")
        if "html" in content_type.lower():
            extractor = _TextExtractor()
            try:
                extractor.feed(text)
                extractor.close()
                text = extractor.text()
            except Exception:  # noqa: BLE001 — malformed html: keep raw text
                pass
        if data.get("truncated"):
            text += "\n[content truncated]"
        return ToolResult(content=text, ok=True)


class WebSearchTool:
    """Web search via the control-plane proxy (provider keys never reach runtime)."""

    name = "web_search"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "web_search",
                    "description": "Search the web and return titled results with snippets.",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "query": {
                                "type": "string",
                                "description": "Search query.",
                            },
                            "maxResults": {
                                "type": "integer",
                                "description": "Maximum number of results to return.",
                            },
                        },
                        "required": ["query"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        query = str(call.arguments.get("query", ""))
        if not query.strip():
            return ToolResult(content="web_search: empty query", ok=False)

        body: dict[str, object] = {"query": query}
        max_results = call.arguments.get("maxResults")
        if max_results is None:
            max_results = self.policy.get("maxResults")
        if max_results is not None:
            body["maxResults"] = int(max_results)

        timeout = int(self.policy.get("timeoutSeconds", 20))
        url = self.context.control_plane_url.rstrip("/") + "/api/v1/tools/web_search"
        payload = json.dumps(body).encode("utf-8")
        # Same auth header pattern as runtime.py control-plane calls.
        req = request.Request(
            url,
            data=payload,
            method="POST",
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        try:
            with request.urlopen(req, timeout=timeout) as response:
                if not 200 <= response.status < 300:
                    return ToolResult(
                        content=f"web_search: HTTP {response.status}", ok=False
                    )
                data = json.loads(response.read().decode("utf-8"))
        except error.HTTPError as exc:
            return ToolResult(content=f"web_search: HTTP {exc.code}", ok=False)
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"web_search: request failed: {exc}", ok=False)

        results = data.get("results") or []
        if not results:
            return ToolResult(content="web_search: no results", ok=True)

        lines: list[str] = []
        for item in results:
            title = str(item.get("title", ""))
            item_url = str(item.get("url", ""))
            snippet = str(item.get("snippet", ""))
            lines.append(f"{title} — {item_url}\n{snippet}")
        return ToolResult(content="\n\n".join(lines), ok=True)


class SendMessageTool:
    """S-164: agent-to-agent messaging inside the squad.

    The model calls ``send_message`` like any other tool; the tool resolves
    the target by name against the squad roster
    (``GET /api/v1/agents/me/peers``) and posts through the existing
    control-plane queue (``POST /api/v1/agents/me/messages``), so same-
    squad permission, delegate materialization, chain budget, and audit
    all stay control-plane-side. The runtime never talks to a peer pod
    directly.

    Correlation: when the tool is invoked while the agent is processing an
    inbox message, an omitted ``correlation_id`` inherits the correlation
    of the message being processed (``A2A_CORRELATION`` contextvar) so a
    follow-up stays on the same thread and the chain budget applies.
    """

    name = "send_message"

    ALLOWED_TYPES = ("consult", "delegate", "ping", "reply")

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "send_message",
                    "description": (
                        "Send a message to another agent in your squad. "
                        "Messages are queued: a busy recipient is never "
                        "interrupted and answers when it becomes free. "
                        "consult asks a question (answered asynchronously), "
                        "delegate hands over work (creates a task for the "
                        "recipient), ping is a notification."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "target_agent": {
                                "type": "string",
                                "description": (
                                    "Name of the squad-mate to message "
                                    '(e.g. "Mary").'
                                ),
                            },
                            "message": {
                                "type": "string",
                                "description": "Message text for the recipient.",
                            },
                            "type": {
                                "type": "string",
                                "enum": ["consult", "delegate", "ping"],
                                "description": "Message type. Defaults to consult.",
                            },
                            "correlation_id": {
                                "type": "string",
                                "description": (
                                    "Thread id (UUID) continuing an existing "
                                    "conversation. Usually omit it and the "
                                    "current thread is inherited."
                                ),
                            },
                            "consult_timeout_seconds": {
                                "type": "integer",
                                "description": (
                                    "S-173: override how long to wait for a "
                                    "reply to this consult (seconds). If no "
                                    "reply lands by the deadline you receive "
                                    "a synthetic consult_timeout notice on "
                                    "the thread. Consult sends only; the "
                                    "platform default is 15 minutes."
                                ),
                            },
                        },
                        "required": ["target_agent", "message"],
                    },
                },
            }
        ]

    def _validate(self, target: str, text: str, mtype: str) -> ToolResult | None:
        """Validate send_message arguments; None when all valid."""
        if not target:
            return ToolResult(content="send_message: target_agent is required", ok=False)
        if not text:
            return ToolResult(content="send_message: message is required", ok=False)
        if mtype not in self.ALLOWED_TYPES:
            return ToolResult(
                content=(
                    "send_message: type must be one of "
                    + ", ".join(self.ALLOWED_TYPES)
                ),
                ok=False,
            )
        max_chars = int(self.policy.get("maxMessageChars", 10000))
        if len(text) > max_chars:
            return ToolResult(
                content=f"send_message: message exceeds {max_chars} characters", ok=False
            )
        return None

    def _apply_consult_deadline(self, body: dict[str, object], call: ToolCall) -> None:
        """S-173: per-send consult deadline override (consult sends only)."""
        if str(body.get("type")) != "consult":
            return
        try:
            timeout_seconds = int(call.arguments.get("consult_timeout_seconds", 0) or 0)
        except (TypeError, ValueError):
            timeout_seconds = 0
        if timeout_seconds > 0:
            body["consult_timeout_seconds"] = timeout_seconds

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        target = str(call.arguments.get("target_agent", "")).strip()
        text = str(call.arguments.get("message", "")).strip()
        mtype = str(call.arguments.get("type", "") or "consult").strip()
        invalid = self._validate(target, text, mtype)
        if invalid is not None:
            return invalid
        correlation = str(call.arguments.get("correlation_id", "") or "").strip()
        if not correlation:
            correlation = A2A_CORRELATION.get()

        peers, err = self._list_peers()
        if err:
            return ToolResult(content=err, ok=False)
        resolved = self._resolve_peer(peers, target)
        if resolved is None:
            names = ", ".join(str(p.get("name", "?")) for p in peers) or EMPTY_DISPLAY
            return ToolResult(
                content=(
                    f"send_message: no squad-mate named {target!r} found. "
                    f"Squad roster: {names}"
                ),
                ok=False,
            )

        body: dict[str, object] = {
            "to_agent_id": str(resolved.get("id", "")),
            "type": mtype,
            "payload": {"message": text},
        }
        if correlation:
            body["correlation_id"] = correlation
        self._apply_consult_deadline(body, call)
        data, err = self._post_message(body)
        if err:
            return ToolResult(content=f"send_message: {err}", ok=False)
        message_id = str(data.get("id", "")) if isinstance(data, dict) else ""
        return ToolResult(
            content=(
                f"sent {mtype} message to {resolved.get('name', target)}"
                + (f" (message {message_id})" if message_id else "")
            ),
            ok=True,
        )

    # -- control-plane round trips -------------------------------------------

    def _request(self, method: str, path: str, body: dict | None = None):
        url = self.context.control_plane_url.rstrip("/") + path
        payload = json.dumps(body).encode("utf-8") if body is not None else None
        req = request.Request(
            url,
            data=payload,
            method=method,
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 15))
        with request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def _list_peers(self) -> tuple[list, str]:
        try:
            data = self._request("GET", PEERS_PATH)
        except error.HTTPError as exc:
            return [], f"send_message: peers lookup failed: HTTP {exc.code}"
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return [], f"send_message: peers lookup failed: {exc}"
        if not isinstance(data, list):
            return [], "send_message: peers lookup returned an unexpected shape"
        return data, ""

    def _post_message(self, body: dict) -> tuple[dict, str]:
        try:
            data = self._request("POST", "/api/v1/agents/me/messages", body)
        except error.HTTPError as exc:
            detail = ""
            try:
                detail = json.loads(exc.read().decode("utf-8")).get("error", "")
            except Exception:  # noqa: BLE001 — error body is best-effort
                pass
            suffix = f" — {detail}" if detail else ""
            return {}, f"send failed: HTTP {exc.code}{suffix}"
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return {}, f"send failed: {exc}"
        return data if isinstance(data, dict) else {}, ""

    @staticmethod
    def _resolve_peer(peers: list[Mapping[str, object]], target: str) -> Mapping[str, object] | None:
        """Exact (case-insensitive) name match, then a unique prefix match."""
        lowered = target.lower()
        for peer in peers:
            if str(peer.get("name", "")).lower() == lowered:
                return peer
        prefixed = [
            peer
            for peer in peers
            if str(peer.get("name", "")).lower().startswith(lowered)
        ]
        if len(prefixed) == 1:
            return prefixed[0]
        return None


class SendInboxTool:
    """S-193: deliver human-requested content to the squad owner's inbox.

    The agent never addresses a user directly: the control plane routes the
    message to the human who owns the agent's squad. Use this when the
    human asked for something "to my inbox" / "notify me" — the content
    arrives in their email-like Inbox, unread until they open it.

    S-216: optional ``attachments`` — a list of file paths inside the
    agent's workspace. The runtime reads each file and uploads it as a
    multipart part on the same agent-authenticated endpoint; the control
    plane validates the bytes server-side (documents, images, video,
    audio, text scripts are fine; executables are rejected with a clear
    reason). Limits: max 8 files, 25 MiB each.
    """

    name = "send_inbox"

    MAX_ATTACHMENTS = 8
    MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "send_inbox",
                    "description": (
                        "Send a message to your squad's human owner's "
                        "Inbox. Use it when the human asked you to send "
                        "something to their inbox or to notify them. The "
                        "message is delivered unread and stays until they "
                        "delete it. Attach task_id so the inbox entry "
                        "links back to the task. To deliver FILES (a "
                        "report, an image, a script), pass attachments: "
                        "a list of file paths inside your workspace — "
                        "max 8 files, 25 MB each. Executables are "
                        "rejected; documents, images, video, audio and "
                        "text files are fine."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "message": {
                                "type": "string",
                                "description": (
                                    "The content the human asked for. "
                                    "Plain text; keep it concise."
                                ),
                            },
                            "subject": {
                                "type": "string",
                                "description": (
                                    "Optional one-line subject shown in "
                                    "the inbox list."
                                ),
                            },
                            "task_id": {
                                "type": "string",
                                "description": (
                                    "Optional task UUID this message is "
                                    "about (must be in your squad). Adds "
                                    "a navigation link in the inbox."
                                ),
                            },
                            "attachments": {
                                "type": "array",
                                "items": {"type": "string"},
                                "description": (
                                    "Optional file paths (relative to "
                                    "your workspace, or absolute inside "
                                    "it) to attach to the message. Max "
                                    "8 files, 25 MB each. Executable "
                                    "binaries are rejected server-side."
                                ),
                            },
                        },
                        "required": ["message"],
                    },
                },
            }
        ]

    def _attachment_note(self, data: object) -> str:
        """Render the attachment summary appended to the send_inbox result."""
        if not isinstance(data, dict):
            return ""
        attachments = data.get("attachments")
        if not isinstance(attachments, list) or not attachments:
            return ""
        names = ", ".join(
            str(a.get("filename", "?")) for a in attachments if isinstance(a, dict)
        )
        if not names:
            return ""
        return f" with {len(attachments)} attachment(s): {names}"

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        text = str(call.arguments.get("message", "")).strip()
        if not text:
            return ToolResult(content="send_inbox: message is required", ok=False)
        max_chars = int(self.policy.get("maxMessageChars", 10000))
        if len(text) > max_chars:
            return ToolResult(
                content=f"send_inbox: message exceeds {max_chars} characters", ok=False
            )
        body: dict[str, object] = {"message": text}
        subject = str(call.arguments.get("subject", "") or "").strip()
        if subject:
            body["subject"] = subject
        task_id = str(call.arguments.get("task_id", "") or "").strip()
        if task_id:
            body["task_id"] = task_id

        raw_attachments = call.arguments.get("attachments") or []
        if not isinstance(raw_attachments, list):
            return ToolResult(
                content="send_inbox: attachments must be a list of file paths", ok=False
            )
        try:
            if raw_attachments:
                files = self._read_attachments(raw_attachments)
                data = self._post_multipart(body, files)
            else:
                data = self._request("POST", "/api/v1/agents/me/inbox", body)
        except Exception as exc:  # noqa: BLE001 - surface as tool error
            return ToolResult(content=f"send_inbox: {exc}", ok=False)
        message_id = str(data.get("id", "")) if isinstance(data, dict) else ""
        att_note = self._attachment_note(data)
        return ToolResult(
            content=(
                "sent message to the squad owner's inbox"
                + (f" (message {message_id})" if message_id else "")
                + att_note
            ),
            ok=True,
        )

    def _read_attachments(self, paths: list[object]) -> list[tuple[str, bytes]]:
        """Read attachment files, enforcing the workspace boundary and
        size/count caps before any bytes hit the network."""
        if len(paths) > self.MAX_ATTACHMENTS:
            raise ValueError(
                f"too many attachments (max {self.MAX_ATTACHMENTS})"
            )
        workspace = Path(self.context.workspace_dir).resolve()
        files: list[tuple[str, bytes]] = []
        for raw in paths:
            path = Path(str(raw))
            if not path.is_absolute():
                path = workspace / path
            resolved = path.resolve()
            if resolved != workspace and workspace not in resolved.parents:
                raise ValueError(
                    f"attachment {str(raw)!r} is outside your workspace"
                )
            if not resolved.is_file():
                raise ValueError(f"attachment {str(raw)!r} not found")
            size = resolved.stat().st_size
            if size > self.MAX_ATTACHMENT_BYTES:
                raise ValueError(
                    f"attachment {resolved.name!r} exceeds "
                    f"{self.MAX_ATTACHMENT_BYTES // (1024 * 1024)} MB"
                )
            files.append((resolved.name, resolved.read_bytes()))
        return files

    def _post_multipart(self, fields: dict[str, object], files: list[tuple[str, bytes]]) -> dict:
        """POST the message fields + file parts as multipart/form-data."""
        boundary = f"----skquad-inbox-{os.urandom(16).hex()}"
        buf = bytearray()
        for key, value in fields.items():
            buf += f"--{boundary}\r\n".encode()
            buf += (
                f'Content-Disposition: form-data; name="{key}"\r\n\r\n'.encode()
            )
            buf += str(value).encode("utf-8")
            buf += b"\r\n"
        for filename, data in files:
            safe_name = filename.replace('"', "")
            buf += f"--{boundary}\r\n".encode()
            buf += (
                f'Content-Disposition: form-data; name="attachments"; '
                f'filename="{safe_name}"\r\n'.encode()
            )
            buf += b"Content-Type: application/octet-stream\r\n\r\n"
            buf += data
            buf += b"\r\n"
        buf += f"--{boundary}--\r\n".encode()

        url = self.context.control_plane_url.rstrip("/") + "/api/v1/agents/me/inbox"
        req = request.Request(
            url,
            data=bytes(buf),
            method="POST",
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": f"multipart/form-data; boundary={boundary}",
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 30))
        try:
            with request.urlopen(req, timeout=timeout) as response:
                raw = response.read()
        except error.HTTPError as exc:
            detail = ""
            try:
                err_body = json.loads(exc.read().decode("utf-8"))
                detail = str(err_body.get("error", {}).get("message", "")) or ""
            except Exception:  # noqa: BLE001
                pass
            suffix = f" — {detail}" if detail else ""
            raise RuntimeError(f"send failed: HTTP {exc.code}{suffix}") from exc
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def _request(self, method: str, path: str, body: dict | None = None):
        url = self.context.control_plane_url.rstrip("/") + path
        payload = json.dumps(body).encode("utf-8") if body is not None else None
        req = request.Request(
            url,
            data=payload,
            method=method,
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 15))
        with request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))


class NotifyOwnerTool:
    """Platform-prompt awareness: direct escalation to the squad owner.

    Wraps ``POST /api/v1/agents/me/notify-owner``: the control plane
    files an ``action_required`` InboxMessage against the agent's squad
    owner (audited; 404 when the squad has no owner). No squad-mate
    resolution happens runtime-side — the control plane owns routing. The
    message cap mirrors the server-side ``maxInboxMessageChars`` (10000,
    raised from 2000 in S-229) so an over-long message fails fast with a
    clear error instead of being silently trimmed.
    """

    name = "notify_owner"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "notify_owner",
                    "description": (
                        "Drop a message directly into your squad owner's "
                        "inbox (action_required). Use for escalations "
                        "that need the owner regardless of task lifecycle "
                        "— approvals, blockers outside your task, urgent "
                        "notices."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "message": {
                                "type": "string",
                                "description": (
                                    "What the owner needs to see and act "
                                    "on. Plain text; be precise and "
                                    "self-contained."
                                ),
                            },
                        },
                        "required": ["message"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        text = str(call.arguments.get("message", "")).strip()
        if not text:
            return ToolResult(content="notify_owner: message is required", ok=False)
        max_chars = int(self.policy.get("maxMessageChars", 10000))
        if len(text) > max_chars:
            return ToolResult(
                content=f"notify_owner: message exceeds {max_chars} characters", ok=False
            )
        try:
            data = self._request(
                "POST", "/api/v1/agents/me/notify-owner", {"message": text}
            )
        except error.HTTPError as exc:
            detail = ""
            try:
                detail = json.loads(exc.read().decode("utf-8")).get("error", "")
            except Exception:  # noqa: BLE001 — error body is best-effort
                pass
            suffix = f" — {detail}" if detail else ""
            return ToolResult(
                content=f"notify_owner: HTTP {exc.code}{suffix}", ok=False
            )
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"notify_owner: {exc}", ok=False)
        message_id = str(data.get("id", "")) if isinstance(data, dict) else ""
        return ToolResult(
            content=(
                "notified the squad owner (action_required)"
                + (f" (message {message_id})" if message_id else "")
            ),
            ok=True,
        )

    def _request(self, method: str, path: str, body: dict | None = None):
        url = self.context.control_plane_url.rstrip("/") + path
        payload = json.dumps(body).encode("utf-8") if body is not None else None
        req = request.Request(
            url,
            data=payload,
            method=method,
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 15))
        with request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))


class MemorySearchTool:
    """S-212: semantic recall over the agent's own long-term memory.

    The embedding + pgvector retrieval happen control-plane-side
    (``GET /api/v1/agents/me/memory/search``); the tool only formats
    the ranked hits for the model. Results are hard-scoped to this
    agent's memories and exclude rejected rows — the same auth model
    as every other ``/agents/me`` surface. The runtime never sees the
    embedder or the gateway key.
    """

    name = "memory_search"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "memory_search",
                    "description": (
                        "Search your own long-term memory (past task "
                        "completions and archived chat transcripts) by "
                        "meaning, not keywords. Returns the most "
                        "similar memories with relevance scores."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "query": {
                                "type": "string",
                                "description": "What to recall, in natural language.",
                            },
                            "limit": {
                                "type": "integer",
                                "description": (
                                    "Maximum number of memories to "
                                    "return (1-20, default 5)."
                                ),
                            },
                        },
                        "required": ["query"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        query = str(call.arguments.get("query", ""))
        if not query.strip():
            return ToolResult(content="memory_search: empty query", ok=False)

        params = [f"q={quote(query)}"]
        limit = call.arguments.get("limit")
        if limit is None:
            limit = self.policy.get("maxResults")
        if limit is not None:
            params.append(f"limit={int(limit)}")

        timeout = int(self.policy.get("timeoutSeconds", 30))
        url = (
            self.context.control_plane_url.rstrip("/")
            + "/api/v1/agents/me/memory/search?"
            + "&".join(params)
        )
        req = request.Request(
            url,
            method="GET",
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        try:
            with request.urlopen(req, timeout=timeout) as response:
                data = json.loads(response.read().decode("utf-8"))
        except error.HTTPError as exc:
            detail = ""
            try:
                detail = json.loads(exc.read().decode("utf-8")).get("error", "")
            except Exception:  # noqa: BLE001 — error body is best-effort
                pass
            suffix = f" — {detail}" if detail else ""
            return ToolResult(
                content=f"memory_search: HTTP {exc.code}{suffix}", ok=False
            )
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"memory_search: request failed: {exc}", ok=False)

        results = data.get("results") or []
        if not results:
            return ToolResult(content="memory_search: no matching memories", ok=True)

        lines: list[str] = []
        for item in results:
            content = str(item.get("content", "")).strip()
            score = item.get("score")
            score_text = f"{float(score):.3f}" if isinstance(score, (int, float)) else "?"
            memory_id = str(item.get("id", ""))
            created = str(item.get("created_at", ""))
            lines.append(
                f"[score {score_text} | memory {memory_id} | {created}]\n{content}"
            )
        return ToolResult(content="\n\n".join(lines), ok=True)


# ---------------------------------------------------------------------------
# S-279: file tools (read / write / edit / apply_patch).
#
# All four are jailed to the agent's workspace_dir — the same directory
# exec runs in. _resolve_in_workspace() is the single security boundary:
# every path from a model goes through realpath resolution and must land
# on the workspace root or strictly under it, so ".." traversals,
# absolute paths elsewhere on the host, and symlinks pointing outside
# the workspace are all rejected before any filesystem mutation.
# ---------------------------------------------------------------------------

# Extensions treated as images by `read`: binary content is never dumped
# into the model context; a short attachment marker is returned instead.
IMAGE_EXTENSIONS: frozenset[str] = frozenset({".png", ".jpg", ".jpeg", ".gif", ".webp"})

DEFAULT_FILE_MAX_BYTES = 262144  # 256 KiB, mirrors the web_fetch content cap


class WorkspaceEscapeError(Exception):
    """Raised when a requested path resolves outside the agent workspace."""


def _resolve_in_workspace(workspace_dir: str, path: str) -> Path:
    """Resolve ``path`` under ``workspace_dir`` or raise WorkspaceEscapeError.

    Join + realpath (via Path.resolve, non-strict so not-yet-existing
    targets still resolve lexically through symlinks that DO exist), then
    assert the result is the workspace itself or strictly under
    ``realpath(workspace) + os.sep``. This kills:
      * ``../../etc/passwd`` style traversals,
      * absolute paths pointing anywhere else on the host,
      * symlinks inside the workspace that point outside it
        (realpath follows the link before the containment check).
    """
    raw = str(path or "").strip()
    if not raw:
        raise WorkspaceEscapeError("path is required")
    base = Path(workspace_dir).resolve()
    candidate = Path(raw)
    resolved = candidate if candidate.is_absolute() else (base / candidate)
    resolved = resolved.resolve()
    if resolved != base and not str(resolved).startswith(str(base) + os.sep):
        raise WorkspaceEscapeError(
            f"path escapes workspace: {raw!r} resolves to {str(resolved)!r}"
        )
    return resolved


def _relpath_in_workspace(workspace_dir: str, resolved: Path) -> str:
    base = Path(workspace_dir).resolve()
    if resolved == base:
        return "."
    return str(resolved.relative_to(base))


def _max_bytes(policy: dict) -> int:
    try:
        return int(policy.get("maxBytes", DEFAULT_FILE_MAX_BYTES))
    except (TypeError, ValueError):
        return DEFAULT_FILE_MAX_BYTES


class ReadTool:
    """Read a text file from the agent workspace (S-279)."""

    name = "read"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "read",
                    "description": (
                        "Read a text file from the agent's task workspace. "
                        "Optional offset (1-indexed line) and limit (max lines). "
                        "Image files return an attachment marker, not binary."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "path": {
                                "type": "string",
                                "description": PATH_DESC_WORKSPACE_RELATIVE,
                            },
                            "offset": {
                                "type": "integer",
                                "description": "First line to read (1-indexed).",
                            },
                            "limit": {
                                "type": "integer",
                                "description": "Maximum number of lines to return.",
                            },
                        },
                        "required": ["path"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        try:
            target = _resolve_in_workspace(self.context.workspace_dir, call.arguments.get("path", ""))
        except WorkspaceEscapeError as exc:
            return ToolResult(content=f"read: {exc}", ok=False)

        if not target.exists():
            return ToolResult(content=f"read: file not found: {_relpath_in_workspace(self.context.workspace_dir, target)}", ok=False)
        if target.is_dir():
            return ToolResult(content=f"read: {target.name!r} is a directory, not a file", ok=False)
        if target.suffix.lower() in IMAGE_EXTENSIONS:
            size = target.stat().st_size
            return ToolResult(
                content=(
                    f"[attachment: {_relpath_in_workspace(self.context.workspace_dir, target)} "
                    f"({target.suffix.lower()} image, {size} bytes) — binary content not returned]"
                ),
                ok=True,
            )

        max_bytes = _max_bytes(self.policy)
        try:
            with open(target, "rb") as fh:
                data = fh.read(max_bytes + 1)
        except OSError as exc:
            return ToolResult(content=f"read: {exc}", ok=False)

        byte_truncated = len(data) > max_bytes
        text = data[:max_bytes].decode("utf-8", errors="replace")
        lines = text.split("\n")
        if byte_truncated and text.endswith("\ufffd"):
            # Drop a partial multi-byte char cut mid-sequence.
            lines[-1] = lines[-1][:-1]

        total_lines = len(lines)
        offset = call.arguments.get("offset")
        limit = call.arguments.get("limit")
        start = 0
        if offset is not None:
            try:
                start = max(int(offset) - 1, 0)
            except (TypeError, ValueError):
                return ToolResult(content="read: offset must be an integer", ok=False)
        if start >= total_lines:
            return ToolResult(content=f"read: offset {start + 1} is past end of file ({total_lines} lines)", ok=False)
        end = total_lines
        if limit is not None:
            try:
                end = min(start + max(int(limit), 0), total_lines)
            except (TypeError, ValueError):
                return ToolResult(content="read: limit must be an integer", ok=False)

        selected = lines[start:end]
        content = "\n".join(selected)
        notices: list[str] = []
        if byte_truncated:
            notices.append(f"content truncated at maxBytes={max_bytes}")
        if end < total_lines:
            notices.append(f"showing lines {start + 1}-{end} of {total_lines} (limit reached)")
        elif start > 0:
            notices.append(f"showing lines {start + 1}-{end} of {total_lines}")
        if notices:
            content += "\n[" + "; ".join(notices) + "]"
        return ToolResult(content=content, ok=True)


class WriteTool:
    """Create/overwrite a file in the agent workspace (S-279)."""

    name = "write"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "write",
                    "description": (
                        "Create or overwrite a text file in the agent's task "
                        "workspace. Parent directories are created as needed."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "path": {
                                "type": "string",
                                "description": PATH_DESC_WORKSPACE_RELATIVE,
                            },
                            "content": {
                                "type": "string",
                                "description": "Full file content.",
                            },
                        },
                        "required": ["path", "content"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        if "content" not in call.arguments:
            return ToolResult(content="write: missing content", ok=False)
        content = str(call.arguments.get("content", ""))
        try:
            target = _resolve_in_workspace(self.context.workspace_dir, call.arguments.get("path", ""))
        except WorkspaceEscapeError as exc:
            return ToolResult(content=f"write: {exc}", ok=False)
        if target.is_dir():
            return ToolResult(content=f"write: {target.name!r} is a directory", ok=False)
        payload = content.encode("utf-8")
        max_bytes = _max_bytes(self.policy)
        if len(payload) > max_bytes:
            return ToolResult(
                content=f"write: content exceeds maxBytes={max_bytes} (got {len(payload)})", ok=False
            )
        try:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(payload)
        except OSError as exc:
            return ToolResult(content=f"write: {exc}", ok=False)
        rel = _relpath_in_workspace(self.context.workspace_dir, target)
        return ToolResult(content=f"wrote {len(payload)} bytes to {rel}", ok=True)


class EditTool:
    """Exact-match, all-or-nothing edits against one file (S-279).

    Every oldText must occur EXACTLY ONCE in the ORIGINAL file and the
    matched spans must not overlap. Any violation → error, nothing is
    written. Applied in one pass so partial edits can never land.
    """

    name = "edit"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "edit",
                    "description": (
                        "Apply exact-match edits to a file in the agent's task "
                        "workspace. Each oldText must occur exactly once in the "
                        "original file; edits must not overlap. All edits apply "
                        "atomically — any failure writes nothing."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "path": {
                                "type": "string",
                                "description": PATH_DESC_WORKSPACE_RELATIVE,
                            },
                            "edits": {
                                "type": "array",
                                "items": {
                                    "type": "object",
                                    "properties": {
                                        "oldText": {"type": "string"},
                                        "newText": {"type": "string"},
                                    },
                                    "required": ["oldText", "newText"],
                                },
                            },
                        },
                        "required": ["path", "edits"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        try:
            target = _resolve_in_workspace(self.context.workspace_dir, call.arguments.get("path", ""))
        except WorkspaceEscapeError as exc:
            return ToolResult(content=f"edit: {exc}", ok=False)
        if not target.is_file():
            return ToolResult(content=f"edit: file not found: {_relpath_in_workspace(self.context.workspace_dir, target)}", ok=False)
        edits = call.arguments.get("edits")
        if not isinstance(edits, list) or not edits:
            return ToolResult(content="edit: edits must be a non-empty array of {oldText,newText}", ok=False)

        try:
            original = target.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as exc:
            return ToolResult(content=f"edit: cannot read file: {exc}", ok=False)

        # Phase 1: locate every edit in the ORIGINAL text (validate-before-write).
        spans: list[tuple[int, int, str]] = []  # (start, end, newText)
        for idx, item in enumerate(edits):
            if not isinstance(item, dict) or "oldText" not in item or "newText" not in item:
                return ToolResult(content=f"edit: edits[{idx}] must have oldText and newText", ok=False)
            old = str(item["oldText"])
            new = str(item["newText"])
            if old == "":
                return ToolResult(content=f"edit: edits[{idx}].oldText must not be empty", ok=False)
            occurrences = _find_all(original, old)
            if not occurrences:
                return ToolResult(
                    content=f"edit: oldText not found in file (edit #{idx + 1}): {old[:80]!r}", ok=False
                )
            if len(occurrences) > 1:
                return ToolResult(
                    content=(
                        f"edit: oldText is ambiguous — occurs {len(occurrences)} times "
                        f"(edit #{idx + 1}); include more context. Nothing was written."
                    ),
                    ok=False,
                )
            spans.append((occurrences[0], occurrences[0] + len(old), new))

        # Phase 2: overlap check on the original-text spans.
        spans.sort(key=lambda s: s[0])
        for prev, nxt in zip(spans, spans[1:]):
            if nxt[0] < prev[1]:
                return ToolResult(
                    content="edit: edits overlap in the original file; nothing was written", ok=False
                )

        # Phase 3: build the new content in memory, then a single write.
        out: list[str] = []
        cursor = 0
        for start, end, new in spans:
            out.append(original[cursor:start])
            out.append(new)
            cursor = end
        out.append(original[cursor:])
        updated = "".join(out)

        payload = updated.encode("utf-8")
        max_bytes = _max_bytes(self.policy)
        if len(payload) > max_bytes:
            return ToolResult(
                content=f"edit: result exceeds maxBytes={max_bytes} (got {len(payload)}); nothing was written", ok=False
            )
        try:
            target.write_bytes(payload)
        except OSError as exc:
            return ToolResult(content=f"edit: write failed: {exc}", ok=False)
        rel = _relpath_in_workspace(self.context.workspace_dir, target)
        return ToolResult(content=f"applied {len(edits)} edit(s) to {rel}", ok=True)


def _find_all(haystack: str, needle: str) -> list[int]:
    """All (non-overlapping-by-nature, exact) start indices of needle."""
    idxs: list[int] = []
    start = haystack.find(needle)
    while start != -1:
        idxs.append(start)
        start = haystack.find(needle, start + 1)
    return idxs


class ApplyPatchTool:
    """Apply an OpenClaw-style *** Begin Patch / End Patch changeset (S-279).

    Supports Add File / Update File (with @@ context hunks) / Delete
    File. Fully atomic: the whole patch is parsed and applied in memory
    first; any hunk mismatch reports the offending file/hunk and writes
    nothing.
    """

    name = "apply_patch"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "apply_patch",
                    "description": (
                        "Apply a structured patch (*** Begin Patch / *** End Patch) "
                        "with Add File / Update File / Delete File sections. "
                        "All-or-nothing: a failing hunk writes nothing."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "patch": {
                                "type": "string",
                                "description": "The full patch text.",
                            }
                        },
                        "required": ["patch"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        patch = str(call.arguments.get("patch", ""))
        if "*** Begin Patch" not in patch or "*** End Patch" not in patch:
            return ToolResult(content="apply_patch: patch must contain *** Begin Patch / *** End Patch", ok=False)

        try:
            adds, updates, deletes = self._parse(patch)
        except ValueError as exc:
            return ToolResult(content=f"apply_patch: {exc}", ok=False)

        ws = self.context.workspace_dir
        # Resolve every touched path BEFORE touching the filesystem.
        try:
            add_paths = [(self._check_path(ws, p), content) for p, content in adds]
            update_paths = [(self._check_path(ws, p), hunks) for p, hunks in updates]
            delete_paths = [self._check_path(ws, p) for p in deletes]
        except WorkspaceEscapeError as exc:
            return ToolResult(content=f"apply_patch: {exc}", ok=False)

        # Phase 1: compute all new contents in memory.
        new_files: dict[Path, bytes] = {}
        for path, content in add_paths:
            if path.exists():
                return ToolResult(content=f"apply_patch: cannot add, file already exists: {_relpath_in_workspace(ws, path)}", ok=False)
            new_files[path] = content.encode("utf-8")

        for path, hunks in update_paths:
            if not path.is_file():
                return ToolResult(content=f"apply_patch: cannot update, file not found: {_relpath_in_workspace(ws, path)}", ok=False)
            try:
                text = path.read_text(encoding="utf-8")
            except (OSError, UnicodeDecodeError) as exc:
                return ToolResult(content=f"apply_patch: cannot read {_relpath_in_workspace(ws, path)}: {exc}", ok=False)
            updated_text, err = self._apply_hunks(text, hunks, _relpath_in_workspace(ws, path))
            if err:
                return ToolResult(content=f"apply_patch: {err}; no files were written", ok=False)
            new_files[path] = updated_text.encode("utf-8")

        # maxBytes caps the total bytes written across the patch's files.
        max_bytes = _max_bytes(self.policy)
        total = sum(len(b) for b in new_files.values())
        if total > max_bytes:
            return ToolResult(
                content=f"apply_patch: total write {total} bytes exceeds maxBytes={max_bytes}; no files were written", ok=False
            )

        # Phase 2: commit — writes then deletes.
        try:
            for path, payload in new_files.items():
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(payload)
            for path in delete_paths:
                if path.is_file():
                    path.unlink()
                elif not path.exists():
                    return ToolResult(content=f"apply_patch: cannot delete, file not found: {_relpath_in_workspace(ws, path)}", ok=False)
        except OSError as exc:
            return ToolResult(content=f"apply_patch: write phase failed: {exc}", ok=False)

        parts = []
        if adds:
            parts.append(f"added {len(adds)}")
        if updates:
            parts.append(f"updated {len(updates)}")
        if deletes:
            parts.append(f"deleted {len(deletes)}")
        return ToolResult(content="apply_patch ok: " + ", ".join(parts) + " file(s)", ok=True)

    @staticmethod
    def _check_path(ws: str, rel: str) -> Path:
        return _resolve_in_workspace(ws, rel)

    @staticmethod
    def _parse(patch: str) -> tuple[list[tuple[str, str]], list[tuple[str, list[list[str]]]], list[str]]:
        """Parse the patch into (adds, updates, deletes).

        adds:    [(path, full_content)]
        updates: [(path, [hunk_lines, ...])]  (hunk = raw +/-/context lines)
        deletes: [path]
        Raises ValueError with a clear message on malformed input.
        """
        lines = patch.split("\n")
        try:
            begin = lines.index("*** Begin Patch")
            end = lines.index("*** End Patch")
        except ValueError:
            raise ValueError("malformed patch: missing Begin/End markers")
        if end < begin:
            raise ValueError("malformed patch: End Patch before Begin Patch")

        adds: list[tuple[str, str]] = []
        updates: list[tuple[str, list[list[str]]]] = []
        deletes: list[str] = []

        body = lines[begin + 1 : end]
        i = 0
        while i < len(body):
            line = body[i]
            if line.startswith(DIFF_ADD_PREFIX):
                path = line[len(DIFF_ADD_PREFIX):].strip()
                i += 1
                content: list[str] = []
                while i < len(body) and not body[i].startswith("*** "):
                    if not body[i].startswith("+"):
                        raise ValueError(f"Add File {path!r}: expected '+' lines, got {body[i]!r}")
                    content.append(body[i][1:])
                    i += 1
                adds.append((path, "\n".join(content) + ("\n" if content else "")))
            elif line.startswith(DIFF_UPDATE_PREFIX):
                path = line[len(DIFF_UPDATE_PREFIX):].strip()
                i += 1
                hunks: list[list[str]] = []
                current: list[str] | None = None
                while i < len(body) and not body[i].startswith(DIFF_ADD_PREFIX) and not body[i].startswith(DIFF_UPDATE_PREFIX) and not body[i].startswith(DIFF_DELETE_PREFIX):
                    hline = body[i]
                    if hline.startswith("@@"):
                        if current:
                            hunks.append(current)
                        current = []
                    elif hline.startswith("*** Move to:"):
                        raise ValueError(f"Update File {path!r}: 'Move to' (rename) is not supported")
                    elif hline.startswith("***"):
                        break
                    else:
                        if current is None:
                            current = []
                        current.append(hline)
                    i += 1
                if current:
                    hunks.append(current)
                if not hunks:
                    raise ValueError(f"Update File {path!r}: no hunks")
                updates.append((path, hunks))
            elif line.startswith(DIFF_DELETE_PREFIX):
                path = line[len(DIFF_DELETE_PREFIX):].strip()
                deletes.append(path)
                i += 1
            elif line.strip() == "":
                i += 1
            else:
                raise ValueError(f"unexpected patch line: {line!r}")
        return adds, updates, deletes

    @staticmethod
    def _apply_hunks(text: str, hunks: list[list[str]], fname: str) -> tuple[str, str]:
        """Apply hunks to text; return (new_text, "") or (text, error).

        Each hunk is a sequence of ' ' context, '-' removal and '+'
        addition lines. The search block (context+removal) is located in
        the current text and replaced by the replacement block
        (context+addition). Matches are applied sequentially through the
        file (hunks are expected in order).
        """
        lines = text.split("\n")
        trailing_newline = text.endswith("\n")
        if trailing_newline:
            lines = lines[:-1]
        cursor = 0
        for h_idx, hunk in enumerate(hunks, start=1):
            search = [l[1:] for l in hunk if l[:1] in (" ", "-") or (l == "")]
            replace = [l[1:] for l in hunk if l[:1] in (" ", "+") or (l == "")]
            # Normalise: lines that are exactly "" in a hunk mean an empty
            # context line; keep them as "".
            if not search:
                # Pure-addition hunk: insert at cursor.
                lines[cursor:cursor] = replace
                continue
            found = -1
            for start in range(cursor, len(lines) - len(search) + 1):
                if lines[start : start + len(search)] == search:
                    found = start
                    break
            if found == -1:
                # Retry from the top of the file (hunks not strictly ordered).
                for start in range(0, len(lines) - len(search) + 1):
                    if lines[start : start + len(search)] == search:
                        found = start
                        break
            if found == -1:
                return text, f"hunk {h_idx} in {fname!r} did not match (context line: {search[0][:80]!r})"
            lines[found : found + len(search)] = replace
            cursor = found + len(replace)
        new_text = "\n".join(lines)
        if trailing_newline:
            new_text += "\n"
        return new_text, ""


# ---------------------------------------------------------------------------
# S-281: built-in CRON / scheduled tasks.
#
# The scheduler lives in the control plane (httpapi/scheduled_jobs.go):
# due jobs are claimed atomically and fired through the canonical
# agent-run path (materialized task + busy mirror). This tool is a thin
# client over the agent-scoped REST surface
# (/api/v1/agents/me/scheduled-jobs) using the same bearer credential
# as send_inbox/notify_owner — the runtime never schedules anything
# itself, so jobs survive pod restarts and scale-to-zero.
#
# Client-side pre-validation mirrors the server's bounds (domain/
# scheduled_jobs.go) so the model gets a friendly error instead of a
# round-trip 400: every interval 60..7_776_000s, message <= 10000
# chars, kind in {at, every, cron}, cron = standard 5-field expression.
# The server remains the source of truth; maxActiveJobs enforcement
# here is best-effort (racy by design).
# ---------------------------------------------------------------------------

CRON_MIN_INTERVAL_SECONDS = 60
CRON_MAX_INTERVAL_SECONDS = 90 * 24 * 60 * 60  # 90 days, mirrors domain.MaxEverySeconds
CRON_MAX_MESSAGE_CHARS = 10000  # mirrors domain.MaxScheduledMessageChars
CRON_DEFAULT_MAX_ACTIVE_JOBS = 10
CRON_KINDS: frozenset[str] = frozenset({"at", "every", "cron"})


def _parse_rfc3339(raw: str) -> "datetime | None":
    """Parse an RFC3339 timestamp ('Z' suffix accepted); None on failure."""
    from datetime import datetime, timezone  # noqa: PLC0415 - keep module stdlib-light

    text = str(raw or "").strip()
    if not text:
        return None
    if text.endswith(("Z", "z")):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        # RFC3339 requires an offset; treat naive input as invalid rather
        # than silently assuming a local zone.
        return None
    return parsed.astimezone(timezone.utc)


def _validate_cron_expression(expr: str) -> str:
    """Cheap client-side check: standard 5-field cron. Returns '' or an error."""
    fields = str(expr or "").split()
    if len(fields) != 5:
        return (
            f"cron expression must have exactly 5 fields "
            f"(minute hour day-of-month month day-of-week), got {len(fields)}"
        )
    for field in fields:
        if not re.fullmatch(r"[0-9A-Za-z*,/\-]+", field):
            return f"cron field {field!r} contains invalid characters"
    return ""


def _validate_cron_timezone(tz: str) -> str:
    """Validate an IANA timezone name; '' when valid or unverifiable."""
    name = str(tz or "").strip()
    if not name:
        return ""  # server defaults to UTC
    try:
        from zoneinfo import ZoneInfo, ZoneInfoNotFoundError  # noqa: PLC0415
    except ImportError:  # pragma: no cover - py<3.9
        return ""
    try:
        ZoneInfo("UTC")
    except ZoneInfoNotFoundError:
        return ""  # no tz database on this host — let the server validate
    try:
        ZoneInfo(name)
    except (ZoneInfoNotFoundError, ValueError):
        return f"{tz!r} is not a valid IANA timezone (e.g. Australia/Adelaide)"
    return ""


class CronTool:
    """S-281: manage this agent's scheduled jobs (built-in CRON).

    Actions: add (create a job), list (this agent's jobs), remove
    (delete), pause (enabled=false), resume (enabled=true, next run
    recomputed from now). Jobs fire control-plane-side: each fire
    materializes a task on the agent's board with the job's message as
    the prompt, exactly like a delegate/handoff.
    """

    name = "cron"

    ACTIONS = ("add", "list", "remove", "pause", "resume")

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    @property
    def max_active_jobs(self) -> int:
        try:
            return int(self.policy.get("maxActiveJobs", CRON_DEFAULT_MAX_ACTIVE_JOBS))
        except (TypeError, ValueError):
            return CRON_DEFAULT_MAX_ACTIVE_JOBS

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "cron",
                    "description": (
                        "Schedule your own future work. Actions: add "
                        "(create a scheduled job), list (your jobs "
                        "with next_run times), remove (delete a job), "
                        "pause, resume (next run recomputed from now). "
                        "When a job fires it creates a task on your "
                        "board with the job's message as the prompt, so "
                        "you wake and run it like any assigned task. "
                        "Schedules: kind='at' with an RFC3339 timestamp "
                        "(one-shot, must be in the future), "
                        "kind='every' with interval_seconds "
                        "(60..7776000), or kind='cron' with a standard "
                        "5-field expression plus optional IANA timezone "
                        "(default UTC)."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "action": {
                                "type": "string",
                                "enum": ["add", "list", "remove", "pause", "resume"],
                                "description": "Management action to perform.",
                            },
                            "message": {
                                "type": "string",
                                "description": (
                                    "add only: the prompt delivered to "
                                    "you on each fire (<= 10000 chars)."
                                ),
                            },
                            "title": {
                                "type": "string",
                                "description": (
                                    "add only: optional title for the "
                                    "task created when the job fires."
                                ),
                            },
                            "schedule": {
                                "type": "object",
                                "description": (
                                    "add only: {kind, at?, "
                                    "interval_seconds?, expression?, "
                                    "timezone?}. kind=at needs at; "
                                    "kind=every needs interval_seconds; "
                                    "kind=cron needs expression "
                                    "(timezone optional, default UTC)."
                                ),
                                "properties": {
                                    "kind": {
                                        "type": "string",
                                        "enum": ["at", "every", "cron"],
                                    },
                                    "at": {
                                        "type": "string",
                                        "description": (
                                            "RFC3339 timestamp, e.g. "
                                            "2026-10-21T19:30:00+10:30"
                                        ),
                                    },
                                    "interval_seconds": {"type": "integer"},
                                    "expression": {
                                        "type": "string",
                                        "description": "5-field cron, e.g. '0 9 * * 1-5'",
                                    },
                                    "timezone": {
                                        "type": "string",
                                        "description": "IANA tz, e.g. Australia/Adelaide",
                                    },
                                },
                                "required": ["kind"],
                            },
                            "job_id": {
                                "type": "string",
                                "description": (
                                    "Required for remove/pause/resume."
                                ),
                            },
                        },
                        "required": ["action"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        action = str(call.arguments.get("action", "")).strip().lower()
        if action == "list":
            return self._list()
        if action == "add":
            return self._add(call)
        if action in ("remove", "pause", "resume"):
            return self._job_action(action, call)
        return ToolResult(
            content="cron: action must be one of add/list/remove/pause/resume", ok=False
        )

    # -- actions -------------------------------------------------------------

    def _add(self, call: ToolCall) -> ToolResult:
        message = str(call.arguments.get("message", "")).strip()
        if not message:
            return ToolResult(content="cron add: message is required", ok=False)
        if len(message) > CRON_MAX_MESSAGE_CHARS:
            return ToolResult(
                content=(
                    f"cron add: message exceeds {CRON_MAX_MESSAGE_CHARS} "
                    f"characters (got {len(message)})"
                ),
                ok=False,
            )
        schedule = call.arguments.get("schedule")
        if not isinstance(schedule, dict):
            return ToolResult(
                content=(
                    "cron add: schedule must be an object "
                    "{kind, at?/interval_seconds?/expression+timezone?}"
                ),
                ok=False,
            )
        kind = str(schedule.get("kind", "")).strip().lower()
        if kind not in CRON_KINDS:
            return ToolResult(
                content=f"cron add: unknown schedule kind {kind!r} (want at, every or cron)",
                ok=False,
            )
        body: dict[str, object] = {"message": message, "schedule": {"kind": kind}}
        title = str(call.arguments.get("title", "") or "").strip()
        if title:
            body["title"] = title
        if kind == "at":
            at = str(schedule.get("at", "") or "").strip()
            if not at:
                return ToolResult(content="cron add: schedule.at is required for kind=at", ok=False)
            if _parse_rfc3339(at) is None:
                return ToolResult(
                    content=(
                        "cron add: schedule.at must be an RFC3339 timestamp "
                        "with a UTC offset (e.g. 2026-10-21T19:30:00+10:30)"
                    ),
                    ok=False,
                )
            body["schedule"]["at"] = at
        elif kind == "every":
            raw_interval = schedule.get("interval_seconds")
            if raw_interval is None:
                return ToolResult(
                    content="cron add: schedule.interval_seconds is required for kind=every",
                    ok=False,
                )
            try:
                interval = int(raw_interval)
            except (TypeError, ValueError):
                return ToolResult(
                    content="cron add: schedule.interval_seconds must be a whole number of seconds",
                    ok=False,
                )
            if interval < CRON_MIN_INTERVAL_SECONDS or interval > CRON_MAX_INTERVAL_SECONDS:
                return ToolResult(
                    content=(
                        f"cron add: interval_seconds {interval} out of range "
                        f"[{CRON_MIN_INTERVAL_SECONDS}, {CRON_MAX_INTERVAL_SECONDS}] "
                        "(1 minute to 90 days)"
                    ),
                    ok=False,
                )
            body["schedule"]["interval_seconds"] = interval
        else:  # cron
            expression = str(schedule.get("expression", "") or "").strip()
            if not expression:
                return ToolResult(
                    content="cron add: schedule.expression is required for kind=cron",
                    ok=False,
                )
            expr_err = _validate_cron_expression(expression)
            if expr_err:
                return ToolResult(content=f"cron add: {expr_err}", ok=False)
            tz = str(schedule.get("timezone", "") or "").strip()
            tz_err = _validate_cron_timezone(tz)
            if tz_err:
                return ToolResult(content=f"cron add: {tz_err}", ok=False)
            body["schedule"]["expression"] = expression
            if tz:
                body["schedule"]["timezone"] = tz

        cap_err = self._check_active_cap()
        if cap_err:
            return ToolResult(content=f"cron add: {cap_err}", ok=False)

        try:
            data = self._request("POST", "/api/v1/agents/me/scheduled-jobs", body)
        except error.HTTPError as exc:
            return ToolResult(content=f"cron add: {self._http_detail(exc)}", ok=False)
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"cron add: {exc}", ok=False)
        job_id = str(data.get("id", "")) if isinstance(data, dict) else ""
        next_run = str(data.get("next_run_at", "")) if isinstance(data, dict) else ""
        return ToolResult(
            content=(
                f"created scheduled job {job_id} ({kind})"
                + (f" — next run {next_run}" if next_run else "")
            ),
            ok=True,
        )

    def _check_active_cap(self) -> str:
        """Best-effort client-side maxActiveJobs guard (server is authoritative)."""
        jobs, err = self._get_jobs()
        if err:
            # Cap check is best-effort: if the list fails, let the server
            # enforce and surface its own error on the POST.
            return ""
        active = sum(1 for job in jobs if job.get("enabled"))
        if active >= self.max_active_jobs:
            return (
                f"you already have {active} active scheduled jobs "
                f"(maxActiveJobs={self.max_active_jobs}); cancel or "
                "pause one first"
            )
        return ""

    def _list(self) -> ToolResult:
        jobs, err = self._get_jobs()
        if err:
            return ToolResult(content=f"cron list: {err}", ok=False)
        if not jobs:
            return ToolResult(content="cron: no scheduled jobs", ok=True)
        lines: list[str] = []
        for job in jobs:
            job_id = str(job.get("id", "?"))
            state = "enabled" if job.get("enabled") else "paused"
            summary = self._schedule_summary(job)
            next_run = str(job.get("next_run_at", "")) or "—"
            last = str(job.get("last_run_status", "") or "")
            extras = []
            if last:
                extras.append(f"last={last}")
            fail_count = job.get("fail_count")
            if isinstance(fail_count, int) and fail_count > 0:
                extras.append(f"fails={fail_count}")
            reason = str(job.get("disabled_reason", "") or "")
            if reason:
                extras.append(reason)
            title = str(job.get("title", "") or "")
            label = f" {title!r}" if title else ""
            suffix = f" ({'; '.join(extras)})" if extras else ""
            lines.append(f"{job_id} [{state}] {summary} next={next_run}{label}{suffix}")
        return ToolResult(content="\n".join(lines), ok=True)

    @staticmethod
    def _schedule_summary(job: Mapping[str, object]) -> str:
        kind = str(job.get("schedule_kind", "?"))
        spec = job.get("schedule_spec")
        if isinstance(spec, (str, bytes)):
            try:
                spec = json.loads(spec)
            except (json.JSONDecodeError, UnicodeDecodeError):
                spec = {}
        if not isinstance(spec, dict):
            spec = {}
        if kind == "at":
            return f"at {spec.get('at', '?')}"
        if kind == "every":
            return f"every {spec.get('interval_seconds', '?')}s"
        tz = spec.get("timezone") or "UTC"
        return f"cron {spec.get('expression', '?')} ({tz})"

    def _job_action(self, action: str, call: ToolCall) -> ToolResult:
        job_id = str(call.arguments.get("job_id", "")).strip()
        if not job_id:
            return ToolResult(content=f"cron {action}: job_id is required", ok=False)
        if action == "remove":
            method, path, body = "DELETE", f"/api/v1/agents/me/scheduled-jobs/{job_id}", None
            verb = "cancelled"
        else:
            method, path, body = "POST", f"/api/v1/agents/me/scheduled-jobs/{job_id}/{action}", {}
            verb = "paused" if action == "pause" else "resumed"
        try:
            data = self._request(method, path, body)
        except error.HTTPError as exc:
            return ToolResult(content=f"cron {action}: {self._http_detail(exc)}", ok=False)
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"cron {action}: {exc}", ok=False)
        state = ""
        if isinstance(data, dict) and "enabled" in data:
            state = " (enabled)" if data.get("enabled") else " (paused)"
        return ToolResult(content=f"scheduled job {job_id} {verb}{state}", ok=True)

    # -- control-plane round trips -------------------------------------------

    def _request(self, method: str, path: str, body: dict | None = None):
        url = self.context.control_plane_url.rstrip("/") + path
        payload = json.dumps(body).encode("utf-8") if body is not None else None
        req = request.Request(
            url,
            data=payload,
            method=method,
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 15))
        with request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def _get_jobs(self) -> tuple[list, str]:
        try:
            data = self._request("GET", "/api/v1/agents/me/scheduled-jobs")
        except error.HTTPError as exc:
            return [], f"jobs lookup failed: {self._http_detail(exc)}"
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return [], f"jobs lookup failed: {exc}"
        jobs = data.get("jobs") if isinstance(data, dict) else None
        if not isinstance(jobs, list):
            return [], "jobs lookup returned an unexpected shape"
        return [j for j in jobs if isinstance(j, dict)], ""

    @staticmethod
    def _http_detail(exc: error.HTTPError) -> str:
        detail = ""
        try:
            body = json.loads(exc.read().decode("utf-8"))
            err = body.get("error", "")
            if isinstance(err, dict):
                detail = str(err.get("message", ""))
            else:
                detail = str(err)
        except Exception:  # noqa: BLE001 — error body is best-effort
            pass
        return f"HTTP {exc.code}" + (f" — {detail}" if detail else "")


# ---------------------------------------------------------------------------
# S-282: session inspection builtins (sessions_list / sessions_history /
# session_status).
#
# Read-only mirrors of OpenClaw's sessions_list / sessions_history /
# session_status: an agent can inspect its own tasks, message history,
# task context, peer roster and current-run status beyond the intra-squad
# send_message. Every call is a GET against an EXISTING agent-scoped
# control-plane endpoint (/agents/me/*, or /agents/{id}/chat which the
# server already authorizes for this credential) — no new endpoints and
# no widened visibility: whatever the server returns is exactly what the
# caller was already allowed to see, and 403/404 surface as clean tool
# errors. Rendered output is sanitized: secret-looking keys/values are
# redacted and long bodies truncated so fetched content cannot flood the
# model context or leak credentials into transcripts.
# ---------------------------------------------------------------------------

SESSIONS_DEFAULT_LIMIT = 50
SESSION_BODY_MAX_CHARS = 800  # per-field truncation cap on rendered bodies
REDACTED = "[REDACTED]"

# Key names that mark a JSON field as secret-bearing at any nesting depth.
_SECRET_KEY_RE = re.compile(
    r"(?i)(token|secret|password|passwd|credential|api[_-]?key|authorization|access[_-]?key|private[_-]?key)"
)
# Value shapes that look like live credentials wherever they appear in text.
_SECRET_VALUE_RES = (
    re.compile(r"\bsk-[A-Za-z0-9_\-]{12,}"),
    re.compile(r"(?i)\bbearer\s+[a-z0-9._\-]{12,}"),
    re.compile(r"\b(?:kb|squ|ghp|gho|ghu|ghs|ghr|xox[baprs])_[A-Za-z0-9]{10,}"),
    re.compile(r"\bgithub_pat_[A-Za-z0-9_\-]{10,}"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
)

# Payload keys that carry tool-call data; hidden from the rendering unless
# include_tools=true so large tool inputs/outputs never bloat a history read.
_TOOL_PAYLOAD_KEYS = frozenset({"tool_calls", "tool_call", "tools", "function_call"})


def _redact_text(text: str) -> str:
    """Replace credential-shaped substrings with a redaction marker."""
    out = text
    for rx in _SECRET_VALUE_RES:
        out = rx.sub(REDACTED, out)
    return out


def _sanitize(value: object, max_chars: int = SESSION_BODY_MAX_CHARS) -> object:
    """Recursively redact secret-looking keys/values and truncate long strings.

    Dict keys matching the secret-name pattern have their values replaced
    wholesale (the key itself is kept so the reader knows something was
    there). Long strings are cut with an explicit truncation notice so
    the model can tell a body was shortened rather than complete.
    """
    if isinstance(value, dict):
        cleaned: dict = {}
        for key, inner in value.items():
            if _SECRET_KEY_RE.search(str(key)):
                cleaned[key] = REDACTED
            else:
                cleaned[key] = _sanitize(inner, max_chars)
        return cleaned
    if isinstance(value, list):
        return [_sanitize(inner, max_chars) for inner in value]
    if isinstance(value, str):
        red = _redact_text(value)
        if len(red) > max_chars:
            return red[:max_chars] + f"… [truncated, {len(red)} chars total]"
        return red
    return value


def _parse_limit(raw: object, default: int) -> int:
    """Parse a limit argument; non-positive/garbage falls back to default."""
    if raw is None:
        return default
    try:
        limit = int(raw)
    except (TypeError, ValueError):
        return default
    return limit if limit > 0 else default


def _payload_dict(message: Mapping[str, object]) -> dict:
    """Extract a message's payload as a dict (tolerates JSON-string payloads)."""
    payload = message.get("payload")
    if isinstance(payload, (str, bytes)):
        try:
            payload = json.loads(payload)
        except (json.JSONDecodeError, UnicodeDecodeError):
            payload = {}
    return payload if isinstance(payload, dict) else {}


class _SessionToolBase:
    """Shared GET-only control-plane access for the S-282 inspection tools.

    Deliberately has no POST/PUT/DELETE helper: these tools cannot mutate
    anything, and the test-suite asserts every request is a GET.
    """

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def _get(self, path: str):
        url = self.context.control_plane_url.rstrip("/") + path
        req = request.Request(
            url,
            method="GET",
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 15))
        with request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
        if not raw:
            return None
        return json.loads(raw.decode("utf-8"))

    @staticmethod
    def _http_detail(exc: error.HTTPError) -> str:
        detail = ""
        try:
            body = json.loads(exc.read().decode("utf-8"))
            err = body.get("error", "")
            if isinstance(err, dict):
                detail = str(err.get("message", ""))
            else:
                detail = str(err)
        except Exception:  # noqa: BLE001 — error body is best-effort
            pass
        return f"HTTP {exc.code}" + (f" — {detail}" if detail else "")


class SessionsListTool(_SessionToolBase):
    """S-282: list the calling agent's visible sessions (tasks / peers).

    kind=tasks (default) lists tasks assigned to this agent
    (GET /agents/me/tasks); kind=peers lists the visible squad roster
    (GET /agents/me/peers); kind=all renders both sections. The status
    filter narrows tasks by Kanban column (case-insensitive). Read-only:
    the server already scopes every row to this agent's credential.
    """

    name = "sessions_list"

    KINDS = ("tasks", "peers", "all")

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "sessions_list",
                    "description": (
                        "List the sessions you can see: tasks assigned "
                        "to you and/or your visible squad peers. "
                        "Read-only — shows exactly what your agent "
                        "credential can already access, never more. "
                        "kind=tasks (default) lists your tasks, "
                        "kind=peers lists squad-mates, kind=all shows "
                        "both. Optionally filter tasks by status "
                        "(backlog, todo, in-progress, in-review, done, "
                        "blocked) and cap rows with limit."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "kind": {
                                "type": "string",
                                "enum": ["tasks", "peers", "all"],
                                "description": "What to list. Default: tasks.",
                            },
                            "status": {
                                "type": "string",
                                "description": (
                                    "tasks only: keep only tasks in this "
                                    "column, e.g. 'in-progress'."
                                ),
                            },
                            "limit": {
                                "type": "integer",
                                "description": "Max rows per section (default 50).",
                            },
                        },
                        "required": [],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        kind = str(call.arguments.get("kind", "") or "tasks").strip().lower()
        if kind not in self.KINDS:
            return ToolResult(
                content=f"sessions_list: kind must be one of {', '.join(self.KINDS)}",
                ok=False,
            )
        status_filter = str(call.arguments.get("status", "") or "").strip().lower()
        limit = _parse_limit(call.arguments.get("limit"), SESSIONS_DEFAULT_LIMIT)

        sections: list[str] = []
        if kind in ("tasks", "all"):
            try:
                tasks = self._get(TASKS_PATH)
            except error.HTTPError as exc:
                return ToolResult(
                    content=f"sessions_list: tasks lookup failed: {self._http_detail(exc)}",
                    ok=False,
                )
            except (error.URLError, OSError, json.JSONDecodeError) as exc:
                return ToolResult(
                    content=f"sessions_list: tasks lookup failed: {exc}", ok=False
                )
            if not isinstance(tasks, list):
                return ToolResult(
                    content="sessions_list: tasks lookup returned an unexpected shape",
                    ok=False,
                )
            rows: list[str] = []
            skipped = 0
            for task in tasks:
                if not isinstance(task, dict):
                    continue
                status = str(task.get("status", "?"))
                if status_filter and status.lower() != status_filter:
                    skipped += 1
                    continue
                if len(rows) >= limit:
                    skipped += 1
                    continue
                number = task.get("task_number")
                label = f"T-{number}" if number else str(task.get("id", "?"))[:8]
                title = str(task.get("title", "")).strip() or "(untitled)"
                updated = str(task.get("updated_at", "")) or "—"
                rows.append(
                    f"{label} [{status}] {_redact_text(title)} "
                    f"(id={task.get('id', '?')}, updated={updated})"
                )
            header = f"TASKS — {len(rows)} shown"
            if skipped:
                header += f" ({skipped} filtered out / over limit)"
            sections.append(header + "\n" + ("\n".join(rows) if rows else EMPTY_DISPLAY))

        if kind in ("peers", "all"):
            try:
                peers = self._get(PEERS_PATH)
            except error.HTTPError as exc:
                return ToolResult(
                    content=f"sessions_list: peers lookup failed: {self._http_detail(exc)}",
                    ok=False,
                )
            except (error.URLError, OSError, json.JSONDecodeError) as exc:
                return ToolResult(
                    content=f"sessions_list: peers lookup failed: {exc}", ok=False
                )
            if not isinstance(peers, list):
                return ToolResult(
                    content="sessions_list: peers lookup returned an unexpected shape",
                    ok=False,
                )
            rows = []
            for peer in peers[:limit]:
                if not isinstance(peer, dict):
                    continue
                rows.append(
                    f"{peer.get('name', '?')} [{peer.get('status', '?')}] "
                    f"role={peer.get('role', '?')} (id={peer.get('id', '?')})"
                )
            sections.append(
                f"PEERS — {len(rows)} shown\n" + ("\n".join(rows) if rows else EMPTY_DISPLAY)
            )

        return ToolResult(content="\n\n".join(sections), ok=True)


class SessionsHistoryTool(_SessionToolBase):
    """S-282: sanitized history for a task, an agent chat, or your own history.

    task_id -> GET /agents/me/tasks/{id}/context (server 403s tasks not
    assigned to you — surfaced as a clean error). agent_id ->
    GET /agents/{id}/chat (server-side access control decides what this
    credential may read; we never bypass it). Neither -> the caller's own
    message history via GET /agents/me/messages/history. Tool-call
    payloads are omitted unless include_tools=true; everything rendered
    is sanitized (secrets redacted, long bodies truncated).
    """

    name = "sessions_history"

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "sessions_history",
                    "description": (
                        "Read sanitized history for inspection: pass "
                        "task_id for one of your tasks' context, "
                        "agent_id for an agent's chat transcript, or "
                        "neither for your own full message history. "
                        "Secret-looking values are redacted and long "
                        "bodies truncated. Tool-call payloads are "
                        "hidden unless include_tools=true. Read-only."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID (must be assigned to you).",
                            },
                            "agent_id": {
                                "type": "string",
                                "description": "Agent UUID whose chat to read.",
                            },
                            "include_tools": {
                                "type": "boolean",
                                "description": (
                                    "Include tool-call payloads in the "
                                    "history (default false)."
                                ),
                            },
                            "limit": {
                                "type": "integer",
                                "description": "Max entries to render (default 50).",
                            },
                        },
                        "required": [],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "") or "").strip()
        agent_id = str(call.arguments.get("agent_id", "") or "").strip()
        if task_id and agent_id:
            return ToolResult(
                content="sessions_history: pass at most one of task_id or agent_id",
                ok=False,
            )
        include_tools = bool(call.arguments.get("include_tools", False))
        limit = _parse_limit(call.arguments.get("limit"), SESSIONS_DEFAULT_LIMIT)

        try:
            if task_id:
                data = self._get(
                    f"/api/v1/agents/me/tasks/{quote(task_id, safe='')}/context"
                )
                return self._render_task_context(data)
            if agent_id:
                data = self._get(f"/api/v1/agents/{quote(agent_id, safe='')}/chat")
                return self._render_messages(
                    data, include_tools, limit, header=f"CHAT HISTORY — agent {agent_id}"
                )
            data = self._get("/api/v1/agents/me/messages/history")
            return self._render_messages(
                data, include_tools, limit, header="YOUR MESSAGE HISTORY"
            )
        except error.HTTPError as exc:
            return ToolResult(
                content=f"sessions_history: {self._http_detail(exc)}", ok=False
            )
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return ToolResult(content=f"sessions_history: {exc}", ok=False)

    def _render_task_context(self, data: object) -> ToolResult:
        if not isinstance(data, dict):
            return ToolResult(
                content="sessions_history: task context returned an unexpected shape",
                ok=False,
            )
        task = data.get("task") if isinstance(data.get("task"), dict) else {}
        lines = [
            "TASK CONTEXT",
            f"id: {task.get('id', '?')}  ref: T-{task.get('task_number', '?')}",
            f"title: {_sanitize(task.get('title', ''))}",
            f"status: {task.get('status', '?')}  updated: {task.get('updated_at', '—')}",
        ]
        description = str(task.get("description", "") or "").strip()
        if description:
            lines.append("description:\n" + _indent(str(_sanitize(description))))
        result = str(task.get("result", "") or "").strip()
        if result:
            lines.append("result:\n" + _indent(str(_sanitize(result))))
        for key, label in (("workspace_branch", "workspace branch"), ("workspace_commit_sha", "commit")):
            value = str(task.get(key, "") or "").strip()
            if value:
                lines.append(f"{label}: {value}")
        return ToolResult(content="\n".join(lines), ok=True)

    def _render_messages(
        self, data: object, include_tools: bool, limit: int, header: str
    ) -> ToolResult:
        if not isinstance(data, list):
            return ToolResult(
                content="sessions_history: history returned an unexpected shape", ok=False
            )
        lines: list[str] = []
        shown = 0
        for message in data:
            if not isinstance(message, dict):
                continue
            if shown >= limit:
                break
            payload = _payload_dict(message)
            had_tools = any(key in payload for key in _TOOL_PAYLOAD_KEYS)
            if not include_tools:
                payload = {k: v for k, v in payload.items() if k not in _TOOL_PAYLOAD_KEYS}
            sanitized = _sanitize(payload)
            text_main = sanitized.get("message") if isinstance(sanitized, dict) else None
            extras = {
                k: v
                for k, v in (sanitized.items() if isinstance(sanitized, dict) else [])
                if k != "message"
            }
            parts: list[str] = []
            if text_main:
                parts.append(str(text_main))
            if extras:
                parts.append("extras: " + json.dumps(extras, default=str))
            if not parts:
                parts.append(
                    "[tool payload hidden — pass include_tools=true]"
                    if had_tools and not include_tools
                    else "(empty)"
                )
            text = " | ".join(parts)
            sender = str(
                message.get("from_display")
                or message.get("from_id")
                or message.get("from_type")
                or "?"
            )
            created = str(message.get("created_at", "")) or "—"
            mtype = str(message.get("type", "?"))
            status = str(message.get("status", "?"))
            lines.append(f"[{created}] {sender} ({mtype}/{status}): {text}")
            shown += 1
        remaining = sum(1 for m in data if isinstance(m, dict)) - shown
        notice = f"\n[{remaining} more entries omitted (limit {limit})]" if remaining > 0 else ""
        return ToolResult(
            content=header + f" — {shown} shown\n" + ("\n".join(lines) if lines else EMPTY_DISPLAY) + notice,
            ok=True,
        )


def _indent(text: str) -> str:
    return "\n".join("  " + line for line in text.splitlines())


# ---------------------------------------------------------------------------
# S-283: built-in BROWSER tool (ADR-0014 Browser-as-a-Service).
#
# Thin client over the Tool Gateway's browser driver. Every action is an
# MCP tool call dispatched to the quarantined browser-service via
# POST {gateway}/v1/mcp/{resource_id}/call with the caller's own
# credential (Bearer + X-Skquad-Agent-ID) — the same governed path the
# synthetic mcp_call uses. The runtime NEVER talks to the browser
# service directly: session brokering, navigation ceilings
# (deny_hosts, max_pages), the per-agent session cap and the egress
# quarantine are enforced gateway/service-side. Without a granted
# browser resource or a configured gateway URL the tool fails closed
# before any network activity, and the gateway denies without a grant.
#
# Action → MCP mapping (verified against the LIVE deployed service,
# docs/tg6-browser-protocol.md §3):
#   open        -> browser.navigate {url}   (+ browser.extract summary)
#   snapshot    -> browser.extract          (visible page text; the
#                      service exposes no aria-tree snapshot — elements
#                      are addressed by CSS selector, not refs)
#   screenshot  -> browser.screenshot {full_page?}; the PNG is written
#                      into the workspace and only a REFERENCE is
#                      returned — base64 never enters model context
#   click       -> browser.click {selector}
#   type        -> browser.type {selector, text, submit?}
#   close       -> browser.close_session
#
# NOTE: the deployed TG-6 surface has no key-press tool, so `press` is
# deliberately absent (type with submit=true covers the common Enter
# case). Adding press requires a browser-service change, out of S-283.
# ---------------------------------------------------------------------------

BROWSER_UNTRUSTED_OPEN = "<skquad_untrusted"
BROWSER_UNTRUSTED_CLOSE = "</skquad_untrusted>"
BROWSER_SCREENSHOT_DIR = "browser/screenshots"
BROWSER_DEFAULT_MAX_SNAPSHOT_CHARS = 12000
BROWSER_DEFAULT_MAX_SCREENSHOT_BYTES = 5 * 1024 * 1024


def _unwrap_untrusted(text: str) -> str:
    """Strip the gateway's S-PROMPT WP3 wrapper to reach the payload.

    The gateway browser driver wraps every tool result as
    ``<skquad_untrusted ...>payload</skquad_untrusted>``; the browser
    payload itself is a JSON string. Non-wrapped text passes through.
    """
    s = text.strip()
    if s.startswith(BROWSER_UNTRUSTED_OPEN) and s.endswith(BROWSER_UNTRUSTED_CLOSE):
        gt = s.find(">")
        end = s.rfind(BROWSER_UNTRUSTED_CLOSE)
        if gt != -1 and end > gt:
            return s[gt + 1 : end]
    return s


def _truncate_text(text: str, max_chars: int) -> str:
    if len(text) <= max_chars:
        return text
    return text[:max_chars] + f"… [truncated, {len(text)} chars total]"


class BrowserTool:
    """S-283: governed headless browsing via the tool gateway's browser
    driver (ADR-0014). See the section comment above for the mapping."""

    name = "browser"

    ACTIONS = ("open", "snapshot", "screenshot", "click", "type", "close")

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "browser",
                    "description": (
                        "Drive the platform's quarantined headless "
                        "browser (JS-rendered pages, forms, "
                        "screenshots). Actions: open {url} navigates "
                        "and returns a page summary; snapshot returns "
                        "the visible page text; screenshot captures a "
                        "PNG saved into your workspace (a file "
                        "reference is returned, not the image data); "
                        "click {selector} and type {selector, text, "
                        "submit?} act on CSS selectors from the "
                        "snapshot; close releases the session. All "
                        "calls are routed and audited through the "
                        "tool gateway — you never see browser "
                        "credentials or session tokens, and browsing "
                        "is subject to the platform navigation "
                        "ceiling. Requires a granted browser resource."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "action": {
                                "type": "string",
                                "enum": list(self.ACTIONS),
                                "description": "Browser action to perform.",
                            },
                            "url": {
                                "type": "string",
                                "description": "http(s) URL (action=open).",
                            },
                            "selector": {
                                "type": "string",
                                "description": "CSS selector (action=click/type).",
                            },
                            "text": {
                                "type": "string",
                                "description": "Text to type (action=type).",
                            },
                            "submit": {
                                "type": "boolean",
                                "description": "Press Enter after typing (action=type).",
                            },
                            "full_page": {
                                "type": "boolean",
                                "description": "Full-page capture (action=screenshot).",
                            },
                        },
                        "required": ["action"],
                        "additionalProperties": False,
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        args = call.arguments if isinstance(call.arguments, Mapping) else {}
        action = str(args.get("action", "") or "").strip().lower()
        if action not in self.ACTIONS:
            return ToolResult(
                content=f"browser: unknown action {action!r} (expected one of: "
                + ", ".join(self.ACTIONS) + ")",
                ok=False,
            )
        # Fail-closed pre-flight: no grant / no gateway => no network.
        if not self.context.tool_gateway_url:
            return ToolResult(
                content="browser: tool gateway is not configured (fail-closed)",
                ok=False,
            )
        if not self.context.browser_resource_id:
            return ToolResult(
                content="browser: no browser resource granted to this agent (fail-closed)",
                ok=False,
            )
        try:
            if action == "open":
                return self._open(str(args.get("url", "") or "").strip())
            if action == "snapshot":
                return self._snapshot()
            if action == "screenshot":
                return self._screenshot(bool(args.get("full_page", False)))
            if action == "click":
                selector = str(args.get("selector", "") or "").strip()
                if not selector:
                    return ToolResult(content="browser click: selector is required", ok=False)
                return self._call("browser.click", {"selector": selector})
            if action == "type":
                selector = str(args.get("selector", "") or "").strip()
                if not selector:
                    return ToolResult(content="browser type: selector is required", ok=False)
                text = str(args.get("text", "") or "")
                payload: dict[str, object] = {"selector": selector, "text": text}
                if args.get("submit") is not None:
                    payload["submit"] = bool(args.get("submit"))
                return self._call("browser.type", payload)
            return self._call("browser.close_session", {})
        except error.HTTPError as exc:
            return ToolResult(content=f"browser {action}: {self._http_detail(exc)}", ok=False)
        except (error.URLError, OSError) as exc:
            return ToolResult(
                content=f"browser {action}: gateway request failed: {type(exc).__name__}",
                ok=False,
            )

    # -- action implementations -------------------------------------------

    def _open(self, url: str) -> ToolResult:
        if not url:
            return ToolResult(content="browser open: url is required", ok=False)
        scheme = url.split(":", 1)[0].lower() if ":" in url else ""
        if scheme not in ("http", "https"):
            return ToolResult(
                content=f"browser open: unsupported scheme {scheme!r} (http/https only)",
                ok=False,
            )
        nav = self._call("browser.navigate", {"url": url})
        if not nav.ok:
            return nav
        lines = [f"opened: {url}"]
        nav_info = _parse_browser_payload(nav.content)
        if isinstance(nav_info, dict):
            title = str(nav_info.get("title", "") or "").strip()
            if title:
                lines.append(f"title: {title}")
            if "page_index" in nav_info:
                lines.append(f"page_index: {nav_info['page_index']}")
        # Initial snapshot summary (best-effort — the page is open even
        # if the summary call fails).
        try:
            snap = self._call("browser.extract", {})
            if snap.ok:
                info = _parse_browser_payload(snap.content)
                text = str(info.get("text", "")) if isinstance(info, dict) else snap.content
                lines.append("")
                lines.append(_truncate_text(text, self._max_snapshot_chars()))
            else:
                lines.append("(initial snapshot failed: " + snap.content + ")")
        except Exception:  # noqa: BLE001 — summary is best-effort
            lines.append("(initial snapshot failed)")
        return ToolResult(content="\n".join(lines), ok=True)

    def _snapshot(self) -> ToolResult:
        result = self._call("browser.extract", {})
        if not result.ok:
            return result
        info = _parse_browser_payload(result.content)
        text = str(info.get("text", "")) if isinstance(info, dict) else result.content
        return ToolResult(content=_truncate_text(text, self._max_snapshot_chars()), ok=True)

    def _screenshot(self, full_page: bool) -> ToolResult:
        result = self._call("browser.screenshot", {"full_page": full_page})
        if not result.ok:
            return result
        info = _parse_browser_payload(result.content)
        if not isinstance(info, dict) or "screenshot_b64" not in info:
            return ToolResult(
                content="browser screenshot: unexpected service response shape", ok=False
            )
        try:
            blob = base64.b64decode(str(info["screenshot_b64"]).encode("ascii"))
        except Exception:  # noqa: BLE001 — malformed base64 from service
            return ToolResult(content="browser screenshot: malformed image data", ok=False)
        cap = int(self.policy.get("maxScreenshotBytes", BROWSER_DEFAULT_MAX_SCREENSHOT_BYTES))
        if len(blob) > cap:
            return ToolResult(
                content=f"browser screenshot: image is {len(blob)} bytes, over the "
                f"{cap}-byte policy cap (not saved)",
                ok=False,
            )
        stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
        name = f"screenshot-{stamp}-{os.urandom(4).hex()}.png"
        target_dir = Path(self.context.workspace_dir) / BROWSER_SCREENSHOT_DIR
        target_dir.mkdir(parents=True, exist_ok=True)
        target = target_dir / name
        target.write_bytes(blob)
        rel = f"{BROWSER_SCREENSHOT_DIR}/{name}"
        mime = str(info.get("mime", "image/png"))
        return ToolResult(
            content=f"screenshot saved: {rel} ({len(blob)} bytes, {mime}) — "
            "reference only; the image bytes are not inlined",
            ok=True,
        )

    # -- gateway round trip -------------------------------------------------

    def _call(self, tool: str, arguments: dict) -> ToolResult:
        """Dispatch one browser MCP tool call through the gateway.

        Mirrors the synthetic mcp_call contract: POST
        /v1/mcp/{resource_id}/call with {tool, arguments}; the gateway
        browser driver brokers the session, injects the session token,
        enforces ceilings and wraps the result as untrusted content.
        """
        url = (
            self.context.tool_gateway_url.rstrip("/")
            + "/v1/mcp/"
            + quote(self.context.browser_resource_id, safe="")
            + "/call"
        )
        payload = json.dumps({"tool": tool, "arguments": arguments}).encode("utf-8")
        req = request.Request(
            url,
            data=payload,
            method="POST",
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 30))
        with request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
        try:
            envelope = json.loads(raw.decode("utf-8"))
        except ValueError:
            return ToolResult(
                content=f"browser: malformed gateway response for {tool}", ok=False
            )
        if not isinstance(envelope, dict):
            return ToolResult(
                content=f"browser: malformed gateway response for {tool}", ok=False
            )
        content = str(envelope.get("content", "") or "")
        if envelope.get("isError"):
            return ToolResult(
                content=f"browser tool error ({tool}): {_truncate_text(content, 512)}",
                ok=False,
            )
        return ToolResult(content=content, ok=True)

    def _max_snapshot_chars(self) -> int:
        try:
            value = int(self.policy.get("maxSnapshotChars", BROWSER_DEFAULT_MAX_SNAPSHOT_CHARS))
        except (TypeError, ValueError):
            return BROWSER_DEFAULT_MAX_SNAPSHOT_CHARS
        return value if value > 0 else BROWSER_DEFAULT_MAX_SNAPSHOT_CHARS

    @staticmethod
    def _http_detail(exc: error.HTTPError) -> str:
        detail = ""
        try:
            body = json.loads(exc.read().decode("utf-8"))
            if isinstance(body, dict):
                detail = str(body.get("message") or body.get("error") or "")
        except Exception:  # noqa: BLE001 — error body is best-effort
            pass
        return f"HTTP {exc.code}" + (f" — {detail}" if detail else "")


def _parse_browser_payload(content: str) -> object:
    """Parse a browser-service tool payload (JSON inside the gateway's
    untrusted wrapper). Returns {} when the payload isn't JSON so
    callers can degrade to the raw text."""
    inner = _unwrap_untrusted(content)
    try:
        return json.loads(inner)
    except ValueError:
        return {}


class SessionStatusTool(_SessionToolBase):
    """S-282: a status card for the current run — always about THIS agent.

    Aggregates only what the existing endpoints expose: agent id (from
    the runtime context), bound model name (GET /agents/me/model),
    task counts + active tasks (GET /agents/me/tasks), and visible peer
    count (GET /agents/me/peers). Each section is best-effort: a field
    that isn't available is omitted gracefully, never fabricated. Takes
    no target argument by design — an agent can only ask about itself.
    """

    name = "session_status"

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        super().__init__(policy, context)
        # Tool instances are built at wake time, so this approximates the
        # runtime session's elapsed time for the current wake.
        self._started_at = time.time()

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "session_status",
                    "description": (
                        "A status card for your current session: your "
                        "agent id, bound model, task counts and active "
                        "tasks, visible peers, and elapsed time since "
                        "this wake. No arguments — always about you. "
                        "Unavailable fields are omitted, not guessed. "
                        "Read-only."
                    ),
                    "parameters": {"type": "object", "properties": {}, "required": []},
                },
            }
        ]

    def invoke(self, _call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        lines: list[str] = ["SESSION STATUS"]
        lines.append(f"agent_id: {self.context.agent_id or '(unknown)'}")
        elapsed = int(time.time() - self._started_at)
        lines.append(f"wake_elapsed_seconds: {elapsed}")

        # Model: best-effort — an unbound/unresolvable model returns an
        # empty projection; omit the line rather than show a blank.
        try:
            model = self._get("/api/v1/agents/me/model")
            if isinstance(model, dict) and model.get("model_name"):
                lines.append(f"model: {model['model_name']}")
        except Exception:  # noqa: BLE001 — status degrades, never fails on one field
            pass

        # Tasks: counts by status + the active (in-progress) ones.
        try:
            tasks = self._get(TASKS_PATH)
            if isinstance(tasks, list):
                counts: dict[str, int] = {}
                active: list[str] = []
                for task in tasks:
                    if not isinstance(task, dict):
                        continue
                    status = str(task.get("status", "?"))
                    counts[status] = counts.get(status, 0) + 1
                    if status == "in-progress":
                        number = task.get("task_number")
                        label = f"T-{number}" if number else str(task.get("id", "?"))[:8]
                        title = str(task.get("title", "")).strip() or "(untitled)"
                        active.append(f"{label} {_redact_text(title)}")
                counts_text = ", ".join(f"{k}={v}" for k, v in sorted(counts.items()))
                lines.append(f"tasks: {len(tasks)} total" + (f" ({counts_text})" if counts_text else ""))
                if active:
                    lines.append("active: " + "; ".join(active))
            else:
                lines.append("tasks: unavailable")
        except Exception:  # noqa: BLE001 — graceful omission per contract
            lines.append("tasks: unavailable")

        # Peers: count only, best-effort.
        try:
            peers = self._get(PEERS_PATH)
            if isinstance(peers, list):
                lines.append(f"visible_peers: {len(peers)}")
        except Exception:  # noqa: BLE001
            pass

        return ToolResult(content="\n".join(lines), ok=True)


# ---------------------------------------------------------------------------
# S-292 (ADR-0016 D1): board.* builtins — agent-driven board work.
#
# Thin clients over the agent-authenticated control-plane transition
# endpoints. Each verb maps 1:1 to a server-guarded transition; there is
# no free-form status write — the control plane stays the single
# enforcement point (column allowlists, leases, fencing, dependency
# guard S-278, budget guard S-203, self-approval policy + reopen cap
# S-290). CP error bodies ({"error":{"code","message"}}) are surfaced
# with their machine code so the model can branch (409/400/429).
#
# The claim fence: board.claim (and board.reopen, which mints a NEW
# attempt) return execution_id + fencing_token on the task payload. The
# fencing token is NEVER rendered into tool output — it is kept in this
# process-local registry keyed by task id and attached to the terminal
# verbs (complete/block) server-ward only.
# ---------------------------------------------------------------------------

_BOARD_FENCE_LOCK = threading.Lock()
_BOARD_FENCES: dict[str, dict[str, str]] = {}


def _remember_board_fence(task_id: str, execution_id: str, fencing_token: str) -> None:
    if not task_id or not execution_id or not fencing_token:
        return
    with _BOARD_FENCE_LOCK:
        _BOARD_FENCES[task_id] = {"execution_id": execution_id, "fencing_token": fencing_token}


def _board_fence(task_id: str) -> tuple[str, str] | None:
    with _BOARD_FENCE_LOCK:
        fence = _BOARD_FENCES.get(task_id)
    if not fence:
        return None
    return fence["execution_id"], fence["fencing_token"]


def _forget_board_fence(task_id: str) -> None:
    with _BOARD_FENCE_LOCK:
        _BOARD_FENCES.pop(task_id, None)


def _coerce_review_flag(value: object) -> bool | None:
    """Lenient bool coercion for review_needed; None when un-coercible."""
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in ("true", "1", "yes"):
            return True
        if lowered in ("false", "0", "no"):
            return False
    return None


class _BoardToolBase:
    """Shared control-plane round-trip plumbing for the board.* builtins."""

    def __init__(self, policy: dict, context: BuiltinToolContext) -> None:
        self.policy = policy or {}
        self.context = context

    def _call(self, method: str, path: str, body: dict | None) -> tuple[dict, str]:
        """One control-plane round trip with the agent's own credential.

        Returns ``(payload, error_message)`` — exactly one meaningful.
        CP error bodies are surfaced with their machine-readable code
        (e.g. ``409 reopen_cap_exceeded``) so failures are never
        swallowed into a generic transport error.
        """
        url = self.context.control_plane_url.rstrip("/") + path
        payload = json.dumps(body).encode("utf-8") if body is not None else None
        req = request.Request(
            url,
            data=payload,
            method=method,
            headers={
                "Authorization": f"Bearer {self.context.agent_credential}",
                "X-Skquad-Agent-ID": self.context.agent_id,
                "Content-Type": JSON_CONTENT_TYPE,
                "Accept": JSON_CONTENT_TYPE,
            },
        )
        timeout = int(self.policy.get("timeoutSeconds", 15))
        try:
            with request.urlopen(req, timeout=timeout) as response:
                raw = response.read()
        except error.HTTPError as exc:
            code, message = "", ""
            try:
                err_body = json.loads(exc.read().decode("utf-8"))
                err_obj = err_body.get("error")
                if isinstance(err_obj, dict):
                    code = str(err_obj.get("code", "") or "")
                    message = str(err_obj.get("message", "") or "")
            except Exception:  # noqa: BLE001 — error body is best-effort
                pass
            detail = f" {code}" if code else ""
            if message:
                detail += f": {message}"
            return {}, f"HTTP {exc.code}{detail}"
        except (error.URLError, OSError, json.JSONDecodeError) as exc:
            return {}, f"control-plane request failed: {exc}"
        if not raw:
            return {}, ""
        try:
            data = json.loads(raw.decode("utf-8"))
        except json.JSONDecodeError:
            return {}, "control-plane returned a non-JSON response"
        if not isinstance(data, dict):
            return {}, "control-plane returned an unexpected shape"
        return data, ""


class BoardCreateTaskTool(_BoardToolBase):
    """S-292: board.create_task — create a task on the agent's own squad."""

    name = TOOL_CREATE_TASK

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_CREATE_TASK,
                    "description": (
                        "Create a task on your own squad's board. It "
                        "lands in BACKLOG: not claimable and invisible "
                        "in agent chat until a human prioritises it. "
                        "ready=true creates it directly in TO DO "
                        "(claimable) and requires the can_prioritize "
                        "leader capability granted by your squad "
                        "owner — without it the call fails 403 "
                        "leader_capability_required. squad_id is "
                        "optional and must be your own squad."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "squad_id": {
                                "type": "string",
                                "description": (
                                    "Optional; must be your own squad. "
                                    "Defaults to your squad."
                                ),
                            },
                            "title": {
                                "type": "string",
                                "description": "Task title (required).",
                            },
                            "description": {
                                "type": "string",
                                "description": "Task description.",
                            },
                            "ready": {
                                "type": "boolean",
                                "description": (
                                    "Create directly in TO DO "
                                    "(claimable). Leader capability "
                                    "required; omit unless you know you "
                                    "hold it."
                                ),
                            },
                        },
                        "required": ["title"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        title = str(call.arguments.get("title", "")).strip()
        if not title:
            return ToolResult(content="board.create_task: title is required", ok=False)
        body: dict[str, object] = {
            "title": title,
            "description": str(call.arguments.get("description", "") or ""),
        }
        squad_id = str(call.arguments.get("squad_id", "") or "").strip()
        if squad_id:
            body["squad_id"] = squad_id
        if bool(call.arguments.get("ready", False)):
            body["ready"] = True
        data, err = self._call("POST", TASKS_PATH, body)
        if err:
            return ToolResult(content=f"board.create_task: {err}", ok=False)
        task_id = str(data.get("id", ""))
        status = str(data.get("status", "backlog"))
        return ToolResult(
            content=f"created task {task_id} in {status.upper()}", ok=True
        )


class BoardAssignTool(_BoardToolBase):
    """S-292: board.assign — set a task's assignee (own squad only)."""

    name = TOOL_ASSIGN

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_ASSIGN,
                    "description": (
                        "Set a task's assignee. The task must live in "
                        "your squad and the assignee must be a squad "
                        "member. Assigning to ANOTHER agent requires "
                        "the can_assign_others leader capability "
                        "granted by your squad owner — without it the "
                        "call fails 403 leader_capability_required. "
                        "Self-assign always works. A newly assigned "
                        "agent in a claimable column is woken "
                        "automatically."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to assign.",
                            },
                            "assignee_agent_id": {
                                "type": "string",
                                "description": "Agent UUID of the new assignee.",
                            },
                        },
                        "required": ["task_id", "assignee_agent_id"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        assignee = str(call.arguments.get("assignee_agent_id", "")).strip()
        if not task_id:
            return ToolResult(content="board.assign: task_id is required", ok=False)
        if not assignee:
            return ToolResult(
                content="board.assign: assignee_agent_id is required", ok=False
            )
        _, err = self._call(
            "PATCH",
            f"/api/v1/agents/me/tasks/{task_id}/assignee",
            {"assignee_agent_id": assignee},
        )
        if err:
            return ToolResult(content=f"board.assign: {err}", ok=False)
        return ToolResult(
            content=f"task {task_id} assigned to agent {assignee}", ok=True
        )


class BoardClaimTool(_BoardToolBase):
    """S-292: board.claim — TO DO -> IN PROGRESS with a fenced execution.

    The fencing token returned by the start endpoint is stored in the
    process-local fence registry for the terminal verbs and never
    rendered into the tool result.
    """

    name = TOOL_CLAIM

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_CLAIM,
                    "description": (
                        "Claim a task and start working it: TO DO -> "
                        "IN PROGRESS (or resume your own stalled IN "
                        "PROGRESS task). Creates a lease-backed "
                        "execution; the platform holds the lease and "
                        "fencing token for you. Backlog tasks are "
                        "rejected (409 task_in_backlog — a human must "
                        "prioritise them); tasks blocked by unfinished "
                        "dependencies return 409 with the blocking "
                        "list. You must be the assignee."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to claim.",
                            },
                        },
                        "required": ["task_id"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        if not task_id:
            return ToolResult(content="board.claim: task_id is required", ok=False)
        data, err = self._call(
            "POST", f"/api/v1/agents/me/tasks/{task_id}/start", {}
        )
        if err:
            return ToolResult(content=f"board.claim: {err}", ok=False)
        execution_id = str(data.get("execution_id", ""))
        fencing_token = str(data.get("fencing_token", ""))
        if not execution_id or not fencing_token:
            return ToolResult(
                content=(
                    f"board.claim: claim not established for task {task_id} "
                    "(another worker holds the live execution)"
                ),
                ok=False,
            )
        _remember_board_fence(task_id, execution_id, fencing_token)
        lease = str(data.get("lease_expires_at", ""))
        return ToolResult(
            content=(
                f"claimed task {task_id} (execution {execution_id}); "
                f"lease active{f' until {lease}' if lease else ''}. "
                "confidential_note: fencing token retained by the "
                "platform; never request or print it."
            ),
            ok=True,
        )


class BoardCompleteTool(_BoardToolBase):
    """S-292: board.complete — IN PROGRESS -> DONE | IN REVIEW (D2)."""

    name = TOOL_COMPLETE

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_COMPLETE,
                    "description": (
                        "Complete your in-progress task. Legal source "
                        "column: IN PROGRESS only. review_needed=true "
                        "(the DEFAULT) moves it to IN REVIEW for a "
                        "human. review_needed=false moves it straight "
                        "to DONE and REQUIRES a justification stating "
                        "why no human review is warranted — use false "
                        "ONLY for straightforward, self-evident, "
                        "fully-verified tasks. Anything involving "
                        "judgment, irreversible side effects, or "
                        "partial confidence must go to in-review. If "
                        "self-approval is disabled for you, false "
                        "returns 409 self_approval_disabled — retry "
                        "with review_needed=true."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to complete.",
                            },
                            "review_needed": {
                                "type": "boolean",
                                "description": (
                                    "Default true (human review). "
                                    "false = self-approve straight to "
                                    "DONE; requires justification and "
                                    "is for straightforward, self-"
                                    "evident, fully-verified work only."
                                ),
                            },
                            "summary": {
                                "type": "string",
                                "description": (
                                    "What you did and the outcome. "
                                    "Required; shown on the card."
                                ),
                            },
                            "justification": {
                                "type": "string",
                                "description": (
                                    "Required with review_needed=false: "
                                    "one line on why no human review is "
                                    "warranted. Recorded in the audit "
                                    "trail."
                                ),
                            },
                        },
                        "required": ["task_id", "summary"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        summary = str(call.arguments.get("summary", "")).strip()
        if not task_id:
            return ToolResult(content="board.complete: task_id is required", ok=False)
        if not summary:
            return ToolResult(content="board.complete: summary is required", ok=False)
        raw_review = call.arguments.get("review_needed")
        review_needed = True if raw_review is None else _coerce_review_flag(raw_review)
        if review_needed is None:
            return ToolResult(
                content="board.complete: review_needed must be a boolean", ok=False
            )
        justification = str(call.arguments.get("justification", "") or "").strip()
        if not review_needed and not justification:
            return ToolResult(
                content=(
                    "board.complete: review_needed=false requires a "
                    "justification (why no human review is warranted); "
                    "otherwise use review_needed=true"
                ),
                ok=False,
            )
        fence = _board_fence(task_id)
        if fence is None:
            return ToolResult(
                content=(
                    f"board.complete: no active claim held by this "
                    f"session for task {task_id} — claim it first with {TOOL_CLAIM}"
                ),
                ok=False,
            )
        body: dict[str, object] = {
            "summary": summary,
            "review_needed": review_needed,
            "execution_id": fence[0],
            "fencing_token": fence[1],
        }
        if justification:
            body["justification"] = justification
        data, err = self._call(
            "POST", f"/api/v1/agents/me/tasks/{task_id}/complete", body
        )
        if err:
            return ToolResult(content=f"board.complete: {err}", ok=False)
        _forget_board_fence(task_id)
        status = str(data.get("status", "in-review"))
        return ToolResult(content=f"completed task {task_id} -> {status.upper()}", ok=True)


class BoardBlockTool(_BoardToolBase):
    """S-292: board.block — IN PROGRESS -> BLOCKED with a reason."""

    name = TOOL_BLOCK

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_BLOCK,
                    "description": (
                        "Block your in-progress task (legal source "
                        "column: IN PROGRESS only) with a reason. The "
                        "card moves to BLOCKED, the squad owner is "
                        "notified, and the task waits on a human."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to block.",
                            },
                            "reason": {
                                "type": "string",
                                "description": (
                                    "Why the task is blocked and what "
                                    "it is waiting on. Required."
                                ),
                            },
                        },
                        "required": ["task_id", "reason"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        reason = str(call.arguments.get("reason", "")).strip()
        if not task_id:
            return ToolResult(content="board.block: task_id is required", ok=False)
        if not reason:
            return ToolResult(content="board.block: reason is required", ok=False)
        fence = _board_fence(task_id)
        if fence is None:
            return ToolResult(
                content=(
                    f"board.block: no active claim held by this "
                    f"session for task {task_id} — claim it first with {TOOL_CLAIM}"
                ),
                ok=False,
            )
        _, err = self._call(
            "POST",
            f"/api/v1/agents/me/tasks/{task_id}/block",
            {
                "summary": reason,
                "execution_id": fence[0],
                "fencing_token": fence[1],
            },
        )
        if err:
            return ToolResult(content=f"board.block: {err}", ok=False)
        _forget_board_fence(task_id)
        return ToolResult(content=f"blocked task {task_id}: {reason}", ok=True)


class BoardReopenTool(_BoardToolBase):
    """S-292: board.reopen — IN REVIEW -> IN PROGRESS, new attempt (D3)."""

    name = TOOL_REOPEN

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_REOPEN,
                    "description": (
                        "Reopen YOUR task that sits in IN REVIEW back "
                        "to IN PROGRESS (legal source column: IN "
                        "REVIEW only; you must be the assignee). "
                        "Requires a non-empty reason; pass the "
                        "reviewer's feedback verbatim in "
                        "reviewer_feedback when relaying it. Creates a "
                        "NEW execution attempt (the platform keeps the "
                        "fence for the new attempt automatically). "
                        "DONE is final for agents (409). Subject to "
                        "the squad reopen cap: beyond it you get 429 "
                        "reopen_cap_exceeded and the card needs human "
                        "intervention — do not retry."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to reopen.",
                            },
                            "reason": {
                                "type": "string",
                                "description": "Why the task is reopened. Required.",
                            },
                            "reviewer_feedback": {
                                "type": "string",
                                "description": (
                                    "The reviewer's message, verbatim, "
                                    "carried into the new attempt's "
                                    "task brief."
                                ),
                            },
                        },
                        "required": ["task_id", "reason"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        reason = str(call.arguments.get("reason", "")).strip()
        if not task_id:
            return ToolResult(content="board.reopen: task_id is required", ok=False)
        if not reason:
            return ToolResult(content="board.reopen: reason is required", ok=False)
        body: dict[str, object] = {"reason": reason}
        feedback = str(call.arguments.get("reviewer_feedback", "") or "").strip()
        if feedback:
            body["reviewer_feedback"] = feedback
        data, err = self._call(
            "POST", f"/api/v1/agents/me/tasks/{task_id}/reopen", body
        )
        if err:
            return ToolResult(content=f"board.reopen: {err}", ok=False)
        execution_id = str(data.get("execution_id", ""))
        fencing_token = str(data.get("fencing_token", ""))
        if execution_id and fencing_token:
            _remember_board_fence(task_id, execution_id, fencing_token)
        return ToolResult(
            content=(
                f"reopened task {task_id} -> IN PROGRESS (new attempt "
                f"{execution_id})" if execution_id
                else f"reopened task {task_id} -> IN PROGRESS"
            ),
            ok=True,
        )


class BoardRequestChangesTool(_BoardToolBase):
    """S-292: board.request_changes — IN REVIEW -> TO DO, unassigned."""

    name = TOOL_REQUEST_CHANGES

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_REQUEST_CHANGES,
                    "description": (
                        "Return YOUR reviewed card to the pool: legal "
                        "source column IN REVIEW only, you must be the "
                        "assignee. The task moves to TO DO with the "
                        "assignee cleared so it can be reassigned. "
                        "Requires a comment explaining what needs to "
                        "change (recorded in the audit trail and the "
                        "task thread). Wrong column -> 409."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to return.",
                            },
                            "comment": {
                                "type": "string",
                                "description": (
                                    "What needs to change before this "
                                    "task can be redone. Required."
                                ),
                            },
                        },
                        "required": ["task_id", "comment"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        comment = str(call.arguments.get("comment", "")).strip()
        if not task_id:
            return ToolResult(
                content="board.request_changes: task_id is required", ok=False
            )
        if not comment:
            return ToolResult(
                content="board.request_changes: comment is required", ok=False
            )
        _, err = self._call(
            "POST",
            f"/api/v1/agents/me/tasks/{task_id}/request-changes",
            {"comment": comment},
        )
        if err:
            return ToolResult(content=f"board.request_changes: {err}", ok=False)
        _forget_board_fence(task_id)
        return ToolResult(
            content=f"returned task {task_id} to TO DO for reassignment", ok=True
        )


class BoardApproveTool(_BoardToolBase):
    """S-294: board.approve — leader-approve ANOTHER agent's in-review
    task straight to DONE. Requires a leader capability grant on the
    squad (ADR-0016 D5) AND the squad's leader_approval_allowed knob
    (D8, default off — approval strictly human). Self-approval is NOT
    reachable here; your own card self-approves at board.complete time."""

    name = TOOL_APPROVE

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": TOOL_APPROVE,
                    "description": (
                        "Leader approval: move ANOTHER agent's "
                        "in-review task to DONE. Only for agents the "
                        "squad owner granted a leader capability "
                        "(can_prioritize / can_assign_others) AND only "
                        "while the squad's leader_approval_allowed "
                        "policy is on — otherwise 403 "
                        "leader_capability_required or "
                        "leader_approval_disabled. You cannot approve "
                        "your own task (403) — self-approval happens "
                        "at board.complete time. Optional comment is "
                        "recorded in the audit trail and the task "
                        "thread. Wrong column -> 409."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "task_id": {
                                "type": "string",
                                "description": "Task UUID to approve.",
                            },
                            "comment": {
                                "type": "string",
                                "description": (
                                    "Optional approval note (audit "
                                    "trail + task thread)."
                                ),
                            },
                        },
                        "required": ["task_id"],
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, _config) -> ToolResult:  # noqa: ANN001
        task_id = str(call.arguments.get("task_id", "")).strip()
        if not task_id:
            return ToolResult(
                content="board.approve: task_id is required", ok=False
            )
        comment = str(call.arguments.get("comment", "") or "").strip()
        body: dict[str, object] = {}
        if comment:
            body["comment"] = comment
        data, err = self._call(
            "POST",
            f"/api/v1/agents/me/tasks/{task_id}/approve",
            body,
        )
        if err:
            return ToolResult(content=f"board.approve: {err}", ok=False)
        status = str(data.get("status", "done"))
        return ToolResult(
            content=f"leader-approved task {task_id} -> {status.upper()}", ok=True
        )


_BUILTIN_REGISTRY: dict[str, type] = {
    "exec": ExecTool,
    "web_fetch": WebFetchTool,
    "web_search": WebSearchTool,
    "send_message": SendMessageTool,
    "send_inbox": SendInboxTool,
    "notify_owner": NotifyOwnerTool,
    "memory_search": MemorySearchTool,
    # S-279: file tools, jailed to context.workspace_dir via
    # _resolve_in_workspace (the security boundary).
    "read": ReadTool,
    "write": WriteTool,
    "edit": EditTool,
    "apply_patch": ApplyPatchTool,
    # S-280: background process management (paired with exec background).
    "process": ProcessTool,
    # S-281: built-in CRON — thin client over the control-plane
    # scheduled-jobs API (scheduler fires the wakes, not the runtime).
    "cron": CronTool,
    # S-282: session inspection — GET-only clients over the existing
    # agent-scoped read endpoints (tasks, peers, message history, task
    # context, agent chat, model). No new endpoints, no widened access.
    "sessions_list": SessionsListTool,
    "sessions_history": SessionsHistoryTool,
    "session_status": SessionStatusTool,
    # S-283: built-in BROWSER — gateway-driven client over the
    # quarantined browser-service (ADR-0014). Fail-closed without a
    # granted browser resource or a configured tool gateway.
    "browser": BrowserTool,
    # S-292 (ADR-0016 D1): board.* — agent-driven board work. Each
    # verb is a thin client over a server-guarded transition endpoint.
    TOOL_CREATE_TASK: BoardCreateTaskTool,
    TOOL_ASSIGN: BoardAssignTool,
    TOOL_CLAIM: BoardClaimTool,
    TOOL_COMPLETE: BoardCompleteTool,
    TOOL_BLOCK: BoardBlockTool,
    TOOL_REOPEN: BoardReopenTool,
    TOOL_REQUEST_CHANGES: BoardRequestChangesTool,
    # S-294 (ADR-0016 D5/D8): leader approval of others' in-review
    # tasks — server-side default-deny (grant + leader_approval_allowed).
    TOOL_APPROVE: BoardApproveTool,
}

# Built-ins that are listed in the platform catalog (S-232) for admin
# visibility but instantiated elsewhere — spawn_subagent is wired by
# skquad_runtime.subagents, so build_builtin_tools must skip it silently
# instead of warning "unknown tool name" on every wake.
_BUILTIN_WIRED_ELSEWHERE: frozenset[str] = frozenset({"spawn_subagent"})


def compose_builtin_and_plugins(builtins: list, plugins: list) -> list:
    """Merge built-in tools with loaded plugins: built-ins win name collisions.

    A loaded plugin that shares a name with an enabled built-in tool is
    dropped (with an ERROR log naming the collision) so the platform
    built-in — whose policy is centrally controlled — is the one the tool
    loop dispatches to. Built-ins are prepended so they lead the schema
    list.
    """
    builtin_names = {tool.name for tool in builtins}
    kept: list = []
    for plugin in plugins:
        if plugin.name in builtin_names:
            logger.error(
                "builtin_tools: name collision on %r — enabled built-in wins; "
                "loaded plugin %r is dropped from the tool list",
                plugin.name,
                type(plugin).__name__,
            )
            continue
        kept.append(plugin)
    return list(builtins) + kept


def build_builtin_tools(fetched_config: dict, context: BuiltinToolContext) -> list:
    """Instantiate enabled built-in tools from fetched control-plane config."""
    tools: list = []
    for entry in (fetched_config or {}).get("tools", []):
        name = str(entry.get("name", ""))
        if not entry.get("enabled"):
            continue
        if name in _BUILTIN_WIRED_ELSEWHERE:
            # S-232: listed in the platform catalog for admin visibility,
            # but provided by the subagents module — not this registry.
            continue
        tool_cls = _BUILTIN_REGISTRY.get(name)
        if tool_cls is None:
            logger.warning("builtin_tools: unknown tool name %r — skipped", name)
            continue
        tools.append(tool_cls(entry.get("policy", {}) or {}, context))
    return tools
