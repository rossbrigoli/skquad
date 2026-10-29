"""S-293: subagent task execution (docs/subagent-task-execution.md).

Moves the *execution* of a claimed task out of the agent's main loop into
a nested task-subagent, so a series of N tasks costs the parent chat ~N
result summaries instead of N full work transcripts.

This module owns the three pieces the spec names:

1. **Contracts** (:class:`TaskBrief`, :class:`SubTaskResult`) — the
   ``skquad.task_brief.v1`` / ``skquad.task_result.v1`` shapes. A
   malformed LLM-produced result never reaches the parent as success: it
   becomes a synthetic ``succeeded=false, failure_class=contract_violation``.
2. **Runner** (:func:`run_task_subagent`) — the S-160-style nested loop
   with the TASK turn profile (default 128 since S-298, vs the ad-hoc 12). The
   subagent starts from an empty context, inherits the composed system
   prompt / model / gateway / metering identity, and never sees
   ``board.*`` verbs or ``spawn_subagent`` (no recursion, no board
   writes — every board transition belongs to the parent).
3. **Parent review** (:func:`review_task_result`) — the decision tree
   that turns a TaskResult plus *workspace artifacts* into the parent's
   next move: self-approve, force review, rework (bounded), or block.

Invariants carried here (spec §3):

- **I1** — the lease/fencing token stays with the parent. The runner is
  never given the execution fence; it cannot complete or block anything.
- **I2** — empty context: only the rendered TaskBrief is in the
  subagent's context; the system prompt is inherited byte-identical.
- **I3** — only the TaskResult crosses back; the intermediate thread
  stays inside the runner (bounded, see the S-163 caps it reuses).
- **I4** — review is against artifacts, not prose: a claimed
  ``commit_sha`` that does not match the workspace HEAD forces
  ``review_needed=true`` and records a concern.

Kill switch: ``SKQUAD_TASK_SUBAGENT_ENABLED`` defaults to **false** —
the legacy in-loop execution path stays authoritative until this one has
survived a release cycle (spec §10).
"""

from __future__ import annotations

import json
import re
import threading
from collections import Counter
from dataclasses import dataclass, field, replace
from typing import TYPE_CHECKING, Callable, Mapping, Sequence

if TYPE_CHECKING:  # pragma: no cover — typing only, avoids the runtime cycle
    from .runtime import RuntimePlugin

from .subagents import (
    SUBAGENT_THREAD_ITEM_MAX_CHARS,
    SUBAGENT_THREAD_TOTAL_MAX_CHARS,
    SUBAGENT_TOOL_NAME,
    _cap_thread,
    _trim,
    _trim_args,
)

TASK_BRIEF_SCHEMA = "skquad.task_brief.v1"
TASK_RESULT_SCHEMA = "skquad.task_result.v1"

# S-301: live transparency is a replaceable SNAPSHOT of the captured
# thread ({"thread": [...], "turns": N, "steps": [...]}) published after
# each nested turn — not per-turn message rows. The chat task card's
# Details panel reads the snapshot; the thread caps below already bound
# the payload, so no separate line/char flood caps are needed.

ENV_TASK_SUBAGENT_ENABLED = "SKQUAD_TASK_SUBAGENT_ENABLED"
ENV_TASK_SUBAGENT_MAX_TURNS = "SKQUAD_TASK_SUBAGENT_MAX_TURNS"
ENV_TASK_REWORK_MAX = "SKQUAD_TASK_REWORK_MAX"

# S-298: platform-wide default raised 48 -> 128 (Ross, 2026-10-27). The
# per-agent override (agent Settings tab -> spec.taskSubagentMaxTurns ->
# SKQUAD_TASK_SUBAGENT_MAX_TURNS) wins when set; this constant is the
# last-resort default for pods that receive no env at all.
DEFAULT_TASK_SUBAGENT_MAX_TURNS = 128
DEFAULT_TASK_REWORK_MAX = 2

# Spec §4: the closed set of failure classes. Anything outside it is a
# contract violation, not a failure class — the parent must not build
# rework policy on a string it has never seen.
FAILURE_CLASSES = frozenset(
    {
        "contract_violation",
        "turn_budget",
        "tool_error",
        "dependency_missing",
        "access_denied",
        "aborted",
        "other",
    }
)

