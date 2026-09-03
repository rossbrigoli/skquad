"""Unit tests for the Skquad LiteLLM metering callback.

These run in two places:

1. On the CI runner with only the standard library. ``litellm`` is a very large
   dependency, so when it is absent a minimal stand-in for
   ``litellm.integrations.custom_logger.CustomLogger`` is injected before import.
2. Inside the published gateway container, where the real ``litellm`` is used and
   the base class is whatever the pinned version provides.

The behaviour that matters most here is the last test class: a metering problem
must never propagate into the proxied LLM call.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
import types
import unittest
from pathlib import Path
from unittest import mock
from urllib import error

TESTS_DIR = Path(__file__).resolve().parent
REPO_ROOT = TESTS_DIR.parent.parent
GATEWAY_DIR = TESTS_DIR.parent

if str(GATEWAY_DIR) not in sys.path:
    sys.path.insert(0, str(GATEWAY_DIR))


try:  # pragma: no cover - depends on environment
    import litellm.integrations.custom_logger  # noqa: F401
except ModuleNotFoundError:  # pragma: no cover - test-only shim
    _litellm = types.ModuleType("litellm")
    _integrations = types.ModuleType("litellm.integrations")
    _custom_logger = types.ModuleType("litellm.integrations.custom_logger")

    class CustomLogger:  # minimal stand-in for the real ABC
        pass

    _custom_logger.CustomLogger = CustomLogger
    _integrations.custom_logger = _custom_logger
    _litellm.integrations = _integrations
    sys.modules.setdefault("litellm", _litellm)
    sys.modules.setdefault("litellm.integrations", _integrations)
    sys.modules.setdefault("litellm.integrations.custom_logger", _custom_logger)

import skquad_litellm_callbacks as callbacks  # noqa: E402


ENDPOINT = "http://control-plane.skquad.svc:8080"
TOKEN = "callback-token"


class EnvMixin(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch.dict(
            os.environ,
            {
                "SKQUAD_CONTROL_PLANE_URL": ENDPOINT,
                "SKQUAD_GATEWAY_CALLBACK_TOKEN": TOKEN,
            },
            clear=False,
        )
        patcher.start()
        self.addCleanup(patcher.stop)


def run(coro) -> object:
    return asyncio.run(coro)


class FakeResponse:
    def __init__(self, status: int = 200) -> None:
        self.status = status

    def __enter__(self):
        return self

    def __exit__(self, *exc_info):
        return False


class UsageObject:
    """Usage reported as an object rather than a mapping."""

    def __init__(self, prompt_tokens=11, completion_tokens=7) -> None:
        self.prompt_tokens = prompt_tokens
        self.completion_tokens = completion_tokens


def metadata_kwargs(**metadata) -> dict:
    return {"litellm_params": {"metadata": metadata}}


class CallbackMetadataTests(unittest.TestCase):
    def test_prefers_litellm_params_metadata(self) -> None:
        kwargs = {
            "litellm_params": {"metadata": {"skquad_agent_id": "from-params"}},
            "metadata": {"skquad_agent_id": "from-top-level"},
        }
        self.assertEqual(callbacks.callback_metadata(kwargs)["skquad_agent_id"], "from-params")

    def test_falls_back_to_top_level_metadata(self) -> None:
        # Regression: an empty/absent litellm_params must not short-circuit the
        # top-level metadata, or every such call is metered as unattributed and
        # dropped.
        kwargs = {"metadata": {"skquad_agent_id": "from-top-level"}}
        self.assertEqual(callbacks.callback_metadata(kwargs)["skquad_agent_id"], "from-top-level")

    def test_falls_back_when_nested_metadata_is_empty(self) -> None:
        kwargs = {"litellm_params": {"metadata": {}}, "metadata": {"skquad_agent_id": "from-top-level"}}
        self.assertEqual(callbacks.callback_metadata(kwargs)["skquad_agent_id"], "from-top-level")

    def test_empty_when_absent(self) -> None:
        self.assertEqual(callbacks.callback_metadata({}), {})

    def test_ignores_non_mapping_metadata(self) -> None:
        self.assertEqual(callbacks.callback_metadata({"litellm_params": {"metadata": "nope"}, "metadata": ["nope"]}), {})

    def test_tolerates_none_litellm_params(self) -> None:
        self.assertEqual(callbacks.callback_metadata({"litellm_params": None, "metadata": None}), {})


class ResponseUsageTests(unittest.TestCase):
    def test_mapping_usage_passes_through(self) -> None:
        usage = {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}
        self.assertEqual(callbacks.response_usage({"usage": usage}), usage)

    def test_object_usage_is_normalised_to_a_mapping(self) -> None:
        response = types.SimpleNamespace(usage=UsageObject(prompt_tokens=11, completion_tokens=7))
        self.assertEqual(
            callbacks.response_usage(response),
            {"prompt_tokens": 11, "completion_tokens": 7},
        )

    def test_missing_usage_yields_empty_mapping(self) -> None:
        # No usage at all resolves to {} and therefore to zero tokens downstream.
        self.assertEqual(callbacks.response_usage(None), {})

    def test_non_mapping_usage_object_without_attributes(self) -> None:
        self.assertEqual(
            callbacks.response_usage(types.SimpleNamespace(usage="not-usage")),
            {"prompt_tokens": None, "completion_tokens": None},
        )


class CoercionTests(unittest.TestCase):
    def test_int_value(self) -> None:
        cases = {
            None: 0,
            "": 0,
            "abc": 0,
            "12": 12,
            12: 12,
            3.9: 3,
            object(): 0,
        }
        for raw, expected in cases.items():
            with self.subTest(raw=raw):
                self.assertEqual(callbacks.int_value(raw), expected)

    def test_float_value(self) -> None:
        cases = {
            None: 0.0,
            "": 0.0,
            "abc": 0.0,
            "1.25": 1.25,
            2: 2.0,
            object(): 0.0,
        }
        for raw, expected in cases.items():
            with self.subTest(raw=raw):
                self.assertEqual(callbacks.float_value(raw), expected)

    def test_safe_error(self) -> None:
        self.assertEqual(callbacks.safe_error(None), "")
        self.assertEqual(callbacks.safe_error(ValueError("boom")), "boom")
        self.assertEqual(callbacks.safe_error({"code": 500}), "{'code': 500}")


class ValueTests(unittest.TestCase):
    def test_mapping_and_attribute_access(self) -> None:
        self.assertEqual(callbacks.value({"a": 1}, "a"), 1)
        self.assertIsNone(callbacks.value({"a": 1}, "b"))
        self.assertEqual(callbacks.value(types.SimpleNamespace(a=2), "a"), 2)
        self.assertIsNone(callbacks.value(types.SimpleNamespace(), "a"))
        self.assertIsNone(callbacks.value("string", "a"))


class SendMeteringEventTests(EnvMixin):
    def setUp(self):
        super().setUp()
        urlopen = mock.patch.object(callbacks.request, "urlopen")
        self.urlopen = urlopen.start()
        self.addCleanup(urlopen.stop)
        self.urlopen.return_value = FakeResponse(200)

    # -- gating -------------------------------------------------------------

    def _sent_payload(self):
        self.urlopen.assert_called_once()
        req = self.urlopen.call_args.args[0]
        return json.loads(req.data.decode("utf-8"))

    def test_disabled_without_endpoint(self) -> None:
        os.environ["SKQUAD_CONTROL_PLANE_URL"] = ""
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.urlopen.assert_not_called()

    def test_disabled_without_token(self) -> None:
        os.environ["SKQUAD_GATEWAY_CALLBACK_TOKEN"] = ""
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.urlopen.assert_not_called()

    def test_disabled_when_endpoint_unset(self) -> None:
        os.environ.pop("SKQUAD_CONTROL_PLANE_URL", None)
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.urlopen.assert_not_called()

    def test_whitespace_only_token_is_treated_as_missing(self) -> None:
        os.environ["SKQUAD_GATEWAY_CALLBACK_TOKEN"] = "   "
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.urlopen.assert_not_called()

    def test_skipped_without_agent_id(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_squad_id="s"), {}, ""))
        self.urlopen.assert_not_called()

    def test_skipped_without_squad_id(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a"), {}, ""))
        self.urlopen.assert_not_called()

    def test_skipped_when_metadata_is_empty(self) -> None:
        run(callbacks.send_metering_event("success", {}, {}, ""))
        self.urlopen.assert_not_called()

    def test_top_level_metadata_is_metered(self) -> None:
        # Regression: metadata carried only on kwargs must still produce an event.
        kwargs = {"metadata": {"skquad_agent_id": "agent-1", "skquad_squad_id": "squad-1"}}
        run(callbacks.send_metering_event("success", kwargs, {}, ""))
        payload = self._sent_payload()
        self.assertEqual(payload["agent_id"], "agent-1")
        self.assertEqual(payload["squad_id"], "squad-1")

    # -- payload ------------------------------------------------------------

    def test_payload_fields(self) -> None:
        kwargs = metadata_kwargs(
            skquad_agent_id="agent-1",
            skquad_squad_id="squad-1",
            skquad_task_id="task-1",
            skquad_provider_id="prov-1",
            currency="AUD",
        )
        kwargs["model"] = "gpt-5.5"
        kwargs["response_cost"] = 0.125
        response = {"usage": {"prompt_tokens": 21, "completion_tokens": 9}}

        run(callbacks.send_metering_event("success", kwargs, response, ""))
        payload = self._sent_payload()
        self.assertEqual(
            payload,
            {
                "status": "success",
                "agent_id": "agent-1",
                "squad_id": "squad-1",
                "task_id": "task-1",
                "provider_id": "prov-1",
                "model": "gpt-5.5",
                "input_tokens": 21,
                "output_tokens": 9,
                "cost": 0.125,
                "currency": "AUD",
                "error": "",
            },
        )

    def test_model_falls_back_to_metadata_model(self) -> None:
        kwargs = metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s", model="metadata-model")
        run(callbacks.send_metering_event("success", kwargs, {}, ""))
        self.assertEqual(self._sent_payload()["model"], "metadata-model")

    def test_model_empty_when_neither_present(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.assertEqual(self._sent_payload()["model"], "")

    def test_currency_defaults_to_usd(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.assertEqual(self._sent_payload()["currency"], "USD")

    def test_missing_usage_reports_zero_tokens(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), None, ""))
        payload = self._sent_payload()
        self.assertEqual(payload["input_tokens"], 0)
        self.assertEqual(payload["output_tokens"], 0)

    def test_object_usage_is_metered(self) -> None:
        response = types.SimpleNamespace(usage=UsageObject(prompt_tokens=5, completion_tokens=6))
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), response, ""))
        payload = self._sent_payload()
        self.assertEqual(payload["input_tokens"], 5)
        self.assertEqual(payload["output_tokens"], 6)

    def test_unparsable_cost_becomes_zero(self) -> None:
        kwargs = metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s")
        kwargs["response_cost"] = "not-a-number"
        run(callbacks.send_metering_event("success", kwargs, {}, ""))
        self.assertEqual(self._sent_payload()["cost"], 0.0)

    def test_error_message_is_truncated_to_512_chars(self) -> None:
        long_error = "x" * 5000
        run(callbacks.send_metering_event("failure", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, long_error))
        self.assertEqual(len(self._sent_payload()["error"]), 512)

    def test_non_string_metadata_is_stringified(self) -> None:
        kwargs = metadata_kwargs(skquad_agent_id=12345, skquad_squad_id=678, skquad_task_id=None)
        run(callbacks.send_metering_event("success", kwargs, {}, ""))
        payload = self._sent_payload()
        self.assertEqual(payload["agent_id"], "12345")
        self.assertEqual(payload["squad_id"], "678")
        self.assertEqual(payload["task_id"], "")

    # -- request shape ------------------------------------------------------

    def test_request_url_headers_and_method(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        self.urlopen.assert_called_once()
        req = self.urlopen.call_args.args[0]
        self.assertEqual(req.full_url, ENDPOINT + "/api/v1/gateway/metering")
        self.assertEqual(req.get_method(), "POST")
        self.assertEqual(req.headers["Authorization"], "Bearer " + TOKEN)
        self.assertEqual(req.headers["Content-type"], "application/json")
        self.assertEqual(req.headers["Accept"], "application/json")

    def test_trailing_slash_on_endpoint_does_not_double_up(self) -> None:
        os.environ["SKQUAD_CONTROL_PLANE_URL"] = ENDPOINT + "/"
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        req = self.urlopen.call_args.args[0]
        self.assertEqual(req.full_url, ENDPOINT + "/api/v1/gateway/metering")

    def test_token_whitespace_is_stripped(self) -> None:
        os.environ["SKQUAD_GATEWAY_CALLBACK_TOKEN"] = "  spaced-token  "
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        req = self.urlopen.call_args.args[0]
        self.assertEqual(req.headers["Authorization"], "Bearer spaced-token")

    def test_payload_is_json_serialisable(self) -> None:
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))
        req = self.urlopen.call_args.args[0]
        decoded = json.loads(req.data.decode("utf-8"))
        self.assertIsInstance(decoded, dict)

    # -- failure isolation --------------------------------------------------

    def test_timeout_does_not_raise(self) -> None:
        self.urlopen.side_effect = error.URLError("timed out")
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))

    def test_non_2xx_does_not_raise(self) -> None:
        for status in (302, 400, 401, 403, 429, 500, 503):
            with self.subTest(status=status):
                self.urlopen.return_value = FakeResponse(status)
                run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))

    def test_bare_oserror_does_not_raise(self) -> None:
        # http.client / socket can surface bare OSErrors that are not URLError.
        # Metering is best-effort and must never fail the proxied LLM call.
        self.urlopen.side_effect = ConnectionResetError("connection reset")
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))

    def test_unexpected_runtime_error_does_not_raise(self) -> None:
        self.urlopen.side_effect = RuntimeError("unexpected")
        run(callbacks.send_metering_event("success", metadata_kwargs(skquad_agent_id="a", skquad_squad_id="s"), {}, ""))


class CallbackEventTests(EnvMixin):
    def setUp(self):
        super().setUp()
        send = mock.patch.object(callbacks, "send_metering_event")
        self.send = send.start()
        self.addCleanup(send.stop)

    def test_success_event_status(self) -> None:
        callback = callbacks.SkquadMeteringCallback()
        run(callback.async_log_success_event({"model": "m"}, {"usage": {}}, None, None))
        self.send.assert_called_once()
        args = self.send.call_args.args
        self.assertEqual(args[0], "success")
        self.assertEqual(args[3], "")

    def test_failure_event_captures_error(self) -> None:
        callback = callbacks.SkquadMeteringCallback()
        run(callback.async_log_failure_event({"model": "m"}, ValueError("upstream blew up"), None, None))
        args = self.send.call_args.args
        self.assertEqual(args[0], "failure")
        self.assertEqual(args[3], "upstream blew up")

    def test_failure_event_with_none_response(self) -> None:
        callback = callbacks.SkquadMeteringCallback()
        run(callback.async_log_failure_event({"model": "m"}, None, None, None))
        self.assertEqual(self.send.call_args.args[3], "")

    def test_proxy_handler_instance_is_exported(self) -> None:
        # config.yaml wires this exact attribute name.
        self.assertIsInstance(callbacks.proxy_handler_instance, callbacks.SkquadMeteringCallback)


class CallbackWiringTests(unittest.TestCase):
    """The gateway config and the callback must agree with the API contract."""

    def test_config_references_proxy_handler_instance(self) -> None:
        config = (GATEWAY_DIR / "config.yaml").read_text()
        self.assertIn("skquad_litellm_callbacks.proxy_handler_instance", config)

    def test_metering_payload_keys_match_control_plane_contract(self) -> None:
        """Every key the callback sends must exist on gatewayMeteringRequest.

        A renamed key is silently dropped by Go's JSON decoder, which loses
        billing data without any error surfacing.
        """
        server = REPO_ROOT / "control-plane" / "internal" / "httpapi" / "server.go"
        if not server.exists():  # pragma: no cover - only outside a repo checkout
            self.skipTest("control-plane source not available")

        source = server.read_text()
        start = source.index("type gatewayMeteringRequest struct")
        body = source[start : source.index("\n}", start)]
        contract_keys = set()
        for line in body.splitlines():
            marker = '`json:"'
            if marker in line:
                contract_keys.add(line.split(marker, 1)[1].split('"', 1)[0])

        payload_keys = {
            "status",
            "agent_id",
            "squad_id",
            "task_id",
            "provider_id",
            "model",
            "input_tokens",
            "output_tokens",
            "cost",
            "currency",
            "error",
        }
        missing = payload_keys - contract_keys
        self.assertFalse(missing, f"payload keys absent from gatewayMeteringRequest: {sorted(missing)}")


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
