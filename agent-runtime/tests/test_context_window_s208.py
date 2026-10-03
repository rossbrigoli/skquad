"""S-208: compaction limit derived from the model's context_window.

Covers the cached fetch helper, the compactor factory fallback contract
(unknown window → env behaviour, never a crash), and the handler-level
wiring on both the chat and task paths.
"""

import unittest

from skquad_runtime import runtime as rt


class FakeCP:
    def __init__(self, payload=None, error=None):
        self.payload = payload or {}
        self.error = error
        self.calls = 0

    def get_my_model(self):
        self.calls += 1
        if self.error:
            raise self.error
        return self.payload


class ModelContextWindowCachedTests(unittest.TestCase):
    def test_reads_window_and_caches(self):
        cache: dict = {}
        cp = FakeCP({"context_window": 128000})
        self.assertEqual(rt.model_context_window_cached(cache, "m", cp.get_my_model), 128000)
        self.assertEqual(rt.model_context_window_cached(cache, "m", cp.get_my_model), 128000)
        self.assertEqual(cp.calls, 1, "second read must come from the cache")

    def test_zero_and_missing_read_as_unknown(self):
        for payload in ({}, {"context_window": 0}, {"context_window": None}):
            with self.subTest(payload=payload):
                cache: dict = {}
                self.assertEqual(rt.model_context_window_cached(cache, "m", FakeCP(payload).get_my_model), 0)

    def test_fetch_failure_is_unknown_not_fatal(self):
        cache: dict = {}
        cp = FakeCP(error=RuntimeError("control-plane down"))
        self.assertEqual(rt.model_context_window_cached(cache, "m", cp.get_my_model), 0)
        # Failure is cached too (no hammering a down control-plane).
        self.assertEqual(rt.model_context_window_cached(cache, "m", cp.get_my_model), 0)
        self.assertEqual(cp.calls, 1)

    def test_garbage_value_is_unknown(self):
        cache: dict = {}
        self.assertEqual(
            rt.model_context_window_cached(cache, "m", FakeCP({"context_window": "wide"}).get_my_model), 0
        )


class CompactorForModelTests(unittest.TestCase):
    def _big_messages(self, tokens):
        # chars/4 heuristic: 4 chars ≈ 1 token.
        return [{"role": "user", "content": "x" * (tokens * 4)}]

    def test_model_window_drives_compaction(self):
        # 600 tokens against a 1000-token window → 60% fill → tier 1.
        compactor = rt.compactor_for_model(1000)
        _, report = compactor.maybe_compact(self._big_messages(600))
        self.assertEqual(report.tier, 1)

    def test_unknown_window_falls_back_to_env_default(self):
        # 600 tokens is nothing against the 32768 default → no compaction.
        compactor = rt.compactor_for_model(0)
        _, report = compactor.maybe_compact(self._big_messages(600))
        self.assertEqual(report.tier, 0)

    def test_unknown_window_respects_env_override(self):
        original = __import__("os").environ.get(rt.ENV_CONTEXT_LIMIT)
        __import__("os").environ[rt.ENV_CONTEXT_LIMIT] = "1000"
        try:
            compactor = rt.compactor_for_model(0)
            _, report = compactor.maybe_compact(self._big_messages(600))
            self.assertEqual(report.tier, 1, "env fallback must still work")
        finally:
            if original is None:
                del __import__("os").environ[rt.ENV_CONTEXT_LIMIT]
            else:
                __import__("os").environ[rt.ENV_CONTEXT_LIMIT] = original


class HandlerWiringTests(unittest.TestCase):
    def _config(self, default_model="m"):
        from pathlib import Path

        return rt.BootstrapConfig(
            agent_id="agent-1", squad_id="s-1", role="helper",
            default_model=default_model, idle_timeout="300",
            credentials_dir=Path("/tmp"), agent_credential_path=Path("/tmp/agent"),
            virtual_key_path=Path("/tmp/key"), control_plane_url="http://cp.test",
            llm_gateway_url="http://gw.test", task_loop_enabled=False,
            task_poll_interval_seconds=30, inbox_poll_interval_seconds=15,
            inbox_batch_size=5, task_timeout_seconds=600,
            heartbeat_interval_seconds=15, max_llm_steps=8,
            task_summary_max_chars=2000, plugin_modules=(), enabled_plugins=(),
        )

    def test_chat_handler_uses_bound_model_window(self):
        handler = rt.LLMMessageHandler(client=FakeCP({"context_window": 200000}))
        window = handler._model_context_window(self._config(), "m")
        self.assertEqual(window, 200000)
        # And the compactor built from it triggers at 60% of 200k.
        compactor = rt.compactor_for_model(window)
        msgs = [{"role": "user", "content": "x" * (120_000 * 4)}]
        _, report = compactor.maybe_compact(msgs)
        self.assertGreater(report.tier, 0)

    def test_chat_handler_no_model_is_unknown(self):
        handler = rt.LLMMessageHandler(client=FakeCP({"context_window": 200000}))
        self.assertEqual(handler._model_context_window(self._config(default_model=""), ""), 0)

    def test_chat_handler_falls_back_to_env_when_window_unknown(self):
        handler = rt.LLMMessageHandler(client=FakeCP({}))
        window = handler._model_context_window(self._config(), "m")
        self.assertEqual(window, 0)
        compactor = rt.compactor_for_model(window)
        msgs = [{"role": "user", "content": "x" * (600 * 4)}]
        _, report = compactor.maybe_compact(msgs)
        self.assertEqual(report.tier, 0, "600 tokens is under the 32768 env default")

    def test_task_handler_uses_bound_model_window(self):
        handler = rt.LiteLLMTaskHandler(completion=lambda **kw: None)
        cp = FakeCP({"context_window": 131072})
        original = rt.ControlPlaneClient.from_bootstrap
        rt.ControlPlaneClient.from_bootstrap = classmethod(lambda cls, config: cp)
        try:
            window = handler._model_context_window(self._config(default_model="m"), "m")
        finally:
            rt.ControlPlaneClient.from_bootstrap = original
        self.assertEqual(window, 131072)


if __name__ == "__main__":
    unittest.main()