# Rework policy (spec §6/§9): only these classes are worth a second
# subagent on the same branch. ``turn_budget`` is deliberately NOT
# fixable-by-rework — the spec's own failure example makes it a human
# decision ("split the task or raise the budget"), and respawning another
# 48-turn run against the same overrun is how a task eats a whole wake.
FIXABLE_FAILURE_CLASSES = frozenset({"tool_error", "contract_violation", "other"})

PROPOSED_STATUSES = frozenset({"done", "in-review", "blocked"})

# Tools the task subagent must never hold, whatever the parent was granted.
# ``board.*`` is the parent's authority (ADR-0016 D1); spawn_subagent is
# the recursion guard (S-160).
FORBIDDEN_SUBAGENT_TOOL_PREFIXES = ("board.",)
FORBIDDEN_SUBAGENT_TOOLS = frozenset({SUBAGENT_TOOL_NAME})

# The result block the subagent is told to finish with.
RESULT_BLOCK_OPEN = "```task_result"
RESULT_BLOCK_CLOSE = "```"

KwargsFactory = Callable[[list, list], dict]


# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------


def env_flag(environ: Mapping[str, str] | None, name: str, default: bool) -> bool:
    env = _os_environ(environ)
    raw = env.get(name, "").strip().lower()
    if not raw:
        return default
    return raw not in {"0", "false", "no", "off"}


def env_int(environ: Mapping[str, str] | None, name: str, default: int) -> int:
    env = _os_environ(environ)
    raw = env.get(name, "").strip()
    if not raw:
        return default
    try:
        return max(1, int(raw))
    except ValueError:
        return default


def _os_environ(environ: Mapping[str, str] | None) -> Mapping[str, str]:
    import os

    return os.environ if environ is None else environ


def task_subagent_enabled(environ: Mapping[str, str] | None = None) -> bool:
    """Master switch. **Default false** (spec §7/§10): the legacy
    in-loop execution path stays the default until this one is proven."""
    return env_flag(environ, ENV_TASK_SUBAGENT_ENABLED, False)


def task_subagent_max_turns(environ: Mapping[str, str] | None = None) -> int:
    """TASK profile turn budget — distinct from the ad-hoc digging
    profile's ``SKQUAD_SUBAGENT_MAX_TURNS`` (12)."""
    return env_int(environ, ENV_TASK_SUBAGENT_MAX_TURNS, DEFAULT_TASK_SUBAGENT_MAX_TURNS)


def task_rework_max(environ: Mapping[str, str] | None = None) -> int:
    """Max rework respawns per task attempt cycle (spec §6)."""
    return env_int(environ, ENV_TASK_REWORK_MAX, DEFAULT_TASK_REWORK_MAX)


# --------------------------------------------------------------------------
# TaskBrief (parent -> subagent), spec §5
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class TaskBrief:
    """The complete, standalone input for one task-subagent run.

    ``reviewer_feedback`` is the ADR-0016 D3 hook: chat-originated
    reviewer text is carried **verbatim** (never paraphrased) so the
    subagent works against what the reviewer actually said."""

    task_id: str
    title: str
    description: str = ""
    task_number: int = 0
    acceptance_criteria: tuple[str, ...] = ()
    workspace: Mapping[str, object] = field(default_factory=dict)
    reviewer_feedback: str = ""
    attempt: int = 1
    prior_result: Mapping[str, object] | None = None
    fix_instructions: tuple[str, ...] = ()
    tests_required: bool = True

    def to_payload(self) -> dict[str, object]:
        payload: dict[str, object] = {
            "schema": TASK_BRIEF_SCHEMA,
            "task_id": self.task_id,
            "task_number": self.task_number,
            "title": self.title,
            "description": self.description,
            "acceptance_criteria": list(self.acceptance_criteria),
            "workspace": dict(self.workspace),
            "attempt": self.attempt,
        }
        if self.reviewer_feedback:
            payload["reviewer_feedback"] = self.reviewer_feedback
        if self.prior_result is not None:
            payload["prior_result"] = self.prior_result
        if self.fix_instructions:
            payload["fix_instructions"] = list(self.fix_instructions)
        return payload

    def render_prompt(self) -> str:
        """The subagent's first (and only) user message.

        The brief is rendered as the v1 JSON payload plus the terminal
        contract, so the model has the shape it must answer with in the
        same message it is working from."""
        parts = [
            "You are executing one task as a task-subagent. You have no "
            "memory of any conversation — everything you need is below.",
            "",
            "TASK BRIEF:",
            json.dumps(self.to_payload(), indent=2, default=str),
            "",
            "Do the work with the tools you have. When you finish (or "
            "determine you cannot finish), end your reply with a "
            f"{RESULT_BLOCK_OPEN} block containing ONLY a single JSON "
            f"object with \"schema\": \"{TASK_RESULT_SCHEMA}\".",
            "Required fields: schema, succeeded, summary, "
            "proposed_status, review_recommended, concerns, turns_used.",
            "If succeeded is false you MUST also set failure_class to one "
            "of: " + ", ".join(sorted(FAILURE_CLASSES)) + ".",
            "If you ran tests, report them in `tests` "
            "(run/command/passed/failed/skipped).",
            "Report the commit SHA and branch of your work if the task ran "
            "against a git workspace — the parent verifies them against "
            "the real workspace.",
            "You cannot change any board state: propose a status, and the "
            "assignee agent makes the transition.",
        ]
        if self.attempt > 1 and self.fix_instructions:
            parts.extend(
                [
                    "",
                    "This is a REWORK attempt. Continue on the existing "
                    "branch; do not restart from scratch. Fix these:",
                    *[f"- {item}" for item in self.fix_instructions],
                ]
            )
        return "\n".join(parts)


