"""S-293: subagent task execution — contracts, runner and parent review.

No live gateway: the nested loop runs against fake completions and fake
granted plugins, the same way the S-160 subagent tests do.

Coverage: config defaults/overrides, TaskBrief rendering (verbatim
reviewer feedback), TaskResult parsing and every contract-violation
path, tool exclusion (board.* / spawn_subagent), turn-budget and LLM
failure classification, metering markers, and the full parent-review
decision tree including the I4 commit cross-check and the bounded
rework cycle.
"""

import json
import unittest

from skquad_runtime import task_subagent as ts
from skquad_runtime.runtime import ToolResult

GOOD_RESULT = {
    "schema": "skquad.task_result.v1",
    "succeeded": True,
    "summary": "Added rate-limit middleware; 14 tests pass.",
    "changed_files": ["server/middleware/ratelimit.go", "server/middleware/ratelimit_test.go"],
    "commit_sha": "abc1234def5678",
    "branch": "feat/s300-ratelimit",
    "tests": {"run": True, "command": "go test ./...", "passed": 14, "failed": 0, "skipped": 0},
    "proposed_status": "done",
    "review_recommended": False,
    "review_notes": "Mechanical change, fully covered by tests.",
    "concerns": [],
    "turns_used": 11,
    "artifacts": [{"kind": "log", "name": "test-output", "truncated": True}],
}


def fenced(payload: dict) -> str:
    return "Working done.\n```task_result\n" + json.dumps(payload) + "\n```"


class ConfigTest(unittest.TestCase):
    def test_disabled_by_default(self):
        # Spec §10: the legacy in-loop path stays the default.
        self.assertFalse(ts.task_subagent_enabled({}))
        self.assertFalse(ts.task_subagent_enabled({"SKQUAD_TASK_SUBAGENT_ENABLED": "0"}))
        self.assertTrue(ts.task_subagent_enabled({"SKQUAD_TASK_SUBAGENT_ENABLED": "true"}))

    def test_turn_and_rework_defaults_and_overrides(self):
        # S-298: platform-wide default raised 48 -> 128 (Ross, 2026-10-27).
        self.assertEqual(ts.task_subagent_max_turns({}), 128)
        self.assertEqual(ts.task_rework_max({}), 2)
        self.assertEqual(ts.task_subagent_max_turns({"SKQUAD_TASK_SUBAGENT_MAX_TURNS": "9"}), 9)
        self.assertEqual(ts.task_rework_max({"SKQUAD_TASK_REWORK_MAX": "1"}), 1)
        # garbage falls back rather than crashing the wake
        self.assertEqual(ts.task_subagent_max_turns({"SKQUAD_TASK_SUBAGENT_MAX_TURNS": "many"}), 128)


class TaskBriefTest(unittest.TestCase):
    def test_payload_is_v1_shaped(self):
        brief = ts.TaskBrief(
            task_id="t1",
            task_number=301,
            title="Rate limit",
            description="Add per-IP limiting",
            acceptance_criteria=["429 after 100 rpm"],
            workspace={"resource_id": "repo-1", "branch_hint": "feat/s301-x"},
        )
        payload = brief.to_payload()
        self.assertEqual(payload["schema"], "skquad.task_brief.v1")
        self.assertEqual(payload["task_number"], 301)
        self.assertEqual(payload["acceptance_criteria"], ["429 after 100 rpm"])
        self.assertNotIn("reviewer_feedback", payload)

    def test_reviewer_feedback_carried_verbatim(self):
        # ADR-0016 D3: nothing lost in paraphrase.
        feedback = "Ross: rate limit must be per-IP, not global."
        brief = ts.TaskBrief(task_id="t1", title="x", reviewer_feedback=feedback)
        self.assertIn(feedback, brief.to_payload()["reviewer_feedback"])
        self.assertIn(feedback, brief.render_prompt())

    def test_render_prompt_states_result_contract(self):
        prompt = ts.TaskBrief(task_id="t1", title="x").render_prompt()
        self.assertIn("skquad.task_result.v1", prompt)
        self.assertIn("contract_violation", prompt)
        self.assertIn("cannot change any board state", prompt)

    def test_rework_attempt_includes_fix_instructions(self):
        brief = ts.TaskBrief(
            task_id="t1",
            title="x",
            attempt=2,
            prior_result={"succeeded": False},
            fix_instructions=["add the regression test"],
        )
        rendered = brief.render_prompt()
        self.assertIn("REWORK attempt", rendered)
        self.assertIn("add the regression test", rendered)


