"""S-161: tiered context-window compaction.

The agent loop (task steps, chat turns) grows its message list with
every tool result. This module keeps that growth under control with
three tiers, keyed on the estimated fill ratio of the model's context
window:

Tier 1 — micro-prune (50%-75% fill):
    Cheap hygiene. Oversized tool results are clipped to a head+tail
    window; blank tool results get a placeholder. No turns removed.

Tier 2 — rolling window (75%-90% fill):
    The oldest 20% of non-system turn GROUPS are evicted and replaced
    by a single compact digest message (deterministic by default; an
    LLM summarizer can be injected). The system prompt and the most
    recent KEEP_RECENT groups are never evicted.

Tier 3 — emergency compression (>=90% fill):
    The digest is folded into the SYSTEM prompt and ALL older turn
    groups are hard-deleted, keeping only the last TIER3_KEEP_RECENT
    groups. Last stop before the gateway rejects the request.

Turn-group invariant (OpenAI tool-call protocol): an assistant message
carrying tool_calls and its following `tool` messages form ONE atomic
group — eviction removes whole groups, so no dangling tool_call ids or
orphaned tool results are ever produced.

Token counting is the same chars/4 heuristic the control plane uses for
prompt-composition budgets; it is an estimate, so tier thresholds carry
generous margins.
"""

from __future__ import annotations

import json
import os
from collections.abc import Sequence
from dataclasses import dataclass, field
from typing import Callable, Mapping

# --- Tunables (env-overridable) ------------------------------------------
ENV_CONTEXT_LIMIT = "SKQUAD_MODEL_CONTEXT_TOKENS"
DEFAULT_CONTEXT_LIMIT = 32768

TIER1_RATIO = 0.50
TIER2_RATIO = 0.75
TIER3_RATIO = 0.90

# Tier 1: tool results longer than this many chars get head+tail clipped.
TIER1_TOOL_RESULT_MAX_CHARS = 2000
TIER1_CLIP_HEAD = 1200
TIER1_CLIP_TAIL = 500

# Tier 2: how many recent turn groups are always kept verbatim.
TIER2_KEEP_RECENT = 5
# Tier 2 evicts this fraction of the eligible (older) groups each pass.
TIER2_EVICTION_FRACTION = 0.20

# Tier 3 keeps fewer recent groups (and folds the digest into the system prompt).
TIER3_KEEP_RECENT = 3

# Summarizer: takes the rendered transcript of evicted turns, returns a
# compact summary string. Deterministic digest is the default; the
# runtime may inject an LLM-backed summarizer.
Summarizer = Callable[[str], str]


@dataclass
class CompactionReport:
    tokens_before: int
    tokens_after: int
    tier: int = 0  # 0 = no compaction needed
    clipped_messages: int = 0
    evicted_turns: int = 0
    digest_chars: int = 0
    notes: list[str] = field(default_factory=list)

    @property
    def changed(self) -> bool:
        return self.tier > 0


def context_token_limit(environ: Mapping[str, str] | None = None) -> int:
    env = os.environ if environ is None else environ
    raw = env.get(ENV_CONTEXT_LIMIT, "").strip()
    if not raw:
        return DEFAULT_CONTEXT_LIMIT
    try:
        value = int(raw)
        return value if value > 0 else DEFAULT_CONTEXT_LIMIT
    except ValueError:
        return DEFAULT_CONTEXT_LIMIT


def estimate_tokens(text: str) -> int:
    # Same heuristic as the control-plane composer: ~4 chars per token.
    return (len(text) + 3) // 4


def estimate_messages_tokens(messages: Sequence[Mapping[str, object]]) -> int:
    total = 0
    for message in messages:
        content = message.get("content")
        if isinstance(content, str):
            total += estimate_tokens(content)
        elif content is not None:
            total += estimate_tokens(str(content))
        for key in ("tool_calls", "function_call"):
            payload = message.get(key)
            if payload:
                try:
                    total += estimate_tokens(json.dumps(payload, default=str))
                except Exception:  # noqa: BLE001
                    total += estimate_tokens(str(payload))
    return total


def _content_text(message: Mapping[str, object]) -> str:
    content = message.get("content")
    if isinstance(content, str):
        return content
    if content is None:
        return ""
    return str(content)


def _is_system(message: Mapping[str, object]) -> bool:
    return str(message.get("role") or "") == "system"


