"""S-279 file tools tests — read / write / edit / apply_patch.

Focus: happy paths plus the workspace jail (path-escape rejection),
edit uniqueness/atomicity, and apply_patch atomic failure. Stdlib
unittest per repo convention; runs under pytest too.
"""

import os
import pathlib
import tempfile
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


class WorkspaceEscapeTests(unittest.TestCase):
    """_resolve_in_workspace is THE security boundary — test it hard."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ws = pathlib.Path(self.tmp.name) / "ws"
        self.ws.mkdir()
        # A file OUTSIDE the workspace used by escape tests.
        self.outside = pathlib.Path(self.tmp.name) / "outside.txt"
        self.outside.write_text("secret-outside")

    def tearDown(self):
        self.tmp.cleanup()

    def test_relative_inside_ok(self):
        p = bt._resolve_in_workspace(str(self.ws), "sub/file.txt")
        self.assertTrue(str(p).startswith(str(self.ws.resolve())))

    def test_workspace_root_itself_ok(self):
        self.assertEqual(bt._resolve_in_workspace(str(self.ws), "."), self.ws.resolve())

    def test_dotdot_traversal_rejected(self):
        with self.assertRaises(bt.WorkspaceEscapeError):
            bt._resolve_in_workspace(str(self.ws), "../../etc/passwd")

    def test_absolute_outside_rejected(self):
        with self.assertRaises(bt.WorkspaceEscapeError):
            bt._resolve_in_workspace(str(self.ws), str(self.outside))

    def test_absolute_etc_hosts_rejected(self):
        with self.assertRaises(bt.WorkspaceEscapeError):
            bt._resolve_in_workspace(str(self.ws), "/etc/hosts")

    def test_symlink_escape_rejected(self):
        link = self.ws / "escape"
        link.symlink_to(self.outside)
        with self.assertRaises(bt.WorkspaceEscapeError):
            bt._resolve_in_workspace(str(self.ws), "escape")

    def test_symlink_dir_escape_rejected(self):
        outdir = pathlib.Path(self.tmp.name) / "outdir"
        outdir.mkdir()
        link = self.ws / "dlink"
        link.symlink_to(outdir)
        with self.assertRaises(bt.WorkspaceEscapeError):
            bt._resolve_in_workspace(str(self.ws), "dlink/inside.txt")

    def test_empty_path_rejected(self):
        with self.assertRaises(bt.WorkspaceEscapeError):
            bt._resolve_in_workspace(str(self.ws), "")

    def test_symlink_inside_workspace_ok(self):
        target = self.ws / "real.txt"
        target.write_text("fine")
        link = self.ws / "link.txt"
        link.symlink_to(target)
        p = bt._resolve_in_workspace(str(self.ws), "link.txt")
        self.assertEqual(p, target.resolve())


class ReadToolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ws = pathlib.Path(self.tmp.name) / "ws"
        self.ws.mkdir()
        self.tool = bt.ReadTool({"maxBytes": 4096}, ctx(str(self.ws)))

    def tearDown(self):
        self.tmp.cleanup()

    def test_happy_path(self):
        (self.ws / "a.txt").write_text("hello\nworld\n")
        res = self.tool.invoke(call("read", path="a.txt"), None)
        self.assertTrue(res.ok)
        self.assertIn("hello", res.content)
        self.assertIn("world", res.content)

    def test_offset_and_limit(self):
        (self.ws / "n.txt").write_text("\n".join(f"line{i}" for i in range(1, 11)))
        res = self.tool.invoke(call("read", path="n.txt", offset=3, limit=2), None)
        self.assertTrue(res.ok)
        self.assertIn("line3", res.content)
        self.assertIn("line4", res.content)
        self.assertNotIn("line5", res.content)
        self.assertIn("showing lines 3-4 of 10", res.content)

    def test_max_bytes_truncation_notice(self):
        (self.ws / "big.txt").write_text("x" * 10000)
        tool = bt.ReadTool({"maxBytes": 100}, ctx(str(self.ws)))
        res = tool.invoke(call("read", path="big.txt"), None)
        self.assertTrue(res.ok)
        self.assertIn("truncated", res.content)
        self.assertLessEqual(len(res.content.encode()), 200)

    def test_image_returns_attachment_marker(self):
        img = self.ws / "pic.PNG"
        img.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * 100)
        res = self.tool.invoke(call("read", path="pic.PNG"), None)
        self.assertTrue(res.ok)
        self.assertIn("attachment", res.content)
        self.assertIn(".png", res.content)
        self.assertNotIn("\x00", res.content)

    def test_missing_file(self):
        res = self.tool.invoke(call("read", path="nope.txt"), None)
        self.assertFalse(res.ok)
        self.assertIn("not found", res.content)

    def test_escape_rejected(self):
        res = self.tool.invoke(call("read", path="../../etc/passwd"), None)
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)

    def test_absolute_outside_rejected(self):
        res = self.tool.invoke(call("read", path="/etc/passwd"), None)
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)


class WriteToolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ws = pathlib.Path(self.tmp.name) / "ws"
        self.ws.mkdir()
        self.tool = bt.WriteTool({"maxBytes": 4096}, ctx(str(self.ws)))

    def tearDown(self):
        self.tmp.cleanup()

    def test_happy_path_creates_parents(self):
        res = self.tool.invoke(call("write", path="deep/dir/f.txt", content="hi\n"), None)
        self.assertTrue(res.ok)
        self.assertEqual((self.ws / "deep/dir/f.txt").read_text(), "hi\n")
        self.assertIn("deep/dir/f.txt", res.content)

    def test_overwrites(self):
        (self.ws / "f.txt").write_text("old")
        res = self.tool.invoke(call("write", path="f.txt", content="new"), None)
        self.assertTrue(res.ok)
        self.assertEqual((self.ws / "f.txt").read_text(), "new")

    def test_max_bytes_rejected(self):
        res = self.tool.invoke(call("write", path="f.txt", content="x" * 5000), None)
        self.assertFalse(res.ok)
        self.assertIn("maxBytes", res.content)
        self.assertFalse((self.ws / "f.txt").exists())

    def test_missing_content_rejected(self):
        res = self.tool.invoke(call("write", path="f.txt"), None)
        self.assertFalse(res.ok)

    def test_escape_rejected(self):
        outside = pathlib.Path(self.tmp.name) / "outside.txt"
        res = self.tool.invoke(call("write", path="../outside.txt", content="pwned"), None)
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)
        self.assertFalse(outside.exists())

    def test_absolute_outside_rejected(self):
        res = self.tool.invoke(call("write", path="/tmp/s279-should-not-exist.txt", content="x"), None)
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)


class EditToolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ws = pathlib.Path(self.tmp.name) / "ws"
        self.ws.mkdir()
        self.tool = bt.EditTool({"maxBytes": 65536}, ctx(str(self.ws)))

    def tearDown(self):
        self.tmp.cleanup()

    def test_happy_multi_edit(self):
        f = self.ws / "code.py"
        f.write_text("alpha\nbeta\ngamma\n")
        res = self.tool.invoke(
            call(
                "edit",
                path="code.py",
                edits=[
                    {"oldText": "alpha", "newText": "ALPHA"},
                    {"oldText": "gamma", "newText": "GAMMA"},
                ],
            ),
            None,
        )
        self.assertTrue(res.ok)
        self.assertEqual(f.read_text(), "ALPHA\nbeta\nGAMMA\n")

    def test_ambiguous_oldtext_writes_nothing(self):
        f = self.ws / "dup.txt"
        original = "foo bar foo baz\n"
        f.write_text(original)
        res = self.tool.invoke(
            call("edit", path="dup.txt", edits=[{"oldText": "foo", "newText": "QUX"}]), None
        )
        self.assertFalse(res.ok)
        self.assertIn("ambiguous", res.content)
        self.assertEqual(f.read_text(), original)  # untouched

    def test_missing_oldtext_writes_nothing(self):
        f = self.ws / "m.txt"
        original = "one two\n"
        f.write_text(original)
        res = self.tool.invoke(
            call(
                "edit",
                path="m.txt",
                edits=[
                    {"oldText": "one", "newText": "ONE"},
                    {"oldText": "absent", "newText": "X"},
                ],
            ),
            None,
        )
        self.assertFalse(res.ok)
        self.assertEqual(f.read_text(), original)  # all-or-nothing

    def test_overlapping_edits_rejected(self):
        f = self.ws / "ov.txt"
        original = "abcdef\n"
        f.write_text(original)
        res = self.tool.invoke(
            call(
                "edit",
                path="ov.txt",
                edits=[
                    {"oldText": "abcd", "newText": "W"},
                    {"oldText": "cdef", "newText": "V"},
                ],
            ),
            None,
        )
        self.assertFalse(res.ok)
        self.assertIn("overlap", res.content)
        self.assertEqual(f.read_text(), original)

    def test_missing_file(self):
        res = self.tool.invoke(call("edit", path="ghost.txt", edits=[{"oldText": "a", "newText": "b"}]), None)
        self.assertFalse(res.ok)

    def test_escape_rejected(self):
        res = self.tool.invoke(
            call("edit", path="../../etc/passwd", edits=[{"oldText": "x", "newText": "y"}]), None
        )
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)

    def test_empty_edits_rejected(self):
        f = self.ws / "e.txt"
        f.write_text("x")
        res = self.tool.invoke(call("edit", path="e.txt", edits=[]), None)
        self.assertFalse(res.ok)


class ApplyPatchToolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ws = pathlib.Path(self.tmp.name) / "ws"
        self.ws.mkdir()
        self.tool = bt.ApplyPatchTool({"maxBytes": 65536}, ctx(str(self.ws)))

    def tearDown(self):
        self.tmp.cleanup()

    def test_add_update_delete(self):
        (self.ws / "keep.txt").write_text("line1\nline2\nline3\n")
        (self.ws / "gone.txt").write_text("bye\n")
        patch = "\n".join([
            "*** Begin Patch",
            "*** Add File: new.txt",
            "+created by patch",
            "*** Update File: keep.txt",
            "@@",
            " line1",
            "-line2",
            "+LINE TWO",
            " line3",
            "*** Delete File: gone.txt",
            "*** End Patch",
        ])
        res = self.tool.invoke(call("apply_patch", patch=patch), None)
        self.assertTrue(res.ok, res.content)
        self.assertEqual((self.ws / "new.txt").read_text(), "created by patch\n")
        self.assertEqual((self.ws / "keep.txt").read_text(), "line1\nLINE TWO\nline3\n")
        self.assertFalse((self.ws / "gone.txt").exists())

    def test_multi_file_update(self):
        (self.ws / "a.txt").write_text("a1\na2\n")
        (self.ws / "b.txt").write_text("b1\nb2\n")
        patch = "\n".join([
            "*** Begin Patch",
            "*** Update File: a.txt",
            "@@",
            " a1",
            "-a2",
            "+A2",
            "*** Update File: b.txt",
            "@@",
            " b1",
            "-b2",
            "+B2",
            "*** End Patch",
        ])
        res = self.tool.invoke(call("apply_patch", patch=patch), None)
        self.assertTrue(res.ok, res.content)
        self.assertEqual((self.ws / "a.txt").read_text(), "a1\nA2\n")
        self.assertEqual((self.ws / "b.txt").read_text(), "b1\nB2\n")

    def test_bad_hunk_writes_nothing(self):
        (self.ws / "a.txt").write_text("a1\na2\n")
        (self.ws / "b.txt").write_text("b1\nb2\n")
        patch = "\n".join([
            "*** Begin Patch",
            "*** Update File: a.txt",
            "@@",
            " a1",
            "-a2",
            "+A2",
            "*** Update File: b.txt",
            "@@",
            " WRONG-CONTEXT",
            "-b2",
            "+B2",
            "*** End Patch",
        ])
        res = self.tool.invoke(call("apply_patch", patch=patch), None)
        self.assertFalse(res.ok)
        self.assertIn("b.txt", res.content)
        self.assertIn("hunk", res.content)
        # ATOMIC: a.txt must be untouched even though its hunk matched.
        self.assertEqual((self.ws / "a.txt").read_text(), "a1\na2\n")
        self.assertEqual((self.ws / "b.txt").read_text(), "b1\nb2\n")

    def test_add_existing_file_rejected(self):
        (self.ws / "exists.txt").write_text("x\n")
        patch = "\n".join([
            "*** Begin Patch",
            "*** Add File: exists.txt",
            "+nope",
            "*** End Patch",
        ])
        res = self.tool.invoke(call("apply_patch", patch=patch), None)
        self.assertFalse(res.ok)
        self.assertEqual((self.ws / "exists.txt").read_text(), "x\n")

    def test_malformed_patch_rejected(self):
        res = self.tool.invoke(call("apply_patch", patch="not a patch"), None)
        self.assertFalse(res.ok)

    def test_escape_rejected(self):
        patch = "\n".join([
            "*** Begin Patch",
            "*** Add File: ../../escaped.txt",
            "+pwned",
            "*** End Patch",
        ])
        res = self.tool.invoke(call("apply_patch", patch=patch), None)
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)
        self.assertFalse((pathlib.Path(self.tmp.name) / "escaped.txt").exists())

    def test_delete_escape_rejected(self):
        patch = "\n".join([
            "*** Begin Patch",
            "*** Delete File: ../outside.txt",
            "*** End Patch",
        ])
        outside = pathlib.Path(self.tmp.name) / "outside.txt"
        outside.write_text("precious")
        res = self.tool.invoke(call("apply_patch", patch=patch), None)
        self.assertFalse(res.ok)
        self.assertIn("escapes workspace", res.content)
        self.assertEqual(outside.read_text(), "precious")

    def test_max_bytes_caps_total_write(self):
        patch = "\n".join([
            "*** Begin Patch",
            "*** Add File: big.txt",
            "+" + "y" * 5000,
            "*** End Patch",
        ])
        tool = bt.ApplyPatchTool({"maxBytes": 1000}, ctx(str(self.ws)))
        res = tool.invoke(call("apply_patch", patch=patch), None)
        self.assertFalse(res.ok)
        self.assertIn("maxBytes", res.content)
        self.assertFalse((self.ws / "big.txt").exists())


class RegistryWiringTests(unittest.TestCase):
    def test_file_tools_registered(self):
        for name in ("read", "write", "edit", "apply_patch"):
            self.assertIn(name, bt._BUILTIN_REGISTRY)

    def test_build_builtin_tools_instantiates_enabled_file_tools(self):
        cfg = {
            "tools": [
                {"name": "read", "enabled": True, "policy": {"maxBytes": 123}},
                {"name": "write", "enabled": True, "policy": {}},
                {"name": "edit", "enabled": False, "policy": {}},
                {"name": "apply_patch", "enabled": True, "policy": {}},
            ]
        }
        tools = bt.build_builtin_tools(cfg, ctx("/tmp/ws"))
        names = sorted(t.name for t in tools)
        self.assertEqual(names, ["apply_patch", "read", "write"])
        read_tool = next(t for t in tools if t.name == "read")
        self.assertEqual(read_tool.policy["maxBytes"], 123)


if __name__ == "__main__":
    unittest.main()
