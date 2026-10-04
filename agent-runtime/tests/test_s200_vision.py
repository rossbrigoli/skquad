"""S-200: vision passthrough for attached images with model capability gating.

Covers the runtime side of the feature:
- build_image_content_parts: multimodal payload construction, MIME
  allow-list, fetch-failure/empty handling, count + base64-budget limits,
  and the byte-identical plain-text fallback when nothing embeds.
- LLMMessageHandler._model_supports_vision: capability read + caching +
  fail-safe (unknown ⇒ not capable).
- LLMMessageHandler._build_chat_messages: capable model embeds image_url
  parts; non-capable model keeps the plain-text reference (graceful
  degradation, no exception).
- ControlPlaneClient.get_my_model / fetch_upload_bytes via an injected
  opener (no network).
"""

import base64
import unittest

from skquad_runtime.runtime import (
    ControlPlaneClient,
    LLMMessageHandler,
    RuntimeMessage,
    VISION_ALLOWED_MIME,
    build_image_content_parts,
)


PNG = b"\x89PNG\r\n\x1a\n" + b"0" * 32
JPEG = b"\xff\xd8\xff" + b"1" * 40


def _attach(uid, mime="image/png", name="shot.png"):
    return {"id": uid, "content_type": mime, "filename": name, "url": f"/u/{uid}"}


def _user_msg(payload):
    return RuntimeMessage(
        id="m-200", from_type="user", from_id="ross", to_agent_id="enzo",
        squad_id="s-1", message_type="consult", payload=payload,
        status="pending", correlation_id="",
    )


class _StubPrompted:
    def chat_system_prompt(self):
        return "SYS"


class _VisionClient:
    """Minimal ControlPlaneClient stand-in for the chat-build path."""

    def __init__(self, supports_vision, uploads=None, model_raises=False):
        self._supports_vision = supports_vision
        self.uploads = uploads or {}
        self.model_raises = model_raises
        self.model_calls = 0
        self.fetch_calls = []

    def list_message_history(self):
        return []

    def list_peers(self):
        return []

    def get_my_model(self):
        self.model_calls += 1
        if self.model_raises:
            raise RuntimeError("boom")
        return {"model_name": "gpt-4o", "supports_vision": self._supports_vision}

    def fetch_upload_bytes(self, upload_id):
        self.fetch_calls.append(upload_id)
        if upload_id not in self.uploads:
            raise RuntimeError(f"404 {upload_id}")
        return self.uploads[upload_id]