# --------------------------------------------------------------------------
# TaskResult (subagent -> parent), spec §4
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class SubTaskResult:
    """The only thing that crosses back from the subagent (I3)."""

    succeeded: bool
    summary: str = ""
    failure_class: str = ""
    changed_files: tuple[str, ...] = ()
    commit_sha: str = ""
    branch: str = ""
    tests: Mapping[str, object] = field(default_factory=dict)
    proposed_status: str = "in-review"
    review_recommended: bool = True
    review_notes: str = ""
    concerns: tuple[str, ...] = ()
    turns_used: int = 0
    artifacts: tuple[Mapping[str, object], ...] = ()
    raw: str = ""

    def to_payload(self) -> dict[str, object]:
        payload: dict[str, object] = {
            "schema": TASK_RESULT_SCHEMA,
            "succeeded": self.succeeded,
            "summary": self.summary,
            "changed_files": list(self.changed_files),
            "commit_sha": self.commit_sha,
            "branch": self.branch,
            "tests": dict(self.tests),
            "proposed_status": self.proposed_status,
            "review_recommended": self.review_recommended,
            "review_notes": self.review_notes,
            "concerns": list(self.concerns),
            "turns_used": self.turns_used,
            "artifacts": [dict(a) for a in self.artifacts],
        }
        if self.failure_class:
            payload["failure_class"] = self.failure_class
        return payload

    @property
    def tests_passed(self) -> int:
        return _coerce_int(self.tests.get("passed"))

    @property
    def tests_failed(self) -> int:
        return _coerce_int(self.tests.get("failed"))

    @property
    def tests_ran(self) -> bool:
        return bool(self.tests.get("run"))


def contract_violation(detail: str, *, turns_used: int = 0, raw: str = "") -> SubTaskResult:
    """Synthetic failed result for a shape the contract cannot accept."""
    return SubTaskResult(
        succeeded=False,
        summary=f"Task subagent returned an invalid result: {detail}",
        failure_class="contract_violation",
        proposed_status="blocked",
        review_recommended=True,
        concerns=(f"contract_violation: {detail}",),
        turns_used=turns_used,
        raw=raw,
    )


def parse_task_result(text: str, *, turns_used: int = 0) -> SubTaskResult:
    """Parse the subagent's terminal result block.

    Any shape problem returns a synthetic ``contract_violation`` — never
    a partially-trusted result, and never a success."""
    payload, err = extract_result_json(text)
    if err:
        return contract_violation(err, turns_used=turns_used, raw=text)
    return validate_task_result(payload, turns_used=turns_used, raw=text)


