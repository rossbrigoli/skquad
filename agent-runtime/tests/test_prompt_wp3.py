"""S-PROMPT WP3 tests — fetch-at-wake, ETag caching, loud-fail, trust labels.

The control-plane HTTP layer is mocked via the injectable ``opener`` on
:class:`PromptFetcher` (same pattern as ``ControlPlaneClient`` tests). The
suite-wide conftest defaults ``SKQUAD_PROMPT_FETCH_ENABLED=false``; these
tests flip it on explicitly to exercise the composed path.
"""

import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock
from urllib import error

from skquad_runtime import prompt_fetch as pf
from skquad_runtime.journal import load_journal
from skquad_runtime.runtime import (
    LiteLLMTaskHandler,
    PromptedRuntime,
    RuntimeMemory,
    RuntimeResource,
    run_task_once,
    wrap_untrusted,
)

COMPOSED = "<skquad_platform trust=\"platform\">platform rules</skquad_platform>"
SHA = "ab" * 32


def composed_body(prompt=COMPOSED, sha=SHA):
    return json.dumps(
        {
            "prompt": prompt,
            "sha256": sha,
            "total_tokens": 123,
            "tiers": [{"name": "platform", "tokens": 123}],
        }
    ).encode("utf-8")


class FakeHTTPResponse:
    def __init__(self, status, payload, etag=""):
        self.status = status
        self._payload = payload
        self.headers = {"ETag": etag} if etag else {}

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def read(self):
        return self._payload

    def getcode(self):
        return self.status


def wp3_config(tmp, **overrides):
    from skquad_runtime.runtime import load_bootstrap_config

    credential = Path(tmp) / "agent"
    credential.write_text("credential", encoding="utf-8")
    virtual = Path(tmp) / "llm-gateway"
    virtual.write_text("virtual-key", encoding="utf-8")
    env = {
        "SKQUAD_AGENT_ID": "agent-1",
        "SKQUAD_SQUAD_ID": "squad-1",
        "SKQUAD_AGENT_CREDENTIAL_PATH": str(credential),
        "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH": str(virtual),
        "SKQUAD_CONTROL_PLANE_URL": "http://control-plane",
        "SKQUAD_LLM_GATEWAY_URL": "http://gateway",
        "SKQUAD_DEFAULT_MODEL": "model-1",
        "SKQUAD_TASK_LOOP_ENABLED": "false",
    }
    env.update(overrides)
    return load_bootstrap_config(env)


class WP3EnabledMixin(unittest.TestCase):
    """Base that flips SKQUAD_PROMPT_FETCH_ENABLED on for the whole test
    (the suite-wide conftest defaults it off for the legacy tests)."""

    def setUp(self):
        super().setUp()
        patcher = mock.patch.dict(
            "os.environ", {"SKQUAD_PROMPT_FETCH_ENABLED": "true"}
        )
        patcher.start()
        self.addCleanup(patcher.stop)


class PromptFetcherTest(WP3EnabledMixin):
    def _fetcher(self, opener, cache=None):
        return pf.PromptFetcher("http://cp", "agent-1", "cred", opener=opener, cache=cache)

    def test_fetch_success_returns_prompt_and_sha(self):
        calls = []

        def opener(req):
            calls.append(req)
            return FakeHTTPResponse(200, composed_body(), etag='"v1"')

        source = self._fetcher(opener).fetch()
        self.assertEqual(source.status, "ok")
        self.assertEqual(source.prompt, COMPOSED)
        self.assertEqual(source.sha, SHA)
        self.assertEqual(source.etag, '"v1"')
        self.assertEqual(calls[0].full_url, "http://cp/api/v1/agents/me/prompt")
        self.assertEqual(calls[0].headers["Authorization"], "Bearer cred")
        self.assertNotIn("If-None-Match", calls[0].headers)

    def test_etag_round_second_fetch_sends_if_none_match_and_reuses_cache(self):
        calls = []

        def opener(req):
            calls.append(req)
            if "If-None-Match" in req.headers:
                return FakeHTTPResponse(304, b"", etag='"v1"')
            return FakeHTTPResponse(200, composed_body(), etag='"v1"')

        fetcher = self._fetcher(opener)
        first = fetcher.fetch()
        second = fetcher.fetch()
        self.assertEqual(first.prompt, COMPOSED)
        self.assertEqual(second.prompt, COMPOSED)
        self.assertEqual(second.sha, SHA)
        self.assertIn("if-none-match", {k.lower() for k in calls[1].headers})
        self.assertEqual(calls[1].headers["If-none-match"], '"v1"')
        self.assertEqual(len(calls), 2)

    def test_5xx_raises_prompt_fetch_error(self):
        def opener(req):
            raise error.HTTPError(req.full_url, 503, "unavailable", {}, io.BytesIO(b""))

        with self.assertRaises(pf.PromptFetchError):
            self._fetcher(opener).fetch()

    def test_network_error_raises_prompt_fetch_error(self):
        def opener(req):
            raise error.URLError("connection refused")

        with self.assertRaises(pf.PromptFetchError) as ctx:
            self._fetcher(opener).fetch()
        self.assertIn("unreachable", str(ctx.exception))

    def test_404_returns_legacy_no_endpoint(self):
        def opener(req):
            raise error.HTTPError(req.full_url, 404, "not found", {}, io.BytesIO(b""))

        source = self._fetcher(opener).fetch()
        self.assertEqual(source.status, pf.LEGACY_NO_ENDPOINT)

    def test_malformed_body_raises(self):
        def opener(req):
            return FakeHTTPResponse(200, b"not json", etag='"v1"')

        with self.assertRaises(pf.PromptFetchError):
            self._fetcher(opener).fetch()

    def test_304_without_cache_raises(self):
        def opener(req):
            return FakeHTTPResponse(304, b"", etag='"v1"')

        with self.assertRaises(pf.PromptFetchError):
            self._fetcher(opener).fetch()


