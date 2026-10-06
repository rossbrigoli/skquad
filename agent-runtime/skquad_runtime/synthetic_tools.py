"""TG-4d (S-264): synthetic gateway-backed tools for the runtime.

Synthetic tools are NOT loaded plugins (``SKQUAD_PLUGIN_MODULES``) and
NOT platform builtins (``builtin_tools``): they are constructed at wake
time from the fetch-at-wake resource surface (the ``tools`` array the
control plane publishes on granted resources, TG-4) and routed through
the tool gateway. They implement the same ``RuntimePlugin`` protocol
(``name`` / ``tools()`` / ``invoke(call, config)``) so the chat and task
tool loops dispatch to them unchanged via ``invoke_plugin_tool``.

The registry is generic on purpose: future drivers (``mcp_call``,
``browser``, ``ssh``) add a builder to ``SYNTHETIC_TOOL_BUILDERS`` and
join both turn paths the same way. The composition layer in
``runtime.py`` must not make rest-specific assumptions.

Fail-closed contract: a synthetic tool appears ONLY when the agent holds
a qualifying grant AND the runtime is configured for it
(``SKQUAD_TOOL_GATEWAY_URL``). No grant or no gateway URL => the tool is
absent from the schema list entirely, so the model can never request it.

Security: the agent's own credential is read at invoke time from the
credential mount (same ``read_secret_value`` pattern as
``ControlPlaneClient.from_bootstrap``) and sent to the gateway as the
Bearer token. It is never logged; error strings are built from status
codes and gateway-provided messages only.
"""

from __future__ import annotations

import base64
import binascii
import json
import logging
from typing import Callable, Mapping
from urllib import error, request
from urllib.parse import quote

from .runtime import BootstrapConfig, RuntimeResource, ToolCall, ToolResult, read_secret_value

logger = logging.getLogger(__name__)

# The single synthetic tool name published for REST resources (matches the
# control-plane published schema name, rest_tool_surface.go).
REST_CALL_TOOL_NAME = "rest_call"

# Opener injection point for tests (same shape as ControlPlaneClient).
Opener = Callable[[request.Request], object]


class RestCallTool:
    """Synthetic ``rest_call`` plugin: governed REST via the tool gateway.

    One instance covers ALL granted REST resources for the wake: the
    schema's ``resource_id`` enum is the granted-id list, so the model
    can only address resources this agent actually holds a grant for.
    The gateway re-validates everything server-side; this client-side
    check is a fast, network-free rejection (fail-closed).
    """

    name = REST_CALL_TOOL_NAME

    def __init__(
        self,
        gateway_url: str,
        resource_ids: tuple[str, ...],
        methods: tuple[str, ...],
        opener: Opener | None = None,
    ) -> None:
        self.gateway_url = gateway_url.rstrip("/")
        self.resource_ids = resource_ids
        self.methods = methods
        self._opener = opener or request.urlopen

    def tools(self) -> list[Mapping[str, object]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": REST_CALL_TOOL_NAME,
                    "description": (
                        "Call granted REST resources through the tool gateway. "
                        "Credentials are injected at the gateway (never visible to you); "
                        "every call is policy-checked and audited. "
                        "Only the resource_ids, methods and paths in your grants are allowed."
                    ),
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "resource_id": {
                                "type": "string",
                                "enum": list(self.resource_ids),
                                "description": "The granted REST resource to call.",
                            },
                            "method": {
                                "type": "string",
                                "enum": list(self.methods),
                                "description": "HTTP method; must be within the resource's effective ceiling.",
                            },
                            "path": {
                                "type": "string",
                                "description": "Relative path under the resource base_url (query string allowed).",
                            },
                            "headers": {
                                "type": "object",
                                "additionalProperties": {"type": "string"},
                                "description": "Optional agent-controlled headers (allowlist: accept, content-type).",
                            },
                            "body": {
                                "type": "string",
                                "description": "Optional request body.",
                            },
                        },
                        "required": ["resource_id", "method", "path"],
                        "additionalProperties": False,
                    },
                },
            }
        ]

    def invoke(self, call: ToolCall, config: BootstrapConfig) -> ToolResult:
        args = call.arguments if isinstance(call.arguments, Mapping) else {}
        resource_id = str(args.get("resource_id", ""))
        # Client-side grant check: never touch the gateway for an id the
        # agent was not granted (defense in depth; the gateway enforces too).
        if resource_id not in self.resource_ids:
            return ToolResult(content="resource not granted", ok=False)
        method = str(args.get("method", ""))
        path = str(args.get("path", ""))
        payload: dict[str, object] = {"method": method, "path": path}
        headers_arg = args.get("headers")
        if isinstance(headers_arg, Mapping) and headers_arg:
            payload["headers"] = {str(k): str(v) for k, v in headers_arg.items()}
        body_arg = args.get("body")
        if body_arg is not None and str(body_arg) != "":
            payload["body"] = str(body_arg)

        credential = read_secret_value(config.agent_credential_path) or ""
        url = f"{self.gateway_url}/v1/rest/{quote(resource_id, safe='')}"
        req = request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            headers={
                "Authorization": f"Bearer {credential}",
                "X-Skquad-Agent-ID": config.agent_id,
                "Content-Type": "application/json",
                "Accept": "application/json",
            },
            method="POST",
        )
        try:
            with self._opener(req) as response:
                raw = response.read()
                status = getattr(response, "status", None) or response.getcode()
        except error.HTTPError as exc:
            # Gateway/policy rejection (4xx/5xx): the LLM must SEE the
            # rejection, so carry it as a successful tool result. The
            # gateway error body is a controlled JSON envelope
            # ({error:{code,message}}); never echo the raw exception
            # (it can carry request headers in its repr).
            body_text = _safe_read_error_body(exc)
            logger.info(
                "rest_call: gateway rejected call for resource=%s: HTTP %s %s",
                resource_id,
                exc.code,
                body_text,
            )
            return ToolResult(
                content=f"gateway rejected the call: HTTP {exc.code} {body_text}".strip(),
                ok=True,
            )
        except Exception as exc:  # noqa: BLE001 — transport failure
            logger.warning(
                "rest_call: gateway transport failure for resource %s: %s",
                resource_id,
                type(exc).__name__,
            )
            return ToolResult(content="tool 'rest_call' failed: gateway unreachable", ok=False)

        try:
            envelope = json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return ToolResult(content="tool 'rest_call' failed: malformed gateway response", ok=False)
        if not isinstance(envelope, Mapping):
            return ToolResult(content="tool 'rest_call' failed: malformed gateway response", ok=False)
        # Audit-friendly, credential-free trace of the governed call.
        logger.info(
            "rest_call: resource=%s method=%s gateway_status=%s truncated=%s",
            resource_id,
            method,
            envelope.get("status", "?"),
            bool(envelope.get("truncated", False)),
        )
        return ToolResult(content=_render_gateway_envelope(envelope), ok=True)