class BuildImageContentPartsTest(unittest.TestCase):
    def test_embeds_allowed_mime_as_data_url(self):
        content, stats = build_image_content_parts(
            "look", [_attach("a", "image/png")], lambda uid: PNG
        )
        self.assertEqual(stats["embedded"], 1)
        self.assertEqual(stats["skipped"], 0)
        self.assertIsInstance(content, list)
        self.assertEqual(content[0], {"type": "text", "text": "look"})
        img = content[1]
        self.assertEqual(img["type"], "image_url")
        expected = "data:image/png;base64," + base64.b64encode(PNG).decode("ascii")
        self.assertEqual(img["image_url"]["url"], expected)

    def test_skips_disallowed_mime(self):
        content, stats = build_image_content_parts(
            "t", [_attach("a", "application/pdf")], lambda uid: PNG
        )
        self.assertEqual(stats["embedded"], 0)
        self.assertEqual(stats["skipped"], 1)
        self.assertIn("a:mime_not_allowed", stats["reasons"])
        # Nothing embedded ⇒ plain text, byte-identical to S-194 path.
        self.assertEqual(content, "t")

    def test_fetch_failure_is_graceful(self):
        def boom(uid):
            raise RuntimeError("network down")

        content, stats = build_image_content_parts("t", [_attach("a")], boom)
        self.assertEqual(stats["embedded"], 0)
        self.assertEqual(stats["skipped"], 1)
        self.assertIn("a:fetch_failed", stats["reasons"])
        self.assertEqual(content, "t")

    def test_empty_bytes_skipped(self):
        content, stats = build_image_content_parts("t", [_attach("a")], lambda uid: b"")
        self.assertEqual(stats["embedded"], 0)
        self.assertIn("a:empty", stats["reasons"])
        self.assertEqual(content, "t")

    def test_max_images_limit(self):
        uploads = {"a": PNG, "b": PNG, "c": PNG}
        content, stats = build_image_content_parts(
            "t",
            [_attach("a"), _attach("b"), _attach("c")],
            lambda uid: uploads[uid],
            max_images=2,
        )
        self.assertEqual(stats["embedded"], 2)
        self.assertIn("c:max_images", stats["reasons"])
        # text + 2 images
        self.assertEqual(len(content), 3)

    def test_base64_budget_accounts_for_inflation(self):
        # Each PNG base64-encodes to a known size; a budget that fits one
        # but not two must embed exactly one and skip the second on budget.
        one_b64 = len(base64.b64encode(PNG))
        budget = one_b64  # fits exactly one
        content, stats = build_image_content_parts(
            "t",
            [_attach("a"), _attach("b")],
            lambda uid: PNG,
            max_total_b64_bytes=budget,
        )
        self.assertEqual(stats["embedded"], 1)
        self.assertEqual(stats["total_b64_bytes"], one_b64)
        self.assertIn("b:budget", stats["reasons"])

    def test_budget_is_strictly_greater_than_raw(self):
        # base64 inflation ~1.33x: a budget equal to the RAW size must
        # NOT fit the base64 form (proves we budget the encoded size).
        raw = PNG * 8  # 320 bytes
        content, stats = build_image_content_parts(
            "t", [_attach("a")], lambda uid: raw, max_total_b64_bytes=len(raw)
        )
        self.assertEqual(stats["embedded"], 0)
        self.assertIn("a:budget", stats["reasons"])

    def test_no_attachments_returns_text(self):
        for bad in (None, "junk", [], {}):
            content, stats = build_image_content_parts("t", bad, lambda uid: PNG)
            self.assertEqual(content, "t")
            self.assertEqual(stats["embedded"], 0)

    def test_allowed_mime_set_matches_upload_allowlist(self):
        self.assertEqual(
            set(VISION_ALLOWED_MIME),
            {"image/png", "image/jpeg", "image/gif", "image/webp"},
        )


class ModelSupportsVisionTest(unittest.TestCase):
    def _handler(self, client):
        return LLMMessageHandler(completion=lambda **k: None, client=client)

    def test_capable_model(self):
        h = self._handler(_VisionClient(True))
        self.assertTrue(h._model_supports_vision(_cfg(), "gpt-4o"))

    def test_incapable_model(self):
        h = self._handler(_VisionClient(False))
        self.assertFalse(h._model_supports_vision(_cfg(), "gpt-4o"))

    def test_fetch_failure_is_not_capable(self):
        h = self._handler(_VisionClient(True, model_raises=True))
        self.assertFalse(h._model_supports_vision(_cfg(), "gpt-4o"))

    def test_cached_after_first_call(self):
        client = _VisionClient(True)
        h = self._handler(client)
        self.assertTrue(h._model_supports_vision(_cfg(), "gpt-4o"))
        self.assertTrue(h._model_supports_vision(_cfg(), "gpt-4o"))
        self.assertEqual(client.model_calls, 1)  # second served from cache