class PromptedRuntimeTest(WP3EnabledMixin):
    def test_composed_prompt_used_as_system_message(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = wp3_config(tmp)
            runtime = PromptedRuntime(config, fetcher=_ok_fetcher())
            self.assertEqual(runtime.system_prompt(), COMPOSED)
            self.assertEqual(runtime.chat_system_prompt(), COMPOSED)
            self.assertEqual(runtime.source, "composed")
            self.assertEqual(runtime.prompt_sha, SHA)

    def test_composed_path_keeps_dynamic_resource_section(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = wp3_config(tmp)
            runtime = PromptedRuntime(config, fetcher=_ok_fetcher())
            resource = RuntimeResource("tool", "r-1", "kb", "knowledge base", "", {})
            prompt = runtime.system_prompt([resource], [])
            self.assertTrue(prompt.startswith(COMPOSED))
            self.assertIn("Granted resources:", prompt)
            self.assertIn("kb", prompt)

    def test_legacy_fallback_on_404(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = wp3_config(tmp, SKQUAD_AGENT_SYSTEM_PROMPT="You are a terse pirate.")

            def opener(req):
                raise error.HTTPError(req.full_url, 404, "not found", {}, io.BytesIO(b""))

            runtime = PromptedRuntime(config, fetcher=pf.PromptFetcher(
                "http://cp", "agent-1", "cred", opener=opener))
            self.assertEqual(runtime.chat_system_prompt(), "You are a terse pirate.")
            self.assertEqual(runtime.source, "env_legacy")
            self.assertEqual(runtime.prompt_sha, "env_legacy")

    def test_legacy_fallback_when_flag_off(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = wp3_config(tmp, SKQUAD_AGENT_SYSTEM_PROMPT="Legacy pirate prompt.")
            with mock.patch.dict("os.environ", {"SKQUAD_PROMPT_FETCH_ENABLED": "false"}):
                runtime = PromptedRuntime(config)
                self.assertEqual(runtime.source, "env_legacy")
                self.assertEqual(runtime.prompt_sha, "env_legacy")
                self.assertEqual(runtime.chat_system_prompt(), "Legacy pirate prompt.")

    def test_fetch_failure_raises_out_of_system_prompt(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = wp3_config(tmp)

            def opener(req):
                raise error.URLError("boom")

            runtime = PromptedRuntime(config, fetcher=pf.PromptFetcher(
                "http://cp", "agent-1", "cred", opener=opener))
            with self.assertRaises(pf.PromptFetchError):
                runtime.system_prompt()


class TaskWakeIntegrationTest(WP3EnabledMixin):
    def test_task_uses_composed_prompt_and_journals_sha(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "ws"
            config = wp3_config(
                tmp,
                SKQUAD_WORKSPACE_ENABLED="true",
                SKQUAD_WORKSPACE_BASE=str(base),
            )
            llm_calls = []

            def completion(**kwargs):
                llm_calls.append(kwargs)
                return {"choices": [{"message": {"content": "SKQUAD_STATUS: done\nok"}}]}

            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            with mock.patch(
                "skquad_runtime.runtime.ControlPlaneClient.from_bootstrap"
            ) as from_bs:
                fake_cp = _SilentCP()
                from_bs.return_value = fake_cp
                with mock.patch.object(
                    pf.PromptFetcher,
                    "__init__",
                    autospec=True,
                    side_effect=lambda self_, base_url, agent_id, credential, opener=None, cache=None: _init_fetcher(
                        self_, base_url, agent_id, credential, opener_ok, cache
                    ),
                ):
                    run_task_once(config, handler)

            system_message = llm_calls[0]["messages"][0]["content"]
            self.assertEqual(system_message, COMPOSED)
            task_dir = base / "tasks" / "task-1"
            journal = load_journal(task_dir)
            self.assertEqual(journal["prompt_sha"], SHA)

    def test_task_wake_fails_loudly_on_5xx(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp) / "ws"
            config = wp3_config(
                tmp,
                SKQUAD_WORKSPACE_ENABLED="true",
                SKQUAD_WORKSPACE_BASE=str(base),
            )
            llm_calls = []

            def completion(**kwargs):
                llm_calls.append(kwargs)
                return {"choices": [{"message": {"content": "should not run"}}]}

            handler = LiteLLMTaskHandler(completion=completion, discover_resources=False)
            with mock.patch(
                "skquad_runtime.runtime.ControlPlaneClient.from_bootstrap"
            ) as from_bs:
                fake_cp = _SilentCP()
                from_bs.return_value = fake_cp
                with mock.patch.object(
                    pf.PromptFetcher,
                    "__init__",
                    autospec=True,
                    side_effect=lambda self_, base_url, agent_id, credential, opener=None, cache=None: _init_fetcher(
                        self_, base_url, agent_id, credential, opener_bad, cache
                    ),
                ):
                    run_task_once(config, handler)

            self.assertEqual(llm_calls, [], "no LLM call may happen after a failed fetch")
            self.assertTrue(
                any("prompt fetch failed" in s for s in fake_cp.blocked_summaries),
                f"task must be blocked with the fetch failure, got {fake_cp.blocked_summaries}",
            )


def opener_ok(req):
    return FakeHTTPResponse(200, composed_body(), etag='"v1"')


def opener_bad(req):
    raise error.HTTPError(req.full_url, 500, "boom", {}, io.BytesIO(b""))


def _init_fetcher(self_, base_url, agent_id, credential, opener, cache):
    self_.base_url = base_url
    self_.agent_id = agent_id
    self_.credential = credential
    self_._opener = opener
    self_.cache = cache if cache is not None else pf.ComposedPromptCache()
    return None


def _ok_fetcher():
    return pf.PromptFetcher("http://cp", "agent-1", "cred", opener=opener_ok)


class _SilentCP:
    """Minimal control-plane double for run_task_once."""

    def __init__(self):
        self.blocked_summaries = []
        self.completed = []
        self.started = []  # (task_id, prompt_sha) — S-PROMPT WP5 run-audit

    def claim_task(self):
        from skquad_runtime.runtime import RuntimeTask

        return RuntimeTask(
            id="task-1",
            squad_id="squad-1",
            title="T",
            description="D",
            status="in-progress",
            assignee_agent_id="agent-1",
            execution_id="exec-1",
            worker_id="w-1",
            fencing_token="f-1",
            lease_expires_at="",
        )

    def heartbeat(self, status, task=None):
        return {}

    def start_task(self, task_id, prompt_sha=None):
        self.started.append((task_id, prompt_sha))
        return self.claim_task()

    def block_task(self, task, summary=""):
        self.blocked_summaries.append(summary)
        return task

    def complete_task(self, task, status="in-review", summary="", persist_memory=False):
        self.completed.append((status, summary))
        return task

    def task_context(self, task_id):
        from skquad_runtime.runtime import RuntimeTaskContext

        return RuntimeTaskContext(task=None, resources=[], memory={}, limits={})

    def report_task_workspace(self, *args, **kwargs):
        return None


class TrustLabelTest(unittest.TestCase):
    def test_wrap_task(self):
        wrapped = wrap_untrusted("payload", "task", id="t-9")
        self.assertEqual(wrapped, '<skquad_untrusted source="task" id="t-9">payload</skquad_untrusted>')

    def test_wrap_inbox_with_from_attribute(self):
        wrapped = wrap_untrusted("hi", "inbox", **{"from": "agent:xyz"})
        self.assertEqual(wrapped, '<skquad_untrusted source="inbox" from="agent:xyz">hi</skquad_untrusted>')

    def test_wrap_tool_result(self):
        wrapped = wrap_untrusted("out", "tool_result", tool="kb")
        self.assertEqual(wrapped, '<skquad_untrusted source="tool_result" tool="kb">out</skquad_untrusted>')

    def test_wrap_memory(self):
        memory = RuntimeMemory(
            id="m-1", agent_id="a-1", squad_id="s-1",
            content="some  remembered  thing", raw_content="",
            trust_level="raw_model_output", provenance="task_completion",
            review_status="pending_review", embedding_model="",
            source_task_id="task-7", metadata={},
        )
        from skquad_runtime.runtime import memory_prompt_line

        line = memory_prompt_line(memory)
        self.assertIn('source_task=task-7', line)
        self.assertIn(
            '<skquad_untrusted source="memory" trust="raw_model_output" '
            'provenance="task_completion" review="pending_review">'
            "some remembered thing</skquad_untrusted>",
            line,
        )

    def test_attribute_values_are_escaped(self):
        wrapped = wrap_untrusted('x', "inbox", **{"from": 'evil"agent&#39;'})
        # The embedded raw quote must be escaped, never able to close the
        # attribute early.
        self.assertNotIn('evil"', wrapped)
        self.assertIn('from="evil&quot;agent&amp;#39;"', wrapped)
        self.assertIn('from="evil&quot;agent&amp;#39;"', wrapped)


if __name__ == "__main__":
    unittest.main()