def _has_role(messages: object, role: str) -> bool:
    """True if any message (in a flat list or list of groups) has `role`."""
    for item in messages:  # type: ignore[union-attr]
        if isinstance(item, dict) and "role" in item:
            if str(item.get("role") or "") == role:
                return True
        elif isinstance(item, (list, tuple)):
            if _has_role(item, role):
                return True
    return False


def _has_tool_calls(message: Mapping[str, object]) -> bool:
    calls = message.get("tool_calls")
    return bool(calls)


def _clip(text: str) -> tuple[str, bool]:
    if len(text) <= TIER1_TOOL_RESULT_MAX_CHARS:
        return text, False
    head = text[:TIER1_CLIP_HEAD]
    tail = text[-TIER1_CLIP_TAIL:]
    dropped = len(text) - TIER1_CLIP_HEAD - TIER1_CLIP_TAIL
    return f"{head}\n[… {dropped} chars clipped …]\n{tail}", True


def group_turns(messages: list[dict[str, object]]) -> list[list[dict[str, object]]]:
    """Group messages into atomic turn groups.

    - A system message is its own (pinned) group.
    - An assistant message with tool_calls starts a group that ABSORBS
      the following `tool` messages (their responses) — the whole group
      must live or die together to keep tool_call ids paired.
    - Any other message (user, plain assistant) is its own group.
    """
    groups: list[list[dict[str, object]]] = []
    i = 0
    n = len(messages)
    while i < n:
        msg = messages[i]
        if _is_system(msg):
            groups.append([msg])
            i += 1
            continue
        if _has_tool_calls(msg):
            group = [msg]
            j = i + 1
            while j < n and str(messages[j].get("role") or "") == "tool":
                group.append(messages[j])
                j += 1
            groups.append(group)
            i = j
            continue
        groups.append([msg])
        i += 1
    return groups


