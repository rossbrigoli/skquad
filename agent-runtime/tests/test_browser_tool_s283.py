"""S-283 tests — the built-in `browser` tool (gateway-driven browsing).

Mirrors the existing builtin-tool test style (unittest + fake urlopen,
no real network). Covers: fail-closed pre-flight (no gateway URL / no
browser grant => zero network), per-action dispatch to the correct
browser-service MCP tool through POST /v1/mcp/{rid}/call, auth headers,
argument mapping, screenshot-as-reference (no inline base64), snapshot
truncation, error surfacing (gateway denial / tool isError / malformed),
and registry registration + browser resource discovery.
"""

import base64
import io
import json
import pathlib
import tempfile
import unittest
from unittest import mock
from urllib import error

from skquad_runtime import builtin_tools as bt
from skquad_runtime.runtime import ToolCall, browser_resource_id

GATEWAY = "http://gateway.test"
RESOURCE_ID = "res-browser-1"
CALL_PATH = f"/v1/mcp/{RESOURCE_ID}/call"


def wrap(payload: dict) -> str:
    """Wrap a service payload the way the gateway browser driver does."""
    return (
        '<skquad_untrusted source="browser" resource="res-browser-1" '
        f'tool="x">{json.dumps(payload)}</skquad_untrusted>'
    )


def tc(args):
    return ToolCall(id="call-1", name="browser", arguments=args)


class FakeHTTPResponse:
    def __init__(self, status, payload):
        self.status = status
        self._payload = payload
        self.headers = {}

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def read(self, amt=None):
        return self._payload if amt is None else self._payload[:amt]

    def getcode(self):
        return self.status


def call_result(tool, payload, session_id="sess-1"):
    return {
        "tool": tool,
        "content": wrap(payload),
        "isError": False,
        "session_id": session_id,
    }