def extract_result_json(text: str) -> tuple[Mapping[str, object], str]:
    """Pull the result JSON out of the model's final message.

    Accepts the fenced ```` ```task_result ```` block, a fenced ```json
    block, or a bare top-level JSON object (models frequently drop the
    custom fence). Returns ``(payload, error)``."""
    if not text or not text.strip():
        return {}, "empty final message (no result block)"
    for candidate in _result_block_candidates(text):
        try:
            loaded = json.loads(candidate)
        except (ValueError, TypeError):
            continue
        if isinstance(loaded, dict):
            return loaded, ""
    return {}, "no valid JSON result block found in the final message"


def _result_block_candidates(text: str) -> list[str]:
    candidates: list[str] = []
    for fence in (RESULT_BLOCK_OPEN, "```json", "```"):
        candidates.extend(re.findall(re.escape(fence) + r"\s*(.*?)\s*" + re.escape(RESULT_BLOCK_CLOSE), text, re.DOTALL))
    stripped = text.strip()
    candidates.append(stripped)
    # Last resort: the outermost {...} span, so trailing prose after a
    # bare-object answer does not force a violation.
    start = stripped.find("{")
    end = stripped.rfind("}")
    if start != -1 and end > start:
        candidates.append(stripped[start : end + 1])
    return candidates


def _coerce_object_field(payload: Mapping[str, object], key: str) -> "dict | None":
    """Return a dict field, {} when absent, or None when present but not an object."""
    value = payload.get(key)
    if value is None:
        return {}
    if not isinstance(value, dict):
        return None
    return value


def _coerce_array_field(payload: Mapping[str, object], key: str) -> "list | None":
    """Return a list field, [] when absent, or None when present but not an array."""
    value = payload.get(key)
    if value is None:
        return []
    if not isinstance(value, list):
        return None
    return value


def _result_contract_error(payload: Mapping[str, object]) -> str | None:
    """Field-level v1 contract checks. Returns an error message, or None if valid.

    Behavior-identical to the former inline checks in validate_task_result (S3776).
    """
    schema = str(payload.get("schema") or "")
    if schema != TASK_RESULT_SCHEMA:
        return f"schema must be {TASK_RESULT_SCHEMA!r}, got {schema!r}"
    if not isinstance(payload.get("succeeded"), bool):
        return "`succeeded` must be a boolean"

    succeeded = bool(payload.get("succeeded"))
    summary = str(payload.get("summary") or "").strip()
    failure_class = str(payload.get("failure_class") or "").strip()
    proposed_status = str(payload.get("proposed_status") or "").strip()

    if not summary:
        return "`summary` is required"
    if proposed_status not in PROPOSED_STATUSES:
        return f"`proposed_status` must be one of {sorted(PROPOSED_STATUSES)}, got {proposed_status!r}"
    if not succeeded and failure_class not in FAILURE_CLASSES:
        return (
            f"a failed result requires `failure_class` in {sorted(FAILURE_CLASSES)}, "
            f"got {failure_class!r}"
        )
    if _coerce_object_field(payload, "tests") is None:
        return "`tests` must be an object"
    if _coerce_array_field(payload, "artifacts") is None:
        return "`artifacts` must be an array"
    return None


def validate_task_result(
    payload: Mapping[str, object], *, turns_used: int = 0, raw: str = ""
) -> SubTaskResult:
    """Validate the v1 contract. See spec §4 field rules."""
    err = _result_contract_error(payload)
    if err:
        return contract_violation(err, turns_used=turns_used, raw=raw)

    succeeded = bool(payload.get("succeeded"))
    changed_files = _coerce_str_tuple(payload.get("changed_files"))
    concerns = _coerce_str_tuple(payload.get("concerns"))
    tests = _coerce_object_field(payload, "tests") or {}
    artifacts = _coerce_array_field(payload, "artifacts") or []

    return SubTaskResult(
        succeeded=succeeded,
        summary=str(payload.get("summary") or "").strip(),
        failure_class=str(payload.get("failure_class") or "").strip() if succeeded is False else "",
        changed_files=changed_files,
        commit_sha=str(payload.get("commit_sha") or "").strip(),
        branch=str(payload.get("branch") or "").strip(),
        tests=dict(tests),
        proposed_status=str(payload.get("proposed_status") or "").strip(),
        review_recommended=bool(payload.get("review_recommended", True)),
        review_notes=str(payload.get("review_notes") or ""),
        concerns=concerns,
        turns_used=_coerce_int(payload.get("turns_used")) or turns_used,
        artifacts=tuple(a for a in artifacts if isinstance(a, dict)),
        raw=raw,
    )