def _render_gateway_envelope(envelope: Mapping[str, object]) -> str:
    """Map the gateway Response ({status, headers, bodyB64, truncated})."""
    status = envelope.get("status", "?")
    body_b64 = str(envelope.get("bodyB64", "") or "")
    truncated = bool(envelope.get("truncated", False))
    body_text = ""
    if body_b64:
        try:
            body_text = base64.b64decode(body_b64).decode("utf-8", errors="replace")
        except (binascii.Error, ValueError):
            body_text = "(undecodable body)"
    marker = " [truncated]" if truncated else ""
    return f"HTTP {status}{marker}\n{body_text}"


def _safe_read_error_body(exc: error.HTTPError) -> str:
    """Best-effort, bounded read of the gateway's error envelope."""
    try:
        raw = exc.read(4096)
    except Exception:  # noqa: BLE001
        return ""
    if not raw:
        return ""
    try:
        parsed = json.loads(raw.decode("utf-8", errors="replace"))
    except ValueError:
        return ""
    if isinstance(parsed, Mapping):
        err = parsed.get("error")
        if isinstance(err, Mapping):
            return f"{err.get('code', '')} {err.get('message', '')}".strip()
    return ""


def granted_rest_resources(resources: list[RuntimeResource]) -> list[RuntimeResource]:
    """Rest resources that publish a non-empty ``tools`` array (TG-4)."""
    return [
        resource
        for resource in resources
        if resource.resource_type == "rest" and resource.tools
    ]


def _rest_call_method_union(rest_resources: list[RuntimeResource]) -> tuple[str, ...]:
    """Union of the method enums across the granted resources' published
    schemas (parameters.properties.method.enum, falling back to the
    constraints.methods the TG-4 surface carries)."""
    methods: list[str] = []
    for resource in rest_resources:
        for tool in resource.tools:
            if str(tool.get("name", "")) != REST_CALL_TOOL_NAME:
                continue
            enum: object = None
            parameters = tool.get("parameters")
            if isinstance(parameters, Mapping):
                properties = parameters.get("properties")
                if isinstance(properties, Mapping):
                    method_prop = properties.get("method")
                    if isinstance(method_prop, Mapping):
                        enum = method_prop.get("enum")
            if not isinstance(enum, list) or not enum:
                constraints = tool.get("constraints")
                if isinstance(constraints, Mapping):
                    enum = constraints.get("methods")
            if isinstance(enum, list):
                for value in enum:
                    item = str(value)
                    if item and item not in methods:
                        methods.append(item)
    return tuple(methods)


def build_rest_call_tools(
    resources: list[RuntimeResource], config: BootstrapConfig
) -> list[RestCallTool]:
    """Register the single synthetic rest_call tool (TG-4d contract).

    Present only when (a) at least one granted rest resource publishes a
    tools array AND (b) SKQUAD_TOOL_GATEWAY_URL is configured. Otherwise
    [] — the tool is absent, fail-closed.
    """
    rest_resources = granted_rest_resources(resources)
    if not rest_resources:
        return []
    if not config.tool_gateway_url:
        logger.info(
            "rest_call: %d rest grant(s) present but SKQUAD_TOOL_GATEWAY_URL is unset; "
            "tool not registered (fail-closed)",
            len(rest_resources),
        )
        return []
    resource_ids = tuple(r.resource_id for r in rest_resources if r.resource_id)
    if not resource_ids:
        return []
    methods = _rest_call_method_union(rest_resources)
    return [RestCallTool(config.tool_gateway_url, resource_ids, methods)]


# Generic synthetic-tool registry: future drivers (mcp_call, browser,
# ssh) append their builder here and inherit both turn paths.
SYNTHETIC_TOOL_BUILDERS: tuple[
    Callable[[list[RuntimeResource], BootstrapConfig], list], ...
] = (build_rest_call_tools,)


def build_synthetic_tools(
    resources: list[RuntimeResource], config: BootstrapConfig
) -> list:
    """All synthetic tools for this wake (never raises; absent on any gap)."""
    tools: list = []
    for builder in SYNTHETIC_TOOL_BUILDERS:
        try:
            tools.extend(builder(resources, config))
        except Exception as exc:  # noqa: BLE001 — synthetic tools never break a wake
            logger.warning(
                "synthetic_tools: builder %s failed: %s", builder.__name__, type(exc).__name__
            )
    return tools