class ContextCompactor:
    """Apply the S-161 tiered compaction to a message list."""

    def __init__(
        self,
        *,
        limit: int | None = None,
        summarizer: Summarizer | None = None,
        environ: Mapping[str, str] | None = None,
    ) -> None:
        self._limit = limit if limit is not None else context_token_limit(environ)
        self._summarizer = summarizer

    # -- public API ------------------------------------------------------
    def maybe_compact(
        self, messages: list[Mapping[str, object]]
    ) -> tuple[list[dict[str, object]], CompactionReport]:
        before = estimate_messages_tokens(messages)
        report = CompactionReport(tokens_before=before, tokens_after=before)
        ratio = before / self._limit if self._limit else 0.0
        if ratio < TIER1_RATIO:
            return [dict(m) for m in messages], report

        working: list[dict[str, object]] = [dict(m) for m in messages]
        if ratio < TIER2_RATIO:
            self._tier1_micro_prune(working, report)
        elif ratio < TIER3_RATIO:
            self._tier1_micro_prune(working, report)
            self._tier2_rolling_window(working, report)
        else:
            self._tier1_micro_prune(working, report)
            self._tier3_emergency(working, report)

        # Safety net (incident 2026-09-30): upstream chat templates (Qwen3
        # et al.) reject requests with no user-role message ("No user query
        # found in messages"). Compaction must never erase the user's query.
        if _has_role(messages, "user") and not _has_role(working, "user"):
            working.append(
                {
                    "role": "user",
                    "content": (
                        "<skquad_context trust=\"platform\">\n"
                        "Earlier turns were compacted; answer using the "
                        "context above.\n"
                        "</skquad_context>"
                    ),
                }
            )
            report.notes.append("guard: re-injected user-role message")

        report.tokens_after = estimate_messages_tokens(working)
        return working, report

    # -- tiers ---------------------------------------------------------
    def _tier1_micro_prune(
        self, messages: list[dict[str, object]], report: CompactionReport
    ) -> None:
        report.tier = max(report.tier, 1)
        for message in messages:
            if str(message.get("role") or "") != "tool":
                continue
            text = _content_text(message)
            clipped, did_clip = _clip(text)
            if did_clip:
                message["content"] = clipped
                report.clipped_messages += 1
            elif not text.strip():
                # Keep the message (tool_call pairing) but shrink it.
                message["content"] = "(empty result)"

    def _tier2_rolling_window(
        self, messages: list[dict[str, object]], report: CompactionReport
    ) -> None:
        report.tier = max(report.tier, 2)
        groups = group_turns(messages)
        system_groups = [g for g in groups if any(_is_system(m) for m in g)]
        non_system = [g for g in groups if g not in system_groups]
        if len(non_system) <= TIER2_KEEP_RECENT:
            report.notes.append("tier2: nothing eligible to evict")
            return
        older = non_system[:-TIER2_KEEP_RECENT]
        count = max(1, int(len(older) * TIER2_EVICTION_FRACTION))
        evicted = older[:count]
        kept_non_system = non_system[count:]
        report.evicted_turns = sum(len(g) for g in evicted)
        digest = self._digest([_render_group(g) for g in evicted])
        report.digest_chars = len(digest)
        digest_message = {
            "role": "user",
            "content": (
                "<skquad_context_digest trust=\"platform\">\n"
                "Earlier turns of this session were compacted into this digest.\n"
                f"{digest}\n"
                "</skquad_context_digest>"
            ),
        }
        # Rebuild: system groups first (order preserved), then digest,
        # then the kept non-system groups in order.
        rebuilt: list[dict[str, object]] = []
        for g in system_groups:
            rebuilt.extend(g)
        rebuilt.append(digest_message)
        for g in kept_non_system:
            rebuilt.extend(g)
        messages[:] = rebuilt

    def _tier3_emergency(
        self, messages: list[dict[str, object]], report: CompactionReport
    ) -> None:
        report.tier = max(report.tier, 3)
        groups = group_turns(messages)
        system_groups = [g for g in groups if any(_is_system(m) for m in g)]
        non_system = [g for g in groups if g not in system_groups]
        keep = non_system[-TIER3_KEEP_RECENT:] if non_system else []
        evicted = non_system[: len(non_system) - len(keep)] if keep else non_system
        digest = self._digest([_render_group(g) for g in evicted])
        report.evicted_turns = sum(len(g) for g in evicted)
        report.digest_chars = len(digest)
        # If the user's own turn(s) are all evicted, the digest MUST be a
        # user-role message: folding it into the system prompt would leave
        # the request with no user-role message at all, which upstream chat
        # templates reject outright ("No user query found in messages",
        # incident 2026-09-30 with tool-heavy chat on Qwen3).
        must_surface_user = _has_role(evicted, "user") and not _has_role(keep, "user")
        rebuilt: list[dict[str, object]] = []
        if system_groups and not must_surface_user:
            base = _content_text(system_groups[0][0])
            system_groups[0][0]["content"] = (
                f"{base}\n\n<skquad_context_digest trust=\"platform\">\n"
                f"Session history (emergency-compressed):\n{digest}\n"
                "</skquad_context_digest>"
            )
            rebuilt.extend(system_groups[0])
            for g in system_groups[1:]:
                rebuilt.extend(g)
        else:
            for g in system_groups:
                rebuilt.extend(g)
            rebuilt.append(
                {
                    "role": "user",
                    "content": (
                        "<skquad_context_digest trust=\"platform\">\n"
                        f"Session history (emergency-compressed):\n{digest}\n"
                        "</skquad_context_digest>"
                    ),
                }
            )
        for g in keep:
            rebuilt.extend(g)
        messages[:] = rebuilt

    # -- digest ----------------------------------------------------------
    def _digest(self, rendered_turns: Sequence[str]) -> str:
        transcript = "\n".join(rendered_turns)
        if self._summarizer is not None:
            try:
                summary = self._summarizer(transcript)
                if summary and summary.strip():
                    return summary.strip()
            except Exception:  # noqa: BLE001 — fall back to deterministic
                pass
        # Deterministic digest: one line per turn, capped width, plus a
        # total-size cap so the digest itself cannot balloon.
        lines = []
        budget = 2000
        for turn in rendered_turns:
            line = " ".join(turn.split())
            if len(line) > 160:
                line = line[:157] + "..."
            if budget - len(line) < 0:
                lines.append("[… earlier turns truncated …]")
                break
            lines.append(f"- {line}")
            budget -= len(line) + 3
        return "\n".join(lines) if lines else "(no earlier context)"


def _render_group(group: list[dict[str, object]]) -> str:
    return " | ".join(_render_turn(m) for m in group)


def _render_turn(message: Mapping[str, object]) -> str:
    role = str(message.get("role") or "?")
    text = " ".join(_content_text(message).split())
    if len(text) > 300:
        text = text[:297] + "..."
    calls = message.get("tool_calls")
    suffix = ""
    if calls:
        try:
            names = ", ".join(
                str((c.get("function") or {}).get("name", "?"))
                for c in calls
                if isinstance(c, Mapping)
            )
            suffix = f" [called: {names}]"
        except Exception:  # noqa: BLE001
            pass
    return f"{role}: {text}{suffix}"