def _coerce_int(value: object) -> int:
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, int):
        return value
    try:
        return int(str(value).strip())
    except (TypeError, ValueError):
        return 0


def _coerce_str_tuple(value: object) -> tuple[str, ...]:
    if isinstance(value, (list, tuple)):
        return tuple(str(v) for v in value)
    return ()


# --------------------------------------------------------------------------
# Tool filtering (spec §3 I2: no board writes, no recursion)
# --------------------------------------------------------------------------


def forbidden_subagent_tool(name: str) -> bool:
    if not name:
        return True
    if name in FORBIDDEN_SUBAGENT_TOOLS:
        return True
    return any(name.startswith(prefix) for prefix in FORBIDDEN_SUBAGENT_PREFIXES)


FORBIDDEN_SUBAGENT_PREFIXES = FORBIDDEN_SUBAGENT_TOOL_PREFIXES


def filter_subagent_tools(tools: Sequence[object]) -> list[object]:
    """Drop every tool the task subagent must not hold.

    Defence in depth: the caller composes a grant-filtered list, but a
    ``board.*`` verb that reached the parent (S-292) must still never
    reach the task subagent — the subagent proposing a transition is
    the whole point of the parent-review model."""
    kept: list[object] = []
    for schema in tools:
        name = _schema_tool_name(schema)
        if forbidden_subagent_tool(name):
            continue
        kept.append(schema)
    return kept


def _schema_tool_name(schema: object) -> str:
    if isinstance(schema, Mapping):
        fn = schema.get("function")
        if isinstance(fn, Mapping):
            return str(fn.get("name") or "")
        return str(schema.get("name") or "")
    return ""


# --------------------------------------------------------------------------
# Runner (spec §3: nested bounded loop under the parent's lease)
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class SubagentRun:
    """Runner output: the parsed result plus the bounded transparency thread."""

    result: SubTaskResult
    thread: tuple[Mapping[str, object], ...] = ()
    steps: tuple[str, ...] = ()


def _emit_snapshot(
    progress: "Callable[[Mapping[str, object]], None] | None",
    thread: "list[dict[str, object]]",
    turns: int,
    steps: "list[str]",
) -> None:
    """S-301: publish the observational snapshot; a failed publish is swallowed."""
    if progress is None:
        return
    try:
        # list() copy: _cap_thread returns the LIVE list when it
        # fits — without the copy every "snapshot" would alias the
        # growing thread and show its final state.
        progress(
            {
                "thread": list(_cap_thread(thread)),
                "turns": turns,
                "steps": list(steps),
            }
        )
    except Exception:  # noqa: BLE001 — never break the run
        pass


def _run_child_tool_round(
    calls: Sequence[object],
    messages: "list[dict[str, object]]",
    thread: "list[dict[str, object]]",
    steps: "list[str]",
) -> None:
    """Execute one round of the subagent's tool calls, appending results."""
    from .runtime import invoke_plugin_tool, wrap_untrusted

    for call in calls:
        # Belt and braces: a forbidden tool cannot be executed even if
        # it somehow appears in a model response.
        if forbidden_subagent_tool(call.name):  # type: ignore[attr-defined]
            tool_content = f"tool {call.name!r} is not available to task subagents"  # type: ignore[attr-defined]
            ok = False
        else:
            tool_result = invoke_plugin_tool(call, None, _runner_plugins())  # type: ignore[arg-type]
            tool_content = tool_result.content
            ok = tool_result.ok
        steps.append(call.name)  # type: ignore[attr-defined]
        thread.append(
            {
                "role": "tool",
                "name": call.name,  # type: ignore[attr-defined]
                "ok": ok,
                "content": _trim(tool_content, SUBAGENT_THREAD_ITEM_MAX_CHARS),
            }
        )
        messages.append(
            {
                "role": "tool",
                "tool_call_id": call.id,  # type: ignore[attr-defined]
                "name": call.name,  # type: ignore[attr-defined]
                "content": wrap_untrusted(
                    tool_content, "tool_result", tool=call.name, subagent="true"  # type: ignore[attr-defined]
                ),
            }
        )


