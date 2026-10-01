"""S-194: image attachment references in the agent chat path.

The agent receives a text reference (filename, MIME type, control-plane
URL) inside the trust-labeled message — not raw image bytes. Full vision
passthrough is a documented follow-up.
"""

import unittest

from skquad_runtime.runtime import (
    LLMMessageHandler,
    RuntimeMessage,
    format_attachment_note,
)


def _user_msg(payload):
    return RuntimeMessage(
        id="m-194", from_type="user", from_id="ross", to_agent_id="enzo",
        squad_id="s-1", message_type="consult",
        payload=payload,
        status="pending", correlation_id="",
    )


class FormatAttachmentNoteTest(unittest.TestCase):
    def test_renders_reference_line(self):
        note = format_attachment_note([
            {
                "id": "abc",
                "filename": "ui-bug.png",
                "content_type": "image/png",
                "url": "/api/v1/uploads/abc",
            }
        ])
        self.assertEqual(note, "[attached image/png: ui-bug.png at /api/v1/uploads/abc]")

    def test_multiple_attachments(self):
        note = format_attachment_note([
            {"filename": "a.png", "content_type": "image/png", "url": "/u/1"},
            {"filename": "b.gif", "content_type": "image/gif", "url": "/u/2"},
        ])
        self.assertEqual(
            note,
            "[attached image/png: a.png at /u/1]\n[attached image/gif: b.gif at /u/2]",
        )

    def test_malformed_entries_skipped(self):
        self.assertEqual(format_attachment_note(None), "")
        self.assertEqual(format_attachment_note("junk"), "")
        self.assertEqual(format_attachment_note([None, 42]), "")
        self.assertEqual(format_attachment_note([{"filename": "no-url.png"}]), "")

    def test_missing_fields_default(self):
        note = format_attachment_note([{"url": "/u/3"}])
        self.assertEqual(note, "[attached image: image at /u/3]")


class TrustLabelCarriesAttachmentsTest(unittest.TestCase):
    def test_user_message_appends_attachment_note(self):
        msg = _user_msg({
            "message": "look at this",
            "attachments": [
                {
                    "id": "abc",
                    "filename": "ui-bug.png",
                    "content_type": "image/png",
                    "url": "/api/v1/uploads/abc",
                }
            ],
        })
        labeled = LLMMessageHandler._with_trust_label(msg)
        text = labeled.payload["message"]
        self.assertIn("<skquad_human", text)
        self.assertIn("look at this", text)
        self.assertIn("[attached image/png: ui-bug.png at /api/v1/uploads/abc]", text)
        self.assertTrue(labeled.payload["_skquad_trusted"])

    def test_image_only_message_still_delivered(self):
        msg = _user_msg({
            "attachments": [
                {"filename": "shot.png", "content_type": "image/png", "url": "/u/9"}
            ],
        })
        labeled = LLMMessageHandler._with_trust_label(msg)
        text = labeled.payload["message"]
        self.assertIn("[attached image/png: shot.png at /u/9]", text)
        self.assertIn("<skquad_human", text)

    def test_agent_mail_gets_untrusted_note(self):
        msg = RuntimeMessage(
            id="m-194b", from_type="agent", from_id="agent:other", to_agent_id="enzo",
            squad_id="s-1", message_type="consult",
            payload={"message": "see attached", "attachments": [{"url": "/u/4", "filename": "x.png", "content_type": "image/png"}]},
            status="pending", correlation_id="",
        )
        labeled = LLMMessageHandler._with_trust_label(msg)
        text = labeled.payload["message"]
        self.assertIn("<skquad_untrusted", text)
        self.assertIn("[attached image/png: x.png at /u/4]", text)

    def test_no_attachments_unchanged_behavior(self):
        msg = _user_msg({"message": "plain"})
        labeled = LLMMessageHandler._with_trust_label(msg)
        self.assertIn("plain", labeled.payload["message"])
        self.assertNotIn("[attached", labeled.payload["message"])


if __name__ == "__main__":
    unittest.main()
