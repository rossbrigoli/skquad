"""Unit tests for scripts/go-coverage-gate.py (S-189, test-only)."""

import sys

import pytest

from helpers import load_script

gcg = load_script("go_coverage_gate", "go-coverage-gate.py")


def make_profile(tmp_path, body):
    path = tmp_path / "cover.out"
    path.write_text(body, encoding="utf-8")
    return path


# ---------------------------------------------------------------------------
# merge_profile
# ---------------------------------------------------------------------------

def test_merge_profile_merges_duplicate_blocks_covered_by_any_binary(tmp_path):
    # The same block emitted by two test binaries: covered if *any* hit it.
    p = make_profile(
        tmp_path,
        "mode: count\n"
        "pkg/a.go:1.1,3.2 2 0\n"
        "pkg/a.go:1.1,3.2 2 5\n"
        "pkg/b.go:4.1,6.2 3 0\n",
    )
    covered, total = gcg.merge_profile(p)
    assert total == 5  # 2 + 3; the duplicate collapses
    assert covered == 2  # a.go block hit by the second binary


def test_merge_profile_all_covered(tmp_path):
    p = make_profile(
        tmp_path,
        "mode: set\n"
        "pkg/a.go:1.1,3.2 2 1\n"
        "pkg/b.go:4.1,6.2 3 7\n",
    )
    covered, total = gcg.merge_profile(p)
    assert (covered, total) == (5, 5)


def test_merge_profile_blank_lines_and_mode_ignored(tmp_path):
    p = make_profile(tmp_path, "mode: count\n\n   \n")
    assert gcg.merge_profile(p) == (0, 0)


def test_merge_profile_malformed_count_returns_negative(tmp_path):
    p = make_profile(tmp_path, "mode: count\npkg/a.go:1.1,3.2 2 notanumber\n")
    assert gcg.merge_profile(p) == (-1, -1)


def test_merge_profile_malformed_statement_count_returns_negative(tmp_path):
    p = make_profile(tmp_path, "mode: count\npkg/a.go:1.1,3.2 x 1\n")
    assert gcg.merge_profile(p) == (-1, -1)


# ---------------------------------------------------------------------------
# main (CLI behaviour via monkeypatched argv)
# ---------------------------------------------------------------------------

def run_main(monkeypatch, argv):
    monkeypatch.setattr(sys, "argv", ["go-coverage-gate.py", *argv])
    return gcg.main()


def test_main_missing_profile_exits_2(tmp_path, monkeypatch):
    missing = tmp_path / "nope.out"
    assert run_main(monkeypatch, [str(missing), "--min", "50"]) == 2


def test_main_empty_profile_exits_2(tmp_path, monkeypatch):
    p = make_profile(tmp_path, "mode: count\n")
    assert run_main(monkeypatch, [str(p), "--min", "50"]) == 2


def test_main_malformed_profile_exits_2(tmp_path, monkeypatch):
    p = make_profile(tmp_path, "mode: count\nbogus line without numbers x y\n")
    assert run_main(monkeypatch, [str(p), "--min", "50"]) == 2


def test_main_below_floor_fails(tmp_path, monkeypatch):
    # 1 of 4 statements covered = 25% < 60%
    p = make_profile(
        tmp_path,
        "mode: count\n"
        "pkg/a.go:1.1,2.2 1 3\n"
        "pkg/b.go:1.1,2.2 3 0\n",
    )
    assert run_main(monkeypatch, [str(p), "--min", "60"]) == 1


def test_main_at_floor_passes(tmp_path, monkeypatch):
    # 1 of 2 statements = 50%, floor 50 → pass (strictly-below comparison)
    p = make_profile(
        tmp_path,
        "mode: count\n"
        "pkg/a.go:1.1,2.2 1 1\n"
        "pkg/b.go:1.1,2.2 1 0\n",
    )
    assert run_main(monkeypatch, [str(p), "--min", "50"]) == 0


def test_main_above_floor_passes(tmp_path, monkeypatch):
    p = make_profile(tmp_path, "mode: set\npkg/a.go:1.1,2.2 4 1\n")
    assert run_main(monkeypatch, [str(p), "--min", "80"]) == 0