def run_task_subagent(
    brief: TaskBrief,
    *,
    system_prompt: str,
    completion: Callable[..., object],
    tools: Sequence[object],
    kwargs_factory: KwargsFactory,
    origin: str,
    max_turns: int | None = None,
    environ: Mapping[str, str] | None = None,
    progress: Callable[[Mapping[str, object]], None] | None = None,
) -> SubagentRun:
    """Run one task-subagent to a TaskResult.

    Never raises for a subagent-side failure: an LLM error, a turn-budget
    overrun or a garbage final message all come back as a structured
    failed result so the parent's review can classify them (spec §9).
    """
    from .runtime import (
        assistant_message,
        first_message,
        invoke_plugin_tool,
        message_value,
        parse_tool_calls,
        wrap_untrusted,
    )

    budget = max_turns if max_turns is not None else task_subagent_max_turns(environ)
    child_tools = filter_subagent_tools(tools)
    prompt = brief.render_prompt()

    messages: list[dict[str, object]] = [
        {"role": "system", "content": system_prompt},
        {
            "role": "user",
            "content": wrap_untrusted(prompt, "task_brief", origin=origin),
        },
    ]
    thread: list[dict[str, object]] = [
        {"role": "user", "content": _trim(prompt, SUBAGENT_THREAD_ITEM_MAX_CHARS)}
    ]
    steps: list[str] = []
    turns = 0
    last_content = ""

    while turns < budget:
        turns += 1
        kwargs = kwargs_factory(messages, child_tools)
        _mark_task_subagent_metadata(kwargs, brief.task_id)
        try:
            response = completion(**kwargs)
        except Exception as exc:  # noqa: BLE001 — a subagent failure is data
            _emit_snapshot(progress, thread, turns, steps)
            return SubagentRun(
                result=SubTaskResult(
                    succeeded=False,
                    summary=f"Task subagent LLM call failed: {exc}",
                    failure_class="other",
                    proposed_status="blocked",
                    review_recommended=True,
                    concerns=(f"subagent llm error: {exc}",),
                    turns_used=turns,
                ),
                thread=tuple(_cap_thread(thread)),
                steps=tuple(steps),
            )

        message = first_message(response)
        content = str(message_value(message, "content") or "")
        if content.strip():
            last_content = content
        calls = parse_tool_calls(message)
        if not calls:
            result = parse_task_result(content, turns_used=turns)
            # The runner's count is authoritative for the §8 metric — a
            # subagent under-reporting its own turns must not flatter the
            # cost/turn rollups.
            result = replace(result, turns_used=turns)
            thread.append({"role": "assistant", "content": _trim(content, SUBAGENT_THREAD_ITEM_MAX_CHARS)})
            # S-301: final snapshot carries the complete thread.
            _emit_snapshot(progress, thread, turns, steps)
            return SubagentRun(
                result=result, thread=tuple(_cap_thread(thread)), steps=tuple(steps)
            )

        messages.append(assistant_message(content, calls))
        thread.append(
            {
                "role": "assistant",
                "content": _trim(content, SUBAGENT_THREAD_ITEM_MAX_CHARS),
                "tool_calls": [
                    {"name": c.name, "arguments": _trim_args(c.arguments)} for c in calls
                ],
            }
        )
        _run_child_tool_round(calls, messages, thread, steps)
        # S-301: one snapshot per completed turn — the Details panel
        # polls the replaceable view instead of a message flood.
        _emit_snapshot(progress, thread, turns, steps)

    # Turn budget exhausted (spec §4 failure example, §9). The last
    # completed turn already emitted the current snapshot; the failed
    # result itself reaches the parent through the return value.
    return SubagentRun(
        result=SubTaskResult(
            succeeded=False,
            summary=_trim(
                f"Task subagent exhausted its {budget}-turn budget without producing a "
                f"final result. Last output:\n{last_content}",
                4000,
            ),
            failure_class="turn_budget",
            proposed_status="blocked",
            review_recommended=True,
            concerns=["turn budget exhausted; work may be half-finished on the branch"],
            turns_used=turns,
        ),
        thread=tuple(_cap_thread(thread)),
        steps=tuple(steps),
    )


_RUNNER_PLUGINS: list = []


