"""S-T6 (2026-11-04): unit tests for the agent git credential helper."""

from __future__ import annotations

import io
import unittest
from unittest import mock

from skquad_runtime import git_credential as gc


def _env(tmp_path=None, **overrides):
    env = {
        "SKQUAD_AGENT_ID": "agent-abc",
        "SKQUAD_TOOL_GATEWAY_URL": (
            "http://skquad-tool-gateway.skquad-system.svc.cluster.local:8080"
        ),
        "SKQUAD_AGENT_CREDENTIAL_PATH": str(_cred_file(tmp_path)),
    }
    env.update(overrides)
    return env


def _cred_file(tmp_path=None):
    import tempfile
    import os

    fd, path = tempfile.mkstemp(prefix="cred-")
    with os.fdopen(fd, "w") as fh:
        fh.write("s3cret-agent-cred\n")
    return path


class GatewayHostTests(unittest.TestCase):
    def test_host_with_port(self):
        self.assertEqual(
            gc.gateway_host({"SKQUAD_TOOL_GATEWAY_URL": "http://gw.internal:8080"}),
            "gw.internal:8080",
        )

    def test_host_without_port(self):
        self.assertEqual(gc.gateway_host({"SKQUAD_TOOL_GATEWAY_URL": "http://gw.internal"}), "gw.internal")

    def test_https_default_port_not_included(self):
        self.assertEqual(
            gc.gateway_host({"SKQUAD_TOOL_GATEWAY_URL": "https://gw.internal:443"}), "gw.internal:443"
        )

    def test_unset(self):
        self.assertEqual(gc.gateway_host({}), "")

    def test_bogus_url(self):
        self.assertEqual(gc.gateway_host({"SKQUAD_TOOL_GATEWAY_URL": "not a url"}), "")


class CredentialForTests(unittest.TestCase):
    def test_gateway_host_gets_agent_identity(self):
        import tempfile
        import os

        fd, cred = tempfile.mkstemp(prefix="cred-")
        with os.fdopen(fd, "w") as fh:
            fh.write("tok123\n")
        env = {
            "SKQUAD_AGENT_ID": "agent-abc",
            "SKQUAD_TOOL_GATEWAY_URL": "http://gw.internal:8080",
            "SKQUAD_AGENT_CREDENTIAL_PATH": cred,
        }
        pair = gc.credential_for({"host": "gw.internal:8080", "protocol": "http"}, env)
        self.assertEqual(pair, ("agent-abc", "tok123"))
        os.unlink(cred)

    def test_other_host_never_answered(self):
        import tempfile
        import os

        fd, cred = tempfile.mkstemp(prefix="cred-")
        with os.fdopen(fd, "w") as fh:
            fh.write("tok123")
        env = {
            "SKQUAD_AGENT_ID": "agent-abc",
            "SKQUAD_TOOL_GATEWAY_URL": "http://gw.internal:8080",
            "SKQUAD_AGENT_CREDENTIAL_PATH": cred,
        }
        for host in ("github.com", "gw.internal.evil", "gw.internal:9090", "npmjs.org"):
            self.assertIsNone(gc.credential_for({"host": host}, env), host)
        os.unlink(cred)

    def test_default_port_normalization(self):
        import tempfile
        import os

        fd, cred = tempfile.mkstemp(prefix="cred-")
        with os.fdopen(fd, "w") as fh:
            fh.write("tok123")
        env = {
            "SKQUAD_AGENT_ID": "a",
            "SKQUAD_TOOL_GATEWAY_URL": "http://gw.internal:80",
            "SKQUAD_AGENT_CREDENTIAL_PATH": cred,
        }
        self.assertEqual(gc.credential_for({"host": "gw.internal"}, env), ("a", "tok123"))
        os.unlink(cred)

    def test_missing_agent_id_silent(self):
        env = _env(SKQUAD_AGENT_ID="")
        self.assertIsNone(gc.credential_for({"host": "gw.internal:8080"}, env))

    def test_missing_credential_file_silent(self):
        env = _env(SKQUAD_AGENT_CREDENTIAL_PATH="/nonexistent/cred")
        self.assertIsNone(gc.credential_for({"host": "gw.internal:8080"}, env))

    def test_empty_credential_silent(self):
        import tempfile
        import os

        fd, cred = tempfile.mkstemp(prefix="cred-")
        os.close(fd)
        env = _env(SKQUAD_AGENT_CREDENTIAL_PATH=cred)
        self.assertIsNone(gc.credential_for({"host": "gw.internal:8080"}, env))
        os.unlink(cred)


class MainTests(unittest.TestCase):
    def test_get_emits_pair(self):
        import tempfile
        import os

        fd, cred = tempfile.mkstemp(prefix="cred-")
        with os.fdopen(fd, "w") as fh:
            fh.write("tok123")
        env = {
            "SKQUAD_AGENT_ID": "agent-abc",
            "SKQUAD_TOOL_GATEWAY_URL": "http://gw.internal:8080",
            "SKQUAD_AGENT_CREDENTIAL_PATH": cred,
        }
        out = io.StringIO()
        rc = gc.main(
            ["get"],
            stdin=io.StringIO("protocol=http\nhost=gw.internal:8080\npath=/git/r/x.git\n"),
            environ=env,
            stdout=out,
        )
        self.assertEqual(rc, 0)
        self.assertEqual(out.getvalue(), "username=agent-abc\npassword=***\n")
        os.unlink(cred)

    def test_store_is_noop(self):
        out = io.StringIO()
        rc = gc.main(["store"], stdin=io.StringIO("host=gw.internal:8080\n"), environ=_env(), stdout=out)
        self.assertEqual(rc, 0)
        self.assertEqual(out.getvalue(), "")

    def test_no_args_is_noop(self):
        out = io.StringIO()
        rc = gc.main([], stdin=io.StringIO(""), environ=_env(), stdout=out)
        self.assertEqual(rc, 0)
        self.assertEqual(out.getvalue(), "")

    def test_non_gateway_host_silent(self):
        out = io.StringIO()
        rc = gc.main(["get"], stdin=io.StringIO("host=github.com\n"), environ=_env(), stdout=out)
        self.assertEqual(rc, 0)
        self.assertEqual(out.getvalue(), "")


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
