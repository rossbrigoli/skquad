"""Built-in platform tools per ADR-0012: exec, web_fetch, web_search.

Implements the RuntimePlugin protocol (``name``, ``tools()``,
``invoke(call, config)``) so the chat (S-122) and task tool loops work
unchanged. Stdlib only. Tool results are returned raw here; the runtime
wraps them as untrusted data (S-PROMPT WP3 ``wrap_untrusted``) at the
handler layer.
"""

from __future__ import annotations

import html.parser
import ipaddress
import json
import logging
import os
import re
import socket
import subprocess
from dataclasses import dataclass
from typing import Mapping
from urllib import error, request

from .runtime import ToolCall, ToolResult

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

    def invoke(self, call: ToolCall, config) -> ToolResult:  # noqa: ANN001
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


class _NoRedirectHandler(request.HTTPRedirectHandler):
    """Turn redirects into a catchable 3xx instead of following them silently."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        return None


class WebFetchTool:
    """Fetch a URL with an SSRF guard and stdlib html->text extraction."""

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

    def _assert_public_host(self, host: str) -> None:
        """Reject hosts resolving to anything non-public unless explicitly allowed."""
        if self.policy.get("allowPrivateNetwork") is True:
            return
        try:
            infos = socket.getaddrinfo(host, None)
        except socket.gaierror as exc:
            raise ValueError(f"web_fetch: cannot resolve host {host!r}: {exc}") from exc
        for info in infos:
            ip = ipaddress.ip_address(info[4][0])
            if (
                ip.is_loopback
                or ip.is_private
                or ip.is_link_local
                or ip.is_multicast
                or ip.is_reserved
                or ip.is_unspecified
                or (ip.version == 4 and ip in ipaddress.ip_network("100.64.0.0/10"))
                or (ip.version == 6 and ip in ipaddress.ip_network("fc00::/7"))
            ):
                raise ValueError(
                    f"web_fetch: blocked by SSRF guard — {host!r} resolves to "
                    f"non-public address {ip}"
                )

    def invoke(self, call: ToolCall, config) -> ToolResult:  # noqa: ANN001
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
        max_bytes = int(self.policy.get("maxBytes", 262144))
        opener = request.build_opener(_NoRedirectHandler())
        max_hops = 3

        for _hop in range(max_hops + 1):
            try:
                parsed_host = url.split("://", 1)[1].split("/", 1)[0]
                host_only = parsed_host.split("@")[-1].split(":")[0]
                self._assert_public_host(host_only)
            except ValueError as exc:
                return ToolResult(content=str(exc), ok=False)

            req = request.Request(url, method="GET", headers={"Accept": "*/*"})
            try:
                with opener.open(req, timeout=timeout) as response:
                    body = response.read(max_bytes + 1)
                    content_type = response.headers.get("Content-Type", "")
                    final_status = response.status
            except error.HTTPError as exc:
                if exc.code in (301, 302, 303, 307, 308):
                    location = exc.headers.get("Location")
                    if not location:
                        return ToolResult(
                            content=f"web_fetch: redirect ({exc.code}) without Location",
                            ok=False,
                        )
                    url = request.urljoin(url, location)
                    continue
                return ToolResult(
                    content=f"web_fetch: HTTP {exc.code} for {url}", ok=False
                )
            except (error.URLError, OSError) as exc:
                return ToolResult(content=f"web_fetch: fetch failed: {exc}", ok=False)

            if final_status >= 400:
                return ToolResult(
                    content=f"web_fetch: HTTP {final_status} for {url}", ok=False
                )
            break
        else:
            return ToolResult(content="web_fetch: too many redirects (max 3)", ok=False)

        truncated = len(body) > max_bytes
        if truncated:
            body = body[:max_bytes]
        text = body.decode("utf-8", errors="replace")
        if "html" in content_type.lower():
            extractor = _TextExtractor()
            try:
                extractor.feed(text)
                extractor.close()
                text = extractor.text()
            except Exception:  # noqa: BLE001 — malformed html: keep raw text
                pass
        if truncated:
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

    def invoke(self, call: ToolCall, config) -> ToolResult:  # noqa: ANN001
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
                "Content-Type": "application/json",
                "Accept": "application/json",
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


_BUILTIN_REGISTRY: dict[str, type] = {
    "exec": ExecTool,
    "web_fetch": WebFetchTool,
    "web_search": WebSearchTool,
}


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
        tool_cls = _BUILTIN_REGISTRY.get(name)
        if tool_cls is None:
            logger.warning("builtin_tools: unknown tool name %r — skipped", name)
            continue
        tools.append(tool_cls(entry.get("policy", {}) or {}, context))
    return tools