def _runner_plugins() -> "list[RuntimePlugin]":
    """The plugin set the runner executes child tool calls against.

    Set once by the runtime at wake (see
    ``LiteLLMTaskHandler._setup_task_subagent_runner``) so the runner
    stays free of bootstrap wiring — the same shape ``SubagentPlugin``
    captures at construction, but shared rather than per-spawn."""
    return _RUNNER_PLUGINS


def configure_runner_plugins(plugins: Sequence[object]) -> None:
    global _RUNNER_PLUGINS
    _RUNNER_PLUGINS = [p for p in plugins if getattr(p, "name", None) != "spawn_subagent"]


def _mark_task_subagent_metadata(kwargs: dict, task_id: str) -> None:
    """Spec §8: every subagent LLM call carries the subagent marker AND
    the task id so per-task cost rollups line up with S-269 metering."""
    extra_body = kwargs.get("extra_body")
    if not isinstance(extra_body, dict):
        extra_body = {}
        kwargs["extra_body"] = extra_body
    metadata = extra_body.get("litellm_metadata")
    if not isinstance(metadata, dict):
        metadata = {}
        extra_body["litellm_metadata"] = metadata
    metadata["skquad_subagent"] = True
    if task_id:
        metadata["skquad_task_id"] = task_id


# --------------------------------------------------------------------------
# Parent review (spec §3 I4, §6)
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class ReviewDecision:
    """The parent's move after reviewing a TaskResult.

    ``action`` is ``complete`` | ``rework`` | ``block``. For
    ``complete`` the parent calls the board transition with
    ``status``/``review_needed``/``justification`` — the subagent never
    does (I1)."""

    action: str
    status: str = "in-review"
    review_needed: bool = True
    justification: str = ""
    summary: str = ""
    concerns: tuple[str, ...] = ()
    fix_instructions: tuple[str, ...] = ()
    forced_review: bool = False


def sha_matches(claimed: str, actual: str) -> bool:
    """Git SHAs are commonly reported short; match on prefix either way."""
    claimed = (claimed or "").strip()
    actual = (actual or "").strip()
    if not claimed or not actual:
        return False
    return actual.startswith(claimed) or claimed.startswith(actual)


def review_task_result(
    result: SubTaskResult,
    *,
    has_workspace: bool,
    workspace_sha: str = "",
    workspace_dirty: bool = False,
    reworks_used: int = 0,
    rework_max: int = DEFAULT_TASK_REWORK_MAX,
) -> ReviewDecision:
    """Turn a TaskResult + workspace facts into the parent's decision.

    The workspace SHA and dirty state are read by the PARENT from the
    real workspace (I4) — neither is something the subagent can assert
    its way past."""
    concerns = list(result.concerns)

    if not result.succeeded:
        if result.failure_class in FIXABLE_FAILURE_CLASSES and reworks_used < rework_max:
            return ReviewDecision(
                action="rework",
                concerns=tuple(concerns),
                fix_instructions=_fix_instructions(result),
                summary=result.summary,
            )
        return ReviewDecision(
            action="block",
            summary=result.summary,
            concerns=tuple(concerns),
        )

    # I4: cross-check the claimed commit against the actual workspace.
    forced_review = False
    if has_workspace:
        if not result.commit_sha:
            forced_review = True
            concerns.append(
                "subagent reported success but no commit_sha; the work is "
                "unverified against the workspace"
            )
        elif workspace_sha and not sha_matches(result.commit_sha, workspace_sha):
            forced_review = True
            concerns.append(
                f"claimed commit {result.commit_sha!r} does not match workspace HEAD "
                f"{workspace_sha!r}"
            )
        if workspace_dirty:
            # The finalize step would sweep these into a commit the
            # subagent never described, so the claimed SHA does not
            # cover all the work — never self-approve that.
            forced_review = True
            concerns.append(
                "workspace still has uncommitted changes; the claimed commit "
                "does not cover all the work"
            )

    if result.review_recommended:
        # Never downgrade a subagent's own "needs eyes" signal.
        return ReviewDecision(
            action="complete",
            status="in-review",
            review_needed=True,
            justification="",
            summary=_completion_summary(result, concerns, review=True),
            concerns=tuple(concerns),
        )
    if forced_review:
        return ReviewDecision(
            action="complete",
            status="in-review",
            review_needed=True,
            justification="",
            summary=_completion_summary(result, concerns, review=True),
            concerns=tuple(concerns),
            forced_review=True,
        )
    if result.proposed_status != "done":
        # The subagent proposed in-review/blocked while claiming success and
        # not recommending review — take the conservative reading.
        return ReviewDecision(
            action="complete",
            status="in-review",
            review_needed=True,
            justification="",
            summary=_completion_summary(result, concerns, review=True),
            concerns=tuple(concerns),
        )

    return ReviewDecision(
        action="complete",
        status="done",
        review_needed=False,
        justification=_justification(result, workspace_sha),
        summary=_completion_summary(result, concerns, review=False),
        concerns=tuple(concerns),
    )