class BrowserToolTestBase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp_path = pathlib.Path(self._tmp.name)
        self.ctx = bt.BuiltinToolContext(
            workspace_dir=str(self.tmp_path / "ws"),
            control_plane_url="http://control-plane",
            agent_credential="cred-1",
            agent_id="agent-xyz",
            tool_gateway_url=GATEWAY,
            browser_resource_id=RESOURCE_ID,
        )

    def tool(self, policy=None):
        return bt.BrowserTool(policy or {}, self.ctx)

    def stub_gateway(self, responder):
        calls = []

        def fake_urlopen(req, timeout=None):
            calls.append(req)
            return responder(req)

        patcher = mock.patch(
            "skquad_runtime.builtin_tools.request.urlopen", side_effect=fake_urlopen
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        return calls

    def body_of(self, req):
        return json.loads(req.data.decode("utf-8"))


class FailClosedTests(BrowserToolTestBase):
    def test_no_gateway_url_no_network(self):
        ctx = bt.BuiltinToolContext(
            workspace_dir=str(self.tmp_path / "ws"),
            control_plane_url="http://cp",
            agent_credential="c",
            agent_id="a",
            browser_resource_id=RESOURCE_ID,
        )
        tool = bt.BrowserTool({}, ctx)
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = tool.invoke(tc({"action": "open", "url": "https://x.test"}), None)
        self.assertFalse(result.ok)
        self.assertIn("gateway", result.content)
        urlopen.assert_not_called()

    def test_no_browser_grant_no_network(self):
        ctx = bt.BuiltinToolContext(
            workspace_dir=str(self.tmp_path / "ws"),
            control_plane_url="http://cp",
            agent_credential="c",
            agent_id="a",
            tool_gateway_url=GATEWAY,
        )
        tool = bt.BrowserTool({}, ctx)
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = tool.invoke(tc({"action": "snapshot"}), None)
        self.assertFalse(result.ok)
        self.assertIn("no browser resource granted", result.content)
        urlopen.assert_not_called()

    def test_unknown_action_no_network(self):
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = self.tool().invoke(tc({"action": "press", "key": "Enter"}), None)
        self.assertFalse(result.ok)
        self.assertIn("unknown action", result.content)
        urlopen.assert_not_called()

    def test_open_rejects_non_http_scheme_without_network(self):
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = self.tool().invoke(tc({"action": "open", "url": "ftp://x/f"}), None)
        self.assertFalse(result.ok)
        self.assertIn("unsupported scheme", result.content)
        urlopen.assert_not_called()

    def test_click_requires_selector_without_network(self):
        with mock.patch("skquad_runtime.builtin_tools.request.urlopen") as urlopen:
            result = self.tool().invoke(tc({"action": "click"}), None)
        self.assertFalse(result.ok)
        self.assertIn("selector is required", result.content)
        urlopen.assert_not_called()


class DispatchTests(BrowserToolTestBase):
    def _standard_responder(self, req):
        body = json.loads(req.data.decode("utf-8"))
        tool = body["tool"]
        if tool == "browser.navigate":
            return FakeHTTPResponse(
                200,
                json.dumps(
                    call_result("browser.navigate", {"url": body["arguments"]["url"], "title": "Example Domain", "page_index": 1})
                ).encode(),
            )
        if tool == "browser.extract":
            return FakeHTTPResponse(
                200,
                json.dumps(call_result("browser.extract", {"text": "hello visible text"})).encode(),
            )
        if tool == "browser.screenshot":
            png = b"\x89PNG-fake"
            return FakeHTTPResponse(
                200,
                json.dumps(
                    call_result(
                        "browser.screenshot",
                        {
                            "screenshot_b64": base64.b64encode(png).decode(),
                            "bytes": len(png),
                            "mime": "image/png",
                        },
                    )
                ).encode(),
            )
        if tool == "browser.click":
            return FakeHTTPResponse(
                200, json.dumps(call_result("browser.click", {"clicked": body["arguments"]["selector"]})).encode()
            )
        if tool == "browser.type":
            return FakeHTTPResponse(
                200, json.dumps(call_result("browser.type", {"typed": body["arguments"]["selector"]})).encode()
            )
        if tool == "browser.close_session":
            return FakeHTTPResponse(
                200, json.dumps(call_result("browser.close_session", {"closed": "sess-1"})).encode()
            )
        raise AssertionError(f"unexpected tool {tool}")

    def test_open_navigates_then_extracts_with_correct_url_and_auth(self):
        calls = self.stub_gateway(self._standard_responder)
        result = self.tool().invoke(tc({"action": "open", "url": "https://example.com"}), None)
        self.assertTrue(result.ok, result.content)
        self.assertEqual(len(calls), 2)
        first, second = calls
        self.assertEqual(first.get_method(), "POST")
        self.assertEqual(first.full_url, GATEWAY + CALL_PATH)
        self.assertEqual(first.get_header("Authorization"), "Bearer cred-1")
        self.assertEqual(first.get_header("X-skquad-agent-id"), "agent-xyz")
        self.assertEqual(self.body_of(first), {"tool": "browser.navigate", "arguments": {"url": "https://example.com"}})
        self.assertEqual(self.body_of(second)["tool"], "browser.extract")
        self.assertIn("opened: https://example.com", result.content)
        self.assertIn("title: Example Domain", result.content)
        self.assertIn("hello visible text", result.content)

    def test_snapshot_maps_to_browser_extract(self):
        calls = self.stub_gateway(self._standard_responder)
        result = self.tool().invoke(tc({"action": "snapshot"}), None)
        self.assertTrue(result.ok, result.content)
        self.assertEqual(len(calls), 1)
        self.assertEqual(self.body_of(calls[0]), {"tool": "browser.extract", "arguments": {}})
        self.assertEqual(result.content, "hello visible text")

    def test_snapshot_truncates_to_policy_cap(self):
        long_text = "x" * 500

        def responder(req):
            return FakeHTTPResponse(
                200, json.dumps(call_result("browser.extract", {"text": long_text})).encode()
            )

        self.stub_gateway(responder)
        result = self.tool({"maxSnapshotChars": 100}).invoke(tc({"action": "snapshot"}), None)
        self.assertTrue(result.ok)
        self.assertTrue(result.content.startswith("x" * 100))
        self.assertIn("truncated", result.content)
        self.assertIn("500 chars total", result.content)

    def test_click_maps_selector(self):
        calls = self.stub_gateway(self._standard_responder)
        result = self.tool().invoke(tc({"action": "click", "selector": "#submit"}), None)
        self.assertTrue(result.ok, result.content)
        self.assertEqual(self.body_of(calls[0]), {"tool": "browser.click", "arguments": {"selector": "#submit"}})

    def test_type_maps_selector_text_submit(self):
        calls = self.stub_gateway(self._standard_responder)
        result = self.tool().invoke(
            tc({"action": "type", "selector": "input[name=q]", "text": "hello", "submit": True}), None
        )
        self.assertTrue(result.ok, result.content)
        self.assertEqual(
            self.body_of(calls[0]),
            {"tool": "browser.type", "arguments": {"selector": "input[name=q]", "text": "hello", "submit": True}},
        )

    def test_close_maps_to_close_session(self):
        calls = self.stub_gateway(self._standard_responder)
        result = self.tool().invoke(tc({"action": "close"}), None)
        self.assertTrue(result.ok, result.content)
        self.assertEqual(self.body_of(calls[0]), {"tool": "browser.close_session", "arguments": {}})


class ScreenshotReferenceTests(BrowserToolTestBase):
    def test_screenshot_returns_reference_not_inline_base64(self):
        png = b"\x89PNG\r\n\x1a\nFAKEIMAGEDATA"
        b64 = base64.b64encode(png).decode()

        def responder(req):
            return FakeHTTPResponse(
                200,
                json.dumps(
                    call_result("browser.screenshot", {"screenshot_b64": b64, "bytes": len(png), "mime": "image/png"})
                ).encode(),
            )

        calls = self.stub_gateway(responder)
        result = self.tool().invoke(tc({"action": "screenshot"}), None)
        self.assertTrue(result.ok, result.content)
        self.assertEqual(self.body_of(calls[0])["tool"], "browser.screenshot")
        # The result must be a reference, never the base64 blob.
        self.assertNotIn(b64, result.content)
        self.assertIn("screenshot saved:", result.content)
        self.assertIn("browser/screenshots/", result.content)
        self.assertIn(f"{len(png)} bytes", result.content)
        # The file exists on disk with the exact bytes.
        saved = list((self.tmp_path / "ws" / "browser" / "screenshots").glob("screenshot-*.png"))
        self.assertEqual(len(saved), 1)
        self.assertEqual(saved[0].read_bytes(), png)

    def test_screenshot_over_cap_is_not_saved(self):
        big = b"x" * 5000
        b64 = base64.b64encode(big).decode()

        def responder(req):
            return FakeHTTPResponse(
                200,
                json.dumps(
                    call_result("browser.screenshot", {"screenshot_b64": b64, "bytes": len(big), "mime": "image/png"})
                ).encode(),
            )

        self.stub_gateway(responder)
        result = self.tool({"maxScreenshotBytes": 100}).invoke(tc({"action": "screenshot"}), None)
        self.assertFalse(result.ok)
        self.assertIn("over the 100-byte policy cap", result.content)
        self.assertFalse((self.tmp_path / "ws" / "browser" / "screenshots").exists())

    def test_screenshot_malformed_base64_fails_cleanly(self):
        def responder(req):
            return FakeHTTPResponse(
                200,
                json.dumps(call_result("browser.screenshot", {"screenshot_b64": "!!!not-base64!!!", "bytes": 1})).encode(),
            )

        self.stub_gateway(responder)
        result = self.tool().invoke(tc({"action": "screenshot"}), None)
        self.assertFalse(result.ok)
        self.assertIn("malformed image data", result.content)


class ErrorSurfacingTests(BrowserToolTestBase):
    def test_gateway_denial_surfaces_reason(self):
        # Real urlopen raises HTTPError for 4xx/5xx; mirror that.
        def responder(req):
            raise error.HTTPError(
                "http://gateway.test" + CALL_PATH,
                403,
                "Forbidden",
                {},
                io.BytesIO(json.dumps({"error": "denied", "message": "ceiling_exceeded"}).encode()),
            )

        self.stub_gateway(responder)
        result = self.tool().invoke(tc({"action": "open", "url": "https://denied.test"}), None)
        self.assertFalse(result.ok)
        self.assertIn("HTTP 403", result.content)
        self.assertIn("ceiling_exceeded", result.content)

    def test_tool_is_error_envelope_surfaces_content(self):
        def responder(req):
            return FakeHTTPResponse(
                200,
                json.dumps(
                    {
                        "tool": "browser.click",
                        "content": "no page in session; call browser.navigate first",
                        "isError": True,
                    }
                ).encode(),
            )

        self.stub_gateway(responder)
        result = self.tool().invoke(tc({"action": "click", "selector": "#x"}), None)
        self.assertFalse(result.ok)
        self.assertIn("browser tool error", result.content)
        self.assertIn("no page in session", result.content)

    def test_malformed_gateway_response_fails_cleanly(self):
        def responder(req):
            return FakeHTTPResponse(200, b"not json at all")

        self.stub_gateway(responder)
        result = self.tool().invoke(tc({"action": "snapshot"}), None)
        self.assertFalse(result.ok)
        self.assertIn("malformed gateway response", result.content)

    def test_transport_failure_fails_cleanly(self):
        def responder(req):
            raise error.URLError("connection refused")

        self.stub_gateway(responder)
        result = self.tool().invoke(tc({"action": "snapshot"}), None)
        self.assertFalse(result.ok)
        self.assertIn("gateway request failed", result.content)


class RegistryTests(unittest.TestCase):
    def test_browser_registered_as_builtin(self):
        self.assertIs(bt._BUILTIN_REGISTRY["browser"], bt.BrowserTool)

    def test_build_builtin_tools_instantiates_enabled_browser(self):
        ctx = bt.BuiltinToolContext(
            workspace_dir="/tmp/ws",
            control_plane_url="http://cp",
            agent_credential="c",
            agent_id="a",
            tool_gateway_url=GATEWAY,
            browser_resource_id=RESOURCE_ID,
        )
        tools = bt.build_builtin_tools(
            {"tools": [{"name": "browser", "enabled": True, "policy": {"timeoutSeconds": 45}}]},
            ctx,
        )
        self.assertEqual(len(tools), 1)
        self.assertIsInstance(tools[0], bt.BrowserTool)
        self.assertEqual(tools[0].policy["timeoutSeconds"], 45)

    def test_build_builtin_tools_skips_disabled_browser(self):
        ctx = bt.BuiltinToolContext(
            workspace_dir="/tmp/ws", control_plane_url="http://cp", agent_credential="c", agent_id="a"
        )
        tools = bt.build_builtin_tools(
            {"tools": [{"name": "browser", "enabled": False, "policy": {}}]}, ctx
        )
        self.assertEqual(tools, [])


class BrowserResourceDiscoveryTests(unittest.TestCase):
    class _Res:
        def __init__(self, resource_type, resource_id, endpoint_config):
            self.resource_type = resource_type
            self.resource_id = resource_id
            self.endpoint_config = endpoint_config

    def test_finds_mcp_resource_with_browser_driver(self):
        resources = [
            self._Res("rest", "r1", {}),
            self._Res("mcp", "r-plain", {"driver": "mcp"}),
            self._Res("mcp", RESOURCE_ID, {"driver": "Browser", "url": "http://svc/mcp"}),
        ]
        self.assertEqual(browser_resource_id(resources), RESOURCE_ID)

    def test_no_browser_resource_returns_empty(self):
        resources = [
            self._Res("mcp", "r-plain", {"url": "http://other/mcp"}),
            self._Res("web", "r-web", {"driver": "browser"}),  # wrong type
        ]
        self.assertEqual(browser_resource_id(resources), "")

    def test_empty_inputs(self):
        self.assertEqual(browser_resource_id([]), "")


if __name__ == "__main__":
    unittest.main()
