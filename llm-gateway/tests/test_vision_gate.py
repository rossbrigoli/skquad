"""S-200: gateway vision-gate tests (stdlib-only, litellm shimmed).

Covers the pure gate logic and the async_pre_call_hook wiring with an
injected capability resolver — no live litellm router required.
"""

from __future__ import annotations

import asyncio
import sys
import types
import unittest
from pathlib import Path

GATEWAY_DIR = Path(__file__).resolve().parent.parent
for _candidate in (GATEWAY_DIR, Path("/app")):
    if str(_candidate) not in sys.path:
        sys.path.insert(0, str(_candidate))

try:  # pragma: no cover - depends on environment
    import litellm.integrations.custom_logger  # noqa: F401
except ModuleNotFoundError:  # pragma: no cover - test-only shim
    _litellm = types.ModuleType("litellm")
    _integrations = types.ModuleType("litellm.integrations")
    _custom_logger = types.ModuleType("litellm.integrations.custom_logger")

    class CustomLogger:
        def __init__(self, *a, **k):
            pass

    _custom_logger.CustomLogger = CustomLogger
    _integrations.custom_logger = _custom_logger
    _litellm.integrations = _integrations
    sys.modules.setdefault("litellm", _litellm)
    sys.modules.setdefault("litellm.integrations", _integrations)
    sys.modules.setdefault("litellm.integrations.custom_logger", _custom_logger)

import vision_gate  # noqa: E402
import skquad_litellm_callbacks as cb  # noqa: E402


def _img(url="data:image/png;base64,AAAA"):
    return {"type": "image_url", "image_url": {"url": url}}


def _txt(t):
    return {"type": "text", "text": t}


class ApplyVisionGateTest(unittest.TestCase):
    def test_capable_passthrough(self):
        data = {"model": "m", "messages": [{"role": "user", "content": [_txt("hi"), _img()]}]}
        out, stripped = vision_gate.apply_vision_gate(data, True)
        self.assertEqual(stripped, 0)
        self.assertEqual(out["messages"][0]["content"], [_txt("hi"), _img()])

    def test_unknown_capability_passthrough(self):
        data = {"model": "m", "messages": [{"role": "user", "content": [_img()]}]}
        out, stripped = vision_gate.apply_vision_gate(data, None)
        self.assertEqual(stripped, 0)
        self.assertEqual(out["messages"][0]["content"], [_img()])

    def test_incapable_strips_images_keeps_text(self):
        data = {
            "model": "m",
            "messages": [
                {"role": "system", "content": "sys"},
                {"role": "user", "content": [_txt("look [attached image/png]"), _img(), _img()]},
            ],
        }
        out, stripped = vision_gate.apply_vision_gate(data, False)
        self.assertEqual(stripped, 2)
        # system untouched (string content)
        self.assertEqual(out["messages"][0]["content"], "sys")
        user = out["messages"][1]["content"]
        # text preserved, images replaced by ONE marker
        self.assertEqual(user, [_txt("look [attached image/png]"), _txt(vision_gate.OMITTED_MARKER)])
        # no image parts survive
        self.assertFalse(vision_gate.message_has_image(user))

    def test_no_messages_noop(self):
        out, stripped = vision_gate.apply_vision_gate({"model": "m"}, False)
        self.assertEqual(stripped, 0)

    def test_string_content_untouched(self):
        data = {"model": "m", "messages": [{"role": "user", "content": "plain"}]}
        out, stripped = vision_gate.apply_vision_gate(data, False)
        self.assertEqual(stripped, 0)
        self.assertEqual(out["messages"][0]["content"], "plain")


class ResolverTest(unittest.TestCase):
    def test_present_true(self):
        r = vision_gate.make_model_info_resolver(lambda m: {"supports_vision": True})
        self.assertIs(r("m"), True)

    def test_present_false_activates_gate(self):
        r = vision_gate.make_model_info_resolver(lambda m: {"supports_vision": False})
        self.assertIs(r("m"), False)

    def test_absent_is_unknown(self):
        r = vision_gate.make_model_info_resolver(lambda m: {"other": 1})
        self.assertIsNone(r("m"))

    def test_getter_raises_is_unknown(self):
        def boom(m):
            raise RuntimeError("no router")
        r = vision_gate.make_model_info_resolver(boom)
        self.assertIsNone(r("m"))

    def test_empty_model_is_unknown(self):
        r = vision_gate.make_model_info_resolver(lambda m: {"supports_vision": False})
        self.assertIsNone(r(""))


class PreCallHookTest(unittest.TestCase):
    def _run(self, resolver, data):
        handler = cb.SkquadMeteringCallback(vision_resolver=resolver)
        return asyncio.run(handler.async_pre_call_hook(None, None, data, "completion"))

    def test_capable_leaves_images(self):
        data = {"model": "m", "messages": [{"role": "user", "content": [_img()]}]}
        out = self._run(lambda m: True, data)
        self.assertTrue(vision_gate.message_has_image(out["messages"][0]["content"]))

    def test_incapable_strips_with_marker(self):
        data = {
            "model": "m",
            "messages": [{"role": "user", "content": [_txt("ref [attached]"), _img()]}],
        }
        out = self._run(lambda m: False, data)
        content = out["messages"][0]["content"]
        self.assertFalse(vision_gate.message_has_image(content))
        self.assertIn(vision_gate.OMITTED_MARKER, content[1]["text"])
        self.assertIn("[attached]", content[0]["text"])

    def test_resolver_error_passthrough(self):
        def boom(m):
            raise RuntimeError("kaboom")
        data = {"model": "m", "messages": [{"role": "user", "content": [_img()]}]}
        out = self._run(boom, data)
        # unchanged — the hook must never break the call
        self.assertTrue(vision_gate.message_has_image(out["messages"][0]["content"]))


if __name__ == "__main__":
    unittest.main()