def _fix_instructions(result: SubTaskResult) -> tuple[str, ...]:
    parts = [f"Prior attempt failed ({result.failure_class}): {result.summary}"]
    parts.extend(result.concerns)
    parts.extend(f"Files touched by the prior attempt: {name}" for name in result.changed_files[:20])
    return tuple(parts)


def _justification(result: SubTaskResult, workspace_sha: str) -> str:
    if result.tests_ran:
        test_bit = (
            f"tests: {result.tests_passed} passed / {result.tests_failed} failed "
            f"({result.tests.get('command', 'command not reported')})"
        )
    else:
        test_bit = "no tests reported"
    sha_bit = (
        f"commit {workspace_sha[:12]} verified in workspace"
        if workspace_sha
        else "no git workspace involved"
    )
    files_bit = f"{len(result.changed_files)} file(s) changed"
    notes = f" {result.review_notes.strip()}" if result.review_notes.strip() else ""
    return f"Self-approved after parent review: {test_bit}; {sha_bit}; {files_bit}.{notes}"


# --------------------------------------------------------------------------
# Metrics (spec §8) — process-local counters rendered by
# ``runtime_metrics_text``. Outcome/failure_class are the labels the
# dashboards group by; the turn-histogram input is kept as a running
# sum+count so the exposition stays dependency-free.
# --------------------------------------------------------------------------

_METRICS_LOCK = threading.Lock()
_METRICS: dict[str, object] = {
    "runs": Counter(),
    "reworks": 0,
    "turns_total": 0,
    "turns_count": 0,
}


def record_task_subagent_run(
    *, outcome: str, failure_class: str = "", turns_used: int = 0
) -> None:
    with _METRICS_LOCK:
        runs = _METRICS["runs"]
        assert isinstance(runs, Counter)
        runs[(outcome, failure_class)] += 1
        if turns_used > 0:
            _METRICS["turns_total"] = int(_METRICS["turns_total"]) + turns_used
            _METRICS["turns_count"] = int(_METRICS["turns_count"]) + 1


def note_rework() -> None:
    with _METRICS_LOCK:
        _METRICS["reworks"] = int(_METRICS["reworks"]) + 1


def task_subagent_metrics() -> dict[str, object]:
    with _METRICS_LOCK:
        runs = _METRICS["runs"]
        assert isinstance(runs, Counter)
        return {
            "runs": {f"{outcome}|{fc}": n for (outcome, fc), n in runs.items()},
            "reworks": int(_METRICS["reworks"]),
            "turns_total": int(_METRICS["turns_total"]),
            "turns_count": int(_METRICS["turns_count"]),
        }


def reset_task_subagent_metrics() -> None:
    with _METRICS_LOCK:
        _METRICS["runs"] = Counter()
        _METRICS["reworks"] = 0
        _METRICS["turns_total"] = 0
        _METRICS["turns_count"] = 0


def _completion_summary(
    result: SubTaskResult, concerns: Sequence[str], *, review: bool
) -> str:
    """The text the parent posts with the transition. The subagent's
    review notes and concerns ride along verbatim (spec §4) so a human
    reading the task sees what the subagent actually said, not just the
    headline."""
    blocks = [result.summary]
    notes = (result.review_notes or "").strip()
    if review and notes:
        blocks.append(f"Subagent review notes: {notes}")
    if concerns:
        label = "Review notes" if review else "Notes"
        blocks.append(label + ":\n" + "\n".join(f"- {c}" for c in concerns))
    return "\n\n".join(b for b in blocks if b)
