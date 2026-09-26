"""Unit tests for the S-141 release-version computation.

Run with: python3 -m unittest discover -s .github/actions/skquad-version -p 'test_*.py'
"""

from __future__ import annotations

import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from compute_version import VersionError, compute_build, load_baseline, main, normalize_commit


def write_baseline(tmpdir: str, **overrides) -> Path:
    payload = {"major": 0, "minor": 1, "buildBase": 99, "runBase": 147}
    payload.update(overrides)
    path = Path(tmpdir) / "version.json"
    path.write_text(json.dumps(payload), encoding="utf-8")
    return path


class LoadBaselineTest(unittest.TestCase):
    def test_reads_all_four_fields(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(load_baseline(write_baseline(tmp)), (0, 1, 99, 147))

    def test_missing_file_raises(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(VersionError):
                load_baseline(Path(tmp) / "nope.json")

    def test_bad_json_raises(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "version.json"
            path.write_text("{not json", encoding="utf-8")
            with self.assertRaises(VersionError):
                load_baseline(path)

    def test_missing_key_raises(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = write_baseline(tmp)
            payload = json.loads(path.read_text(encoding="utf-8"))
            del payload["runBase"]
            path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaisesRegex(VersionError, "runBase"):
                load_baseline(path)

    def test_bool_is_not_an_integer(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = write_baseline(tmp, major=True)
            with self.assertRaisesRegex(VersionError, "integer"):
                load_baseline(path)

    def test_negative_raises(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = write_baseline(tmp, buildBase=-1)
            with self.assertRaisesRegex(VersionError, "range"):
                load_baseline(path)


class ComputeBuildTest(unittest.TestCase):
    def test_first_release_after_baseline_hits_100(self) -> None:
        # buildBase 99 @ runBase 147 -> run 148 is 0.1.100 (S-141 starting point).
        self.assertEqual(compute_build(99, 147, 148), 100)

    def test_increments_one_per_run(self) -> None:
        self.assertEqual([compute_build(99, 147, run) for run in (148, 149, 150)], [100, 101, 102])

    def test_replayed_old_run_is_refused(self) -> None:
        for run in (146, 147):
            with self.assertRaises(VersionError):
                compute_build(99, 147, run)


class NormalizeCommitTest(unittest.TestCase):
    def test_full_sha_shortens_to_seven(self) -> None:
        self.assertEqual(
            normalize_commit("0123456789abcdef" * 2 + "012345"),
            ("0123456789abcdef0123456789abcdef012345", "0123456"),
        )

    def test_uppercase_is_lowered(self) -> None:
        full, short = normalize_commit("DEADBEEF1234567")
        self.assertEqual((full, short), ("deadbeef1234567", "deadbee"))

    def test_blank_becomes_unknown(self) -> None:
        self.assertEqual(normalize_commit("  "), ("unknown", "unknown"))

    def test_non_hex_raises(self) -> None:
        with self.assertRaises(VersionError):
            normalize_commit("not-a-sha")


class MainTest(unittest.TestCase):
    def test_emits_key_value_outputs(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = write_baseline(tmp)
            buf = io.StringIO()
            with mock.patch("sys.stdout", buf), mock.patch("sys.stderr", io.StringIO()):
                rc = main(["--file", str(path), "--run-number", "148", "--commit", "abc1234def5678"])
            self.assertEqual(rc, 0)
            out = dict(line.split("=", 1) for line in buf.getvalue().splitlines() if "=" in line)
            self.assertEqual(out["version"], "0.1.100")
            self.assertEqual(out["version_tag"], "v0.1.100")
            self.assertEqual(out["commit_short"], "abc1234")

    def test_version_series_is_a_valid_tag_suffix(self) -> None:
        # Images run 148 built `repo:.` because the workflow concatenated
        # major/minor itself and the outputs were not forwarded. The action now
        # emits the series as one value; assert it is always "<n>.<n>".
        with tempfile.TemporaryDirectory() as tmp:
            path = write_baseline(tmp, major=12, minor=34)
            buf = io.StringIO()
            with mock.patch("sys.stdout", buf), mock.patch("sys.stderr", io.StringIO()):
                rc = main(["--file", str(path), "--run-number", "148", "--commit", "abc1234"])
            self.assertEqual(rc, 0)
            out = dict(line.split("=", 1) for line in buf.getvalue().splitlines() if "=" in line)
            self.assertEqual(out["version_series"], "12.34")
            self.assertRegex(out["version_series"], r"^[0-9]+\.[0-9]+$")
            self.assertEqual(out["version"], "12.34.100")

    def test_exit_one_on_bad_baseline(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with mock.patch("sys.stderr", io.StringIO()):
                rc = main(["--file", str(Path(tmp) / "missing.json"), "--run-number", "148"])
            self.assertEqual(rc, 1)

    def test_exit_one_on_replay(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = write_baseline(tmp)
            with mock.patch("sys.stderr", io.StringIO()):
                rc = main(["--file", str(path), "--run-number", "147", "--commit", "abc1234"])
            self.assertEqual(rc, 1)


if __name__ == "__main__":
    unittest.main()
