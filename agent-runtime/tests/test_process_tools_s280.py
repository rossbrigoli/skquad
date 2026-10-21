"""S-280 background process tools tests — exec(background) + process tool.

Covers: start+poll+log roundtrip, write to stdin, kill kills the whole
process group, list, clear, session-cap enforcement, log-cap truncation/
kill, and yieldMs early-return on a fast command. Stdlib unittest per
repo convention; runs under pytest too.
"""

import json
import os
import pathlib
import tempfile
import time
import unittest

from skquad_runtime import builtin_tools as bt
from skquad_runtime.runtime import ToolCall


def ctx(root: str) -> bt.BuiltinToolContext:
    return bt.BuiltinToolContext(
        workspace_dir=root,
        control_plane_url="http://cp.invalid",
        agent_credential="cred",
        agent_id="agent-1",
    )


def call(name: str, **args) -> ToolCall:
    return ToolCall(id="c1", name=name, arguments=args)


class ProcessToolsS280Tests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ws = str(pathlib.Path(self.tmp.name) / "ws")
        os.makedirs(self.ws)
        self.exec_tool = bt.ExecTool({}, ctx(self.ws))
        self.proc_tool = bt.ProcessTool({}, ctx(self.ws))

    def tearDown(self):
        # Reap anything still running so the temp dir cleanup is clean.
        for sid, proc in list(bt._PROCESS_HANDLES.items()):
            try:
                os.killpg(os.getpgid(proc.pid), 9)
            except (ProcessLookupError, PermissionError, OSError):
                pass
            try:
                proc.kill()
                proc.wait(timeout=2)
            except Exception:
                pass
        bt._PROCESS_HANDLES.clear()
        self.tmp.cleanup()

    # -- helpers ---------------------------------------------------------

    def _start(self, command, yield_ms=200):
        res = self.exec_tool.invoke(
            call("exec", command=command, background=True, yieldMs=yield_ms), None
        )
        self.assertTrue(res.ok, f"start failed: {res.content}")
        sid = res.content.split()[3]
        self.assertTrue(sid.startswith("p"), res.content)
        return sid, res

    # -- yieldMs early return -------------------------------------------

    def test_fast_command_returns_foreground_result(self):
        res = self.exec_tool.invoke(
            call("exec", command="echo quick-out", background=True, yieldMs=5000), None
        )
        self.assertTrue(res.ok)
        self.assertIn("exit code: 0", res.content)
        self.assertIn("quick-out", res.content)
        # No background session should linger as running.
        lst = self.proc_tool.invoke(call("process", action="list"), None)
        self.assertNotIn("running", lst.content)

    def test_slow_command_returns_session_id(self):
        sid, res = self._start("sleep 30")
        self.assertIn("started background session", res.content)
        self.assertIn(f"sessionId={sid}", res.content)

    # -- start + poll + log roundtrip ------------------------------------

    def test_start_poll_log_roundtrip(self):
        sid, _ = self._start("sleep 0.4; echo hello-log; echo err-log 1>&2; exit 3")
        # Wait for completion via poll with a wait window.
        res = self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=5), None
        )
        self.assertTrue(res.ok)
        self.assertIn("[exited]", res.content)
        self.assertIn("exit=3", res.content)
        log = self.proc_tool.invoke(call("process", action="log", sessionId=sid), None)
        self.assertIn("hello-log", log.content)
        self.assertIn("err-log", log.content)  # stderr captured too

    def test_poll_waits_for_completion(self):
        sid, _ = self._start("sleep 1; echo later")
        t0 = time.monotonic()
        res = self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=8), None
        )
        elapsed = time.monotonic() - t0
        self.assertIn("[exited]", res.content)
        self.assertGreaterEqual(elapsed, 0.8)  # it actually waited

    def test_log_offset_limit(self):
        sid, _ = self._start("sleep 0.4; seq 1 10")
        self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=5), None
        )
        log = self.proc_tool.invoke(
            call("process", action="log", sessionId=sid, offset=3, limit=2), None
        )
        self.assertIn("3", log.content)
        self.assertIn("4", log.content)
        self.assertNotIn("\n5", log.content)

    # -- registry --------------------------------------------------------

    def test_registry_persists_fields(self):
        sid, _ = self._start("sleep 20")
        reg_path = pathlib.Path(self.ws) / ".skquad-process" / "sessions.json"
        self.assertTrue(reg_path.is_file())
        data = json.loads(reg_path.read_text())
        self.assertIn(sid, data)
        s = data[sid]
        for field in ("id", "command", "pid", "pgid", "started_at", "status", "exit_code", "log_path"):
            self.assertIn(field, s)
        self.assertEqual(s["status"], "running")
        # Log file lives inside the workspace jail.
        log_abs = (pathlib.Path(self.ws) / s["log_path"]).resolve()
        self.assertTrue(str(log_abs).startswith(str(pathlib.Path(self.ws).resolve())))

    # -- list ------------------------------------------------------------

    def test_list_shows_sessions(self):
        sid, _ = self._start("sleep 25")
        lst = self.proc_tool.invoke(call("process", action="list"), None)
        self.assertTrue(lst.ok)
        self.assertIn(sid, lst.content)
        self.assertIn("running", lst.content)

    # -- write to stdin ----------------------------------------------------

    def test_write_stdin(self):
        sid, _ = self._start("read line; echo got:$line")
        w = self.proc_tool.invoke(
            call("process", action="write", sessionId=sid, data="ping\n"), None
        )
        self.assertTrue(w.ok, w.content)
        res = self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=5), None
        )
        self.assertIn("[exited]", res.content)
        log = self.proc_tool.invoke(call("process", action="log", sessionId=sid), None)
        self.assertIn("got:ping", log.content)

    def test_write_to_finished_session_fails(self):
        sid, _ = self._start("sleep 0.4; echo done-immediately")
        self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=5), None
        )
        w = self.proc_tool.invoke(
            call("process", action="write", sessionId=sid, data="too late\n"), None
        )
        self.assertFalse(w.ok)
        self.assertIn("not running", w.content)

    # -- kill kills the whole group ---------------------------------------

    def test_kill_kills_child_processes(self):
        sid, _ = self._start("sh -c 'sleep 300 & echo CHILD=$!; wait'")
        # Grab the child pid from the captured log.
        deadline = time.monotonic() + 5
        child_pid = None
        while time.monotonic() < deadline and child_pid is None:
            log = self.proc_tool.invoke(
                call("process", action="log", sessionId=sid), None
            )
            for token in log.content.split():
                if token.startswith("CHILD="):
                    child_pid = int(token.split("=", 1)[1])
            if child_pid is None:
                time.sleep(0.1)
        self.assertIsNotNone(child_pid, "child pid never appeared in log")
        os.kill(child_pid, 0)  # must be alive pre-kill

        k = self.proc_tool.invoke(call("process", action="kill", sessionId=sid), None)
        self.assertTrue(k.ok, k.content)
        self.assertIn("killed", k.content)
        time.sleep(0.3)
        with self.assertRaises(ProcessLookupError):
            os.kill(child_pid, 0)  # child died with the group
        poll = self.proc_tool.invoke(call("process", action="poll", sessionId=sid), None)
        self.assertIn("[killed]", poll.content)

    def test_kill_finished_session_is_noop_ok(self):
        sid, _ = self._start("sleep 0.4; echo bye")
        self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=5), None
        )
        k = self.proc_tool.invoke(call("process", action="kill", sessionId=sid), None)
        self.assertTrue(k.ok)
        self.assertIn("already exited", k.content)

    # -- clear -------------------------------------------------------------

    def test_clear_removes_record_and_log(self):
        sid, _ = self._start("sleep 0.4; echo to-be-cleared")
        self.proc_tool.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=5), None
        )
        c = self.proc_tool.invoke(call("process", action="clear", sessionId=sid), None)
        self.assertTrue(c.ok)
        lst = self.proc_tool.invoke(call("process", action="list"), None)
        self.assertNotIn(sid, lst.content)
        reg = json.loads(
            (pathlib.Path(self.ws) / ".skquad-process" / "sessions.json").read_text()
        )
        self.assertNotIn(sid, reg)

    def test_clear_running_session_rejected(self):
        sid, _ = self._start("sleep 25")
        c = self.proc_tool.invoke(call("process", action="clear", sessionId=sid), None)
        self.assertFalse(c.ok)
        self.assertIn("still running", c.content)

    # -- session cap ---------------------------------------------------------

    def test_session_cap_enforced(self):
        capped = bt.ExecTool({"maxSessions": 2}, ctx(self.ws))
        r1 = capped.invoke(call("exec", command="sleep 30", background=True, yieldMs=100), None)
        self.assertTrue(r1.ok)
        r2 = capped.invoke(call("exec", command="sleep 30", background=True, yieldMs=100), None)
        self.assertTrue(r2.ok)
        r3 = capped.invoke(call("exec", command="sleep 30", background=True, yieldMs=100), None)
        self.assertFalse(r3.ok)
        self.assertIn("cap reached", r3.content)

    def test_cap_frees_after_kill(self):
        capped = bt.ExecTool({"maxSessions": 1}, ctx(self.ws))
        proc_capped = bt.ProcessTool({"maxSessions": 1}, ctx(self.ws))
        r1 = capped.invoke(call("exec", command="sleep 30", background=True, yieldMs=100), None)
        sid = r1.content.split()[3]
        r2 = capped.invoke(call("exec", command="sleep 30", background=True, yieldMs=100), None)
        self.assertFalse(r2.ok)
        proc_capped.invoke(call("process", action="kill", sessionId=sid), None)
        r3 = capped.invoke(call("exec", command="sleep 30", background=True, yieldMs=100), None)
        self.assertTrue(r3.ok, r3.content)

    # -- log cap --------------------------------------------------------------

    def test_log_cap_kills_and_truncates(self):
        capped = bt.ExecTool({"maxLogBytes": 200}, ctx(self.ws))
        proc_capped = bt.ProcessTool({"maxLogBytes": 200}, ctx(self.ws))
        res = capped.invoke(
            call(
                "exec",
                command="seq 1 5000; sleep 300",
                background=True,
                yieldMs=100,
            ),
            None,
        )
        self.assertTrue(res.ok)
        sid = res.content.split()[3]
        poll = proc_capped.invoke(
            call("process", action="poll", sessionId=sid, timeoutSeconds=10), None
        )
        self.assertIn("killed", poll.content)
        self.assertIn("maxLogBytes", poll.content)
        log = proc_capped.invoke(call("process", action="log", sessionId=sid), None)
        self.assertIn("log truncated at maxLogBytes=200", log.content)
        self.assertLess(len(log.content.encode("utf-8")), 1000)

    # -- denylist still applies in background ----------------------------------

    def test_background_respects_denylist(self):
        res = self.exec_tool.invoke(
            call("exec", command="rm -rf /", background=True, yieldMs=100), None
        )
        self.assertFalse(res.ok)
        self.assertIn("denied by policy", res.content)

    # -- misc validation ---------------------------------------------------------

    def test_unknown_action_rejected(self):
        res = self.proc_tool.invoke(call("process", action="explode"), None)
        self.assertFalse(res.ok)

    def test_unknown_session_rejected(self):
        res = self.proc_tool.invoke(
            call("process", action="poll", sessionId="pdeadbeef"), None
        )
        self.assertFalse(res.ok)
        self.assertIn("unknown session", res.content)

    def test_missing_session_id_rejected(self):
        res = self.proc_tool.invoke(call("process", action="kill"), None)
        self.assertFalse(res.ok)


if __name__ == "__main__":
    unittest.main()
