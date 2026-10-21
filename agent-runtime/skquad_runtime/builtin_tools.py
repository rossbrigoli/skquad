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
import subprocess
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


@dataclass
class BuiltinToolContext:
    """Static context a built-in tool needs to run inside an agent pod."""

    workspace_dir: str
    control_plane_url: str
    agent_credential: str
    agent_id: str = ""


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
                            }
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
            names = ", ".join(str(p.get("name", "?")) for p in peers) or "(none)"
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
            data = self._request("GET", "/api/v1/agents/me/peers")
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
                                "description": "File path relative to the workspace.",
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
                                "description": "File path relative to the workspace.",
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
                                "description": "File path relative to the workspace.",
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
            if line.startswith("*** Add File: "):
                path = line[len("*** Add File: "):].strip()
                i += 1
                content: list[str] = []
                while i < len(body) and not body[i].startswith("*** "):
                    if not body[i].startswith("+"):
                        raise ValueError(f"Add File {path!r}: expected '+' lines, got {body[i]!r}")
                    content.append(body[i][1:])
                    i += 1
                adds.append((path, "\n".join(content) + ("\n" if content else "")))
            elif line.startswith("*** Update File: "):
                path = line[len("*** Update File: "):].strip()
                i += 1
                hunks: list[list[str]] = []
                current: list[str] | None = None
                while i < len(body) and not body[i].startswith("*** Add File: ") and not body[i].startswith("*** Update File: ") and not body[i].startswith("*** Delete File: "):
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
            elif line.startswith("*** Delete File: "):
                path = line[len("*** Delete File: "):].strip()
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
