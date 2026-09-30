"""S-161: tiered context compaction tests.

Covers: tier selection, micro-prune clipping, rolling-window eviction,
emergency compression into the system prompt, the injected-LLM
summarizer path, and — critically — the OpenAI tool-call pairing
invariant across every eviction tier.
"""

import unittest

from skquad_runtime.context_compaction import (
    ContextCompactor,
    context_token_limit,
    estimate_messages_tokens,
    group_turns,
)


def sys_msg(text="SYSTEM PROMPT"):
    return {"role": "system", "content": text}


def user_msg(text):
    return {"role": "user", "content": text}


def assistant_tool_group(call_id="call-1", name="echo", tool_text="tool output"):
    return [
        {
            "role": "assistant",
            "content": "calling tool",
            "tool_calls": [
                {"id": call_id, "type": "function", "function": {"name": name, "arguments": "{}"}}
            ],
        },
        {"role": "tool", "tool_call_id": call_id, "name": name, "content": tool_text},
    ]


def assert_pairing(testcase, messages):
    """Every tool message must directly follow the assistant turn that
    issued its tool_call id (no dangling ids, no orphan tool results)."""
    pending = set()
    for m in messages:
        role = m.get("role")
        if role == "assistant":
            calls = m.get("tool_calls") or []
            pending = {c["id"] for c in calls}
            if calls:
                continue  # tool messages must follow before any other role
        elif role == "tool":
            testcase.assertIn(
                m["tool_call_id"], pending, f"orphan tool message for {m['tool_call_id']}"
            )
        else:
            pending = set()


def build_convo(groups=8, tool_chars=130):
    msgs = [sys_msg()]
    for i in range(groups):
        msgs.extend(assistant_tool_group(call_id=f"c{i}", tool_text="x" * tool_chars))
    return msgs