class BuildChatMessagesVisionTest(unittest.TestCase):
    def _handler(self, client):
        return LLMMessageHandler(completion=lambda **k: None, client=client)

    def test_capable_model_embeds_image_part(self):
        client = _VisionClient(True, uploads={"a": PNG})
        h = self._handler(client)
        msg = _user_msg({
            "message": "check this",
            "attachments": [_attach("a")],
        })
        # Pre-label so the text carries the S-194 reference (as the real
        # wake path does before _build_chat_messages runs).
        msg = LLMMessageHandler._with_trust_label(msg)
        chat = h._build_chat_messages(msg, _cfg(), _StubPrompted(), "gpt-4o")
        user_turn = chat[-1]
        self.assertEqual(user_turn["role"], "user")
        self.assertIsInstance(user_turn["content"], list)
        types = [p["type"] for p in user_turn["content"]]
        self.assertIn("image_url", types)
        # The text part still carries the attachment reference.
        text_part = user_turn["content"][0]["text"]
        self.assertIn("[attached image/png: shot.png at /u/a]", text_part)

    def test_incapable_model_keeps_plain_text_reference(self):
        client = _VisionClient(False, uploads={"a": PNG})
        h = self._handler(client)
        msg = LLMMessageHandler._with_trust_label(_user_msg({
            "message": "check this",
            "attachments": [_attach("a")],
        }))
        chat = h._build_chat_messages(msg, _cfg(), _StubPrompted(), "gpt-4o")
        user_turn = chat[-1]
        # Graceful degradation: plain string, text reference intact,
        # and NO upload fetch attempted (bytes never pulled).
        self.assertIsInstance(user_turn["content"], str)
        self.assertIn("[attached image/png: shot.png at /u/a]", user_turn["content"])
        self.assertEqual(client.fetch_calls, [])

    def test_capable_but_fetch_fails_degrades_to_text(self):
        # Capable model, but the upload can't be fetched → plain text,
        # no exception, reference preserved.
        client = _VisionClient(True, uploads={})
        h = self._handler(client)
        msg = LLMMessageHandler._with_trust_label(_user_msg({
            "message": "check this",
            "attachments": [_attach("missing")],
        }))
        chat = h._build_chat_messages(msg, _cfg(), _StubPrompted(), "gpt-4o")
        self.assertIsInstance(chat[-1]["content"], str)
        self.assertIn("[attached", chat[-1]["content"])


class ControlPlaneClientBytesTest(unittest.TestCase):
    def _client(self, opener):
        return ControlPlaneClient(
            "http://cp.test", "agent-1", "cred", opener=opener
        )

    def test_fetch_upload_bytes_reads_body(self):
        class Resp:
            status = 200
            def read(self):
                return PNG
            def __enter__(self):
                return self
            def __exit__(self, *a):
                return False

        seen = {}

        def opener(req):
            seen["url"] = req.full_url
            seen["auth"] = req.get_header("Authorization")
            return Resp()

        client = self._client(opener)
        got = client.fetch_upload_bytes("abc")
        self.assertEqual(got, PNG)
        self.assertEqual(seen["url"], "http://cp.test/api/v1/agents/me/uploads/abc")
        self.assertEqual(seen["auth"], "Bearer cred")

    def test_get_my_model_parses_json(self):
        body = b'{"model_name":"gpt-4o","supports_vision":true}'

        class Resp:
            status = 200
            def read(self):
                return body
            def __enter__(self):
                return self
            def __exit__(self, *a):
                return False

        client = self._client(lambda req: Resp())
        self.assertTrue(client.get_my_model()["supports_vision"])


def _cfg():
    from skquad_runtime.runtime import BootstrapConfig
    from pathlib import Path
    return BootstrapConfig(
        agent_id="agent-1", squad_id="s-1", role="helper",
        default_model="gpt-4o", idle_timeout="300",
        credentials_dir=Path("/tmp"), agent_credential_path=Path("/tmp/agent"),
        virtual_key_path=Path("/tmp/key"), control_plane_url="http://cp.test",
        llm_gateway_url="http://gw.test", task_loop_enabled=False,
        task_poll_interval_seconds=30, inbox_poll_interval_seconds=15,
        inbox_batch_size=5, task_timeout_seconds=600,
        heartbeat_interval_seconds=15, max_llm_steps=8,
        task_summary_max_chars=2000, plugin_modules=(), enabled_plugins=(),
    )


if __name__ == "__main__":
    unittest.main()