class TaskResultParseTest(unittest.TestCase):
    def test_parses_fenced_block(self):
        result = ts.parse_task_result(fenced(GOOD_RESULT))
        self.assertTrue(result.succeeded)
        self.assertEqual(result.commit_sha, "abc1234def5678")
        self.assertEqual(result.tests_passed, 14)
        self.assertTrue(result.tests_ran)

    def test_parses_bare_json_object(self):
        result = ts.parse_task_result(json.dumps(GOOD_RESULT))
        self.assertTrue(result.succeeded)

    def test_parses_json_with_trailing_prose(self):
        result = ts.parse_task_result(json.dumps(GOOD_RESULT) + "\nDone!")
        self.assertTrue(result.succeeded)

    def test_empty_message_is_contract_violation(self):
        result = ts.parse_task_result("")
        self.assertFalse(result.succeeded)
        self.assertEqual(result.failure_class, "contract_violation")

    def test_non_json_is_contract_violation(self):
        result = ts.parse_task_result("I finished the task, it works great")
        self.assertFalse(result.succeeded)
        self.assertEqual(result.failure_class, "contract_violation")
        self.assertIn("contract_violation", result.concerns[0])

    def test_wrong_schema_is_contract_violation(self):
        payload = dict(GOOD_RESULT, schema="skquad.task_result.v0")
        result = ts.parse_task_result(fenced(payload))
        self.assertEqual(result.failure_class, "contract_violation")

    def test_succeeded_must_be_boolean(self):
        payload = dict(GOOD_RESULT, succeeded="yes")
        result = ts.parse_task_result(fenced(payload))
        self.assertEqual(result.failure_class, "contract_violation")

    def test_missing_summary_is_violation(self):
        payload = {k: v for k, v in GOOD_RESULT.items() if k != "summary"}
        result = ts.parse_task_result(fenced(payload))
        self.assertEqual(result.failure_class, "contract_violation")

    def test_bad_proposed_status_is_violation(self):
        payload = dict(GOOD_RESULT, proposed_status="ship-it")
        result = ts.parse_task_result(fenced(payload))
        self.assertEqual(result.failure_class, "contract_violation")

    def test_failed_result_requires_known_failure_class(self):
        base = dict(
            GOOD_RESULT,
            succeeded=False,
            summary="ran out of turns",
            proposed_status="blocked",
        )
        self.assertEqual(
            ts.parse_task_result(fenced(base)).failure_class, "contract_violation"
        )
        ok = ts.parse_task_result(fenced(dict(base, failure_class="turn_budget")))
        self.assertFalse(ok.succeeded)
        self.assertEqual(ok.failure_class, "turn_budget")

    def test_turns_used_falls_back_to_runner_count(self):
        payload = {k: v for k, v in GOOD_RESULT.items() if k != "turns_used"}
        result = ts.parse_task_result(fenced(payload), turns_used=7)
        self.assertEqual(result.turns_used, 7)


class ToolExclusionTest(unittest.TestCase):
    def test_board_verbs_and_spawn_are_dropped(self):
        tools = [
            {"function": {"name": "exec"}},
            {"function": {"name": "board.claim"}},
            {"function": {"name": "board.complete"}},
            {"function": {"name": "board.block"}},
            {"function": {"name": "spawn_subagent"}},
            {"type": "function", "function": {"name": "read_file"}},
        ]
        kept = [ts.__dict__["_schema_tool_name"](t) for t in ts.filter_subagent_tools(tools)]
        self.assertEqual(kept, ["exec", "read_file"])

    def test_unknown_and_empty_names_are_refused(self):
        self.assertTrue(ts.forbidden_subagent_tool(""))
        self.assertTrue(ts.forbidden_subagent_tool("board.anything"))
        self.assertFalse(ts.forbidden_subagent_tool("exec"))


class _EchoPlugin:
    name = "echo"

    def __init__(self):
        self.calls = []

    def tools(self):
        return [{"function": {"name": "echo"}}]

    def invoke(self, call, config):
        self.calls.append(call)
        return ToolResult(content="echo", ok=True)


class _BoomPlugin:
    name = "boom"

    def tools(self):
        return [{"function": {"name": "boom"}}]

    def invoke(self, call, config):
        raise RuntimeError("kaboom")


def _completion(script):
    """Fake gateway: returns the scripted responses in order."""
    calls = []

    def completion(**kwargs):
        calls.append(kwargs)
        item = script[min(len(calls), len(script)) - 1]
        if isinstance(item, Exception):
            raise item
        return {"choices": [{"message": item}]}

    completion.calls = calls
    return completion


def _tool_call(name, args=None, call_id="c1"):
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(args or {})},
    }