class TierSelectionTest(unittest.TestCase):
    def test_below_tier1_untouched(self):
        msgs = build_convo(groups=2)
        compactor = ContextCompactor(limit=100_000)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 0)
        self.assertEqual(out, [dict(m) for m in msgs])

    def test_tier1_clips_big_tool_results_without_removing_turns(self):
        msgs = build_convo(groups=6)
        # One oversized tool result (second group's tool message).
        msgs[4]["content"] = "y" * 5000
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.60)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 1)
        self.assertEqual(len(out), len(msgs))  # no turns removed
        self.assertEqual(report.clipped_messages, 1)
        clipped = [m for m in out if m.get("role") == "tool" and "clipped" in str(m["content"])]
        self.assertEqual(len(clipped), 1)
        self.assertLess(len(clipped[0]["content"]), 5000)

    def test_tier1_empty_tool_result_placeholder(self):
        msgs = build_convo(groups=6)
        msgs[4]["content"] = "   "
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.60)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 1)
        self.assertEqual(len(out), len(msgs))
        self.assertEqual(out[4]["content"], "(empty result)")

    def test_tier2_evicts_oldest_groups_and_inserts_digest(self):
        msgs = build_convo(groups=12)
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.80)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 2)
        self.assertGreater(report.evicted_turns, 0)
        self.assertLess(len(out), len(msgs))
        # System prompt kept verbatim at the front.
        self.assertEqual(out[0]["role"], "system")
        self.assertEqual(out[0]["content"], "SYSTEM PROMPT")
        # A digest message exists.
        digest_msgs = [m for m in out if "skquad_context_digest" in str(m.get("content", ""))]
        self.assertEqual(len(digest_msgs), 1)
        self.assertEqual(digest_msgs[0]["role"], "user")
        # Pairing invariant holds after eviction.
        assert_pairing(self, out)
        # The most recent group is still verbatim.
        self.assertEqual(out[-2:], [dict(m) for m in msgs[-2:]])

    def test_tier3_folds_digest_into_system_prompt(self):
        msgs = build_convo(groups=16)
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.95)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 3)
        # System prompt now carries the digest.
        self.assertIn("emergency-compressed", out[0]["content"])
        self.assertIn("SYSTEM PROMPT", out[0]["content"])
        # Only the last 3 groups (6 messages) follow the system prompt.
        self.assertEqual(len(out), 1 + 6)
        assert_pairing(self, out)

    def test_tier3_without_system_prompt(self):
        msgs = build_convo(groups=16)
        msgs = [m for m in msgs if m["role"] != "system"]
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.95)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 3)
        self.assertIn("skquad_context_digest", str(out[0]["content"]))
        assert_pairing(self, out)

    def test_tier3_keeps_user_message_when_user_turn_evicted(self):
        # Incident 2026-09-30: user query + trailing assistant/tool groups.
        # Tier 3 evicted the user turn into the SYSTEM digest, leaving no
        # user-role message; Qwen3 template rejected with "No user query
        # found in messages". The digest must surface as a user message.
        msgs = [sys_msg(), user_msg("look at the skquad project")]
        msgs += assistant_tool_group("c1") * 1
        for i in range(2, 8):
            msgs.extend(assistant_tool_group(call_id=f"c{i}", tool_text="x" * 4000))
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.95)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 3)
        self.assertTrue(any(m["role"] == "user" for m in out))
        # The user's original query survives inside the user-role digest.
        user_msgs = [m for m in out if m["role"] == "user"]
        self.assertIn("look at the skquad project", str(user_msgs[0]["content"]))
        # System prompt stays verbatim (no digest folded in).
        self.assertEqual(out[0]["content"], "SYSTEM PROMPT")
        assert_pairing(self, out)

    def test_tier3_folds_into_system_when_user_kept(self):
        # User turn inside the kept window → old fold-into-system behavior.
        msgs = [sys_msg()]
        for i in range(2, 8):
            msgs.extend(assistant_tool_group(call_id=f"c{i}", tool_text="x" * 4000))
        msgs.append(user_msg("latest question"))
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.95)
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 3)
        self.assertIn("emergency-compressed", out[0]["content"])
        self.assertTrue(any(m["role"] == "user" for m in out))
        assert_pairing(self, out)

    def test_no_user_message_never_invented(self):
        # Conversations with no user turn at all must not gain one.
        msgs = build_convo(groups=16)
        compactor = ContextCompactor(limit=estimate_messages_tokens(msgs) // 0.95)
        out, _ = compactor.maybe_compact(msgs)
        self.assertFalse(any(m["role"] == "user" for m in out))


class SummarizerTest(unittest.TestCase):
    def test_injected_summarizer_used(self):
        msgs = build_convo(groups=12)
        compactor = ContextCompactor(
            limit=estimate_messages_tokens(msgs) // 0.80,
            summarizer=lambda transcript: "LLM SUMMARY " + str(len(transcript)),
        )
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 2)
        joined = " ".join(str(m.get("content", "")) for m in out)
        self.assertIn("LLM SUMMARY", joined)

    def test_summarizer_failure_falls_back_to_deterministic(self):
        msgs = build_convo(groups=12)

        def boom(_):
            raise RuntimeError("llm down")

        compactor = ContextCompactor(
            limit=estimate_messages_tokens(msgs) // 0.80,
            summarizer=boom,
        )
        out, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 2)
        joined = " ".join(str(m.get("content", "")) for m in out)
        self.assertIn("skquad_context_digest", joined)
        self.assertNotIn("llm down", joined)


class GroupingTest(unittest.TestCase):
    def test_groups_are_atomic_for_tool_pairs(self):
        msgs = [sys_msg(), user_msg("hi")] + assistant_tool_group() + assistant_tool_group("c9", "f")
        groups = group_turns(msgs)
        self.assertEqual([len(g) for g in groups], [1, 1, 2, 2])

    def test_assistant_without_calls_is_single(self):
        msgs = [{"role": "assistant", "content": "plain"}]
        self.assertEqual([len(g) for g in group_turns(msgs)], [1])


class EnvTest(unittest.TestCase):
    def test_env_override(self):
        self.assertEqual(context_token_limit({"SKQUAD_MODEL_CONTEXT_TOKENS": "8192"}), 8192)
        self.assertEqual(context_token_limit({"SKQUAD_MODEL_CONTEXT_TOKENS": "junk"}), 32768)
        self.assertEqual(context_token_limit({}), 32768)


if __name__ == "__main__":
    unittest.main()