def _run(script, *, plugins=None, max_turns=5, brief=None, captured=None):
    ts.configure_runner_plugins(plugins or [_EchoPlugin()])
    tools = [{"function": {"name": p}} for p in ["echo", "boom", "board.complete", "spawn_subagent"]]

    def kwargs_factory(messages, child_tools):
        kwargs = {
            "model": "test-model",
            "messages": messages,
            "tools": list(child_tools),
            "extra_body": {"litellm_metadata": {"skquad_agent_id": "agent-1"}},
        }
        if captured is not None:
            # Capture the live object: the runner marks metering metadata
            # on this same dict before calling the completion.
            captured.append(kwargs)
        return kwargs

    return ts.run_task_subagent(
        brief or ts.TaskBrief(task_id="task-1", title="Rate limit"),
        system_prompt="PARENT COMPOSED PROMPT",
        completion=_completion(script),
        tools=tools,
        kwargs_factory=kwargs_factory,
        origin="task:task-1",
        max_turns=max_turns,
    )


class RunnerTest(unittest.TestCase):
    def test_happy_path_returns_parsed_result(self):
        run = _run([{"content": fenced(GOOD_RESULT)}])
        self.assertTrue(run.result.succeeded)
        # The runner overrides the subagent's self-report with the real
        # turn count (the §8 metric must not be model-flattered).
        self.assertEqual(run.result.turns_used, 1)
        self.assertEqual(len(run.thread), 2)  # brief + final answer

    def test_empty_context_and_inherited_prompt(self):
        captured = []
        _run([{"content": fenced(GOOD_RESULT)}], captured=captured)
        messages = captured[0]["messages"]
        self.assertEqual(messages[0]["role"], "system")
        self.assertEqual(messages[0]["content"], "PARENT COMPOSED PROMPT")
        # I2: the only other message is the brief — no parent transcript.
        self.assertEqual(len(messages), 2)
        self.assertIn("TASK BRIEF", messages[1]["content"])

    def test_metering_markers_present(self):
        # Spec §8: skquad_subagent AND skquad_task_id on every subagent call.
        captured = []
        _run([{"content": fenced(GOOD_RESULT)}], captured=captured)
        metadata = captured[0]["extra_body"]["litellm_metadata"]
        self.assertTrue(metadata["skquad_subagent"])
        self.assertEqual(metadata["skquad_task_id"], "task-1")
        # the parent's own identity markers survive alongside them
        self.assertEqual(metadata["skquad_agent_id"], "agent-1")

    def test_tools_never_include_board_or_spawn(self):
        captured = []
        _run([{"content": fenced(GOOD_RESULT)}], captured=captured)
        names = [t["function"]["name"] for t in captured[0]["tools"]]
        self.assertIn("echo", names)
        self.assertNotIn("board.complete", names)
        self.assertNotIn("spawn_subagent", names)

    def test_forbidden_tool_call_is_refused_at_execution(self):
        run = _run(
            [
                {"content": "", "tool_calls": [_tool_call("board.complete", {"status": "done"})]},
                {"content": fenced(GOOD_RESULT)},
            ]
        )
        self.assertIn("board.complete", run.steps)
        tool_entry = next(e for e in run.thread if e.get("name") == "board.complete")
        self.assertFalse(tool_entry["ok"])
        self.assertIn("not available to task subagents", tool_entry["content"])

    def test_tool_error_feeds_back_instead_of_aborting(self):
        run = _run(
            plugins=[_BoomPlugin()],
            script=[
                {"content": "", "tool_calls": [_tool_call("boom")]},
                {"content": fenced(GOOD_RESULT)},
            ],
        )
        # S-263 semantics: the error goes back to the model, the run continues.
        self.assertTrue(run.result.succeeded)

    def test_turn_budget_exhaustion_is_classified(self):
        script = [{"content": "still working", "tool_calls": [_tool_call("echo")]}]
        run = _run(script, max_turns=3)
        self.assertFalse(run.result.succeeded)
        self.assertEqual(run.result.failure_class, "turn_budget")
        self.assertEqual(run.result.turns_used, 3)
        self.assertTrue(run.result.review_recommended)

    def test_llm_failure_becomes_structured_failure(self):
        run = _run([RuntimeError("gateway down")])
        self.assertFalse(run.result.succeeded)
        self.assertEqual(run.result.failure_class, "other")
        self.assertIn("gateway down", run.result.summary)

    def test_contract_violation_from_final_message(self):
        run = _run([{"content": "all done, trust me"}])
        self.assertEqual(run.result.failure_class, "contract_violation")


class ParentReviewTest(unittest.TestCase):
    def _ok(self, **overrides):
        return ts.validate_task_result(dict(GOOD_RESULT, **overrides))

    def test_clean_result_self_approves_with_justification(self):
        decision = ts.review_task_result(
            self._ok(), has_workspace=True, workspace_sha="abc1234def5678"
        )
        self.assertEqual(decision.action, "complete")
        self.assertEqual(decision.status, "done")
        self.assertFalse(decision.review_needed)
        # CP requires a non-empty justification for review_needed=false.
        self.assertTrue(decision.justification.strip())
        self.assertIn("14 passed", decision.justification)
        self.assertIn("verified in workspace", decision.justification)

    def test_short_sha_prefix_counts_as_match(self):
        decision = ts.review_task_result(
            self._ok(commit_sha="abc1234"), has_workspace=True, workspace_sha="abc1234def5678"
        )
        self.assertEqual(decision.status, "done")

    def test_sha_mismatch_forces_review(self):
        # I4: a claimed SHA that is not the workspace HEAD is never self-approved.
        decision = ts.review_task_result(
            self._ok(commit_sha="deadbeef"), has_workspace=True, workspace_sha="abc1234def5678"
        )
        self.assertEqual(decision.action, "complete")
        self.assertEqual(decision.status, "in-review")
        self.assertTrue(decision.review_needed)
        self.assertTrue(decision.forced_review)
        self.assertIn("does not match workspace HEAD", " ".join(decision.concerns))

    def test_missing_sha_forces_review(self):
        decision = ts.review_task_result(
            self._ok(commit_sha=""), has_workspace=True, workspace_sha="abc1234def5678"
        )
        self.assertTrue(decision.forced_review)
        self.assertEqual(decision.status, "in-review")

    def test_dirty_workspace_forces_review(self):
        # Uncommitted leftovers mean the claimed SHA does not cover the
        # whole change — the finalize sweep must not be self-approved.
        decision = ts.review_task_result(
            self._ok(),
            has_workspace=True,
            workspace_sha="abc1234def5678",
            workspace_dirty=True,
        )
        self.assertEqual(decision.status, "in-review")
        self.assertTrue(decision.forced_review)
        self.assertIn("uncommitted", " ".join(decision.concerns))

    def test_no_workspace_skips_sha_check(self):
        decision = ts.review_task_result(self._ok(commit_sha=""), has_workspace=False)
        self.assertEqual(decision.status, "done")
        self.assertFalse(decision.forced_review)

    def test_review_recommended_is_never_downgraded(self):
        decision = ts.review_task_result(
            self._ok(review_recommended=True, review_notes="uncertain about the migration"),
            has_workspace=True,
            workspace_sha="abc1234def5678",
        )
        self.assertEqual(decision.status, "in-review")
        self.assertTrue(decision.review_needed)
        self.assertIn("uncertain about the migration", decision.summary)

    def test_proposed_in_review_is_conservative(self):
        decision = ts.review_task_result(
            self._ok(proposed_status="in-review", review_recommended=False),
            has_workspace=True,
            workspace_sha="abc1234def5678",
        )
        self.assertEqual(decision.status, "in-review")

    def test_fixable_failure_reworks_within_budget(self):
        result = ts.validate_task_result(
            {
                "schema": "skquad.task_result.v1",
                "succeeded": False,
                "failure_class": "tool_error",
                "summary": "test runner missing",
                "proposed_status": "blocked",
                "changed_files": ["a.go"],
            }
        )
        decision = ts.review_task_result(result, has_workspace=True, reworks_used=0, rework_max=2)
        self.assertEqual(decision.action, "rework")
        self.assertTrue(decision.fix_instructions)
        self.assertIn("tool_error", decision.fix_instructions[0])

    def test_rework_exhaustion_blocks(self):
        result = ts.validate_task_result(
            {
                "schema": "skquad.task_result.v1",
                "succeeded": False,
                "failure_class": "tool_error",
                "summary": "still broken",
                "proposed_status": "blocked",
            }
        )
        decision = ts.review_task_result(result, has_workspace=True, reworks_used=2, rework_max=2)
        self.assertEqual(decision.action, "block")

    def test_turn_budget_blocks_rather_than_rework(self):
        # Spec §9: a budget overrun is a human decision, not a free respawn.
        result = ts.validate_task_result(
            {
                "schema": "skquad.task_result.v1",
                "succeeded": False,
                "failure_class": "turn_budget",
                "summary": "needs decision: split task or raise budget",
                "proposed_status": "blocked",
            }
        )
        decision = ts.review_task_result(result, has_workspace=True, reworks_used=0, rework_max=2)
        self.assertEqual(decision.action, "block")

    def test_access_denied_blocks(self):
        result = ts.validate_task_result(
            {
                "schema": "skquad.task_result.v1",
                "succeeded": False,
                "failure_class": "access_denied",
                "summary": "repo not reachable with this token",
                "proposed_status": "blocked",
            }
        )
        self.assertEqual(
            ts.review_task_result(result, has_workspace=True, reworks_used=0).action, "block"
        )

    def test_concerns_surface_into_summary(self):
        result = self._ok(concerns=["uncommitted work on branch"])
        decision = ts.review_task_result(result, has_workspace=False)
        self.assertIn("uncommitted work on branch", decision.summary)


if __name__ == "__main__":
    unittest.main()
