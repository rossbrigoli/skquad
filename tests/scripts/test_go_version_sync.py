"""Unit tests for scripts/go-version-sync.py (S-266, test-only)."""

import pytest

from helpers import load_script

gvs = load_script("go_version_sync", "go-version-sync.py")


def make_repo(tmp_path, go_version="1.26.9", gomod="toolchain go1.26.9",
             dockerfile="ARG GO_VERSION=1.26.9", ci="go-version: '1.26.9'"):
    (tmp_path / ".go-version").write_text(go_version + "\n", encoding="utf-8")
    if gomod is not None:
        mod = tmp_path / "mymod"
        mod.mkdir()
        (mod / "go.mod").write_text(
            "module example.com/mymod\n\ngo 1.26.0\n\n" + gomod + "\n", encoding="utf-8"
        )
    if dockerfile is not None:
        comp = tmp_path / "mycomp"
        comp.mkdir()
        (comp / "Dockerfile").write_text(
            "FROM alpine\n" + dockerfile + "\n", encoding="utf-8"
        )
    ci_dir = tmp_path / ".github" / "workflows"
    ci_dir.mkdir(parents=True)
    (ci_dir / "ci.yml").write_text(
        "jobs:\n  x:\n    steps:\n      - uses: actions/setup-go@v7\n"
        "        with:\n          " + ci + "\n", encoding="utf-8"
    )
    return tmp_path


# ---------------------------------------------------------------------------
# read_expected
# ---------------------------------------------------------------------------

def test_read_expected_plain(tmp_path):
    make_repo(tmp_path)
    assert gvs.read_expected(tmp_path) == "1.26.9"


def test_read_expected_accepts_go_prefix(tmp_path):
    make_repo(tmp_path, go_version="go1.26.9")
    assert gvs.read_expected(tmp_path) == "1.26.9"


def test_read_expected_missing_file(tmp_path):
    assert gvs.read_expected(tmp_path) == ""


def test_read_expected_invalid(tmp_path):
    make_repo(tmp_path, go_version="not-a-version")
    assert gvs.read_expected(tmp_path) == ""


# ---------------------------------------------------------------------------
# check_gomod
# ---------------------------------------------------------------------------

def test_check_gomod_match(tmp_path):
    make_repo(tmp_path)
    assert gvs.check_gomod(tmp_path, "1.26.9") == []


def test_check_gomod_mismatch(tmp_path):
    make_repo(tmp_path, gomod="toolchain go1.26.8")
    problems = gvs.check_gomod(tmp_path, "1.26.9")
    assert len(problems) == 1
    assert "go1.26.8" in problems[0] and "1.26.9" in problems[0]


def test_check_gomod_missing_directive(tmp_path):
    make_repo(tmp_path, gomod="")
    problems = gvs.check_gomod(tmp_path, "1.26.9")
    assert problems and "missing" in problems[0]


# ---------------------------------------------------------------------------
# check_dockerfiles
# ---------------------------------------------------------------------------

def test_check_dockerfile_match(tmp_path):
    make_repo(tmp_path)
    assert gvs.check_dockerfiles(tmp_path, "1.26.9") == []


def test_check_dockerfile_mismatch(tmp_path):
    make_repo(tmp_path, dockerfile="ARG GO_VERSION=1.25.0")
    problems = gvs.check_dockerfiles(tmp_path, "1.26.9")
    assert len(problems) == 1 and "1.25.0" in problems[0]


def test_check_dockerfile_without_go_version_arg_ignored(tmp_path):
    make_repo(tmp_path, dockerfile="FROM node:22")
    assert gvs.check_dockerfiles(tmp_path, "1.26.9") == []


# ---------------------------------------------------------------------------
# check_ci
# ---------------------------------------------------------------------------

def test_check_ci_match(tmp_path):
    make_repo(tmp_path)
    assert gvs.check_ci(tmp_path, "1.26.9") == []


def test_check_ci_mismatch_reports_lineno(tmp_path):
    make_repo(tmp_path, ci="go-version: '1.26.8'")
    problems = gvs.check_ci(tmp_path, "1.26.9")
    assert len(problems) == 1 and "ci.yml:6" in problems[0]


def test_check_ci_missing_file(tmp_path):
    (tmp_path / ".github").mkdir()
    assert gvs.check_ci(tmp_path, "1.26.9") == [".github/workflows/ci.yml not found"]


# ---------------------------------------------------------------------------
# check_sync (whole derivation)
# ---------------------------------------------------------------------------

def test_check_sync_all_match(tmp_path):
    make_repo(tmp_path)
    scanned, problems = gvs.check_sync(tmp_path)
    assert problems == [] and scanned >= 2


def test_check_sync_collects_all_mismatches(tmp_path):
    make_repo(tmp_path, gomod="toolchain go1.26.8",
             dockerfile="ARG GO_VERSION=1.26.7", ci="go-version: '1.26.6'")
    _, problems = gvs.check_sync(tmp_path)
    assert len(problems) == 3


def test_check_sync_unreadable_go_version(tmp_path):
    _, problems = gvs.check_sync(tmp_path)  # no .go-version at all
    assert problems == [".go-version unreadable"]


# ---------------------------------------------------------------------------
# runner drift
# ---------------------------------------------------------------------------

def test_parse_runner_go(tmp_path):
    assert gvs.parse_runner_go("go version go1.26.8 linux/amd64") == "1.26.8"
    assert gvs.parse_runner_go("nonsense") == ""


def test_runner_drift_ok(tmp_path):
    ok, msg = gvs.check_runner_drift("1.26.9", ["1.26.9"], "PATH")
    assert ok and "OK" in msg


def test_runner_drift_ok_when_expected_among_many(tmp_path):
    ok, msg = gvs.check_runner_drift("1.26.9", ["1.25.0", "1.26.8", "1.26.9"], "tc")
    assert ok and "contains go1.26.9" in msg


def test_runner_drift_mismatch_message(tmp_path):
    ok, msg = gvs.check_runner_drift("1.26.9", ["1.26.8"], "toolcache")
    assert not ok
    # The exact loud signal required by the card:
    assert "runner Go cache 1.26.8 != app runtime 1.26.9" in msg
    assert "refresh skquad runner image pre-seed" in msg
    assert "K-45" in msg


def test_runner_drift_no_preseed_found(tmp_path):
    ok, msg = gvs.check_runner_drift("1.26.9", [], "scanned: PATH, /opt/x")
    assert not ok and "could not locate runner Go pre-seed" in msg


def test_discover_preseed_uses_path_go_first(tmp_path, monkeypatch):
    class Out:
        stdout = "go version go1.26.8 linux/amd64"

    monkeypatch.setattr(gvs.subprocess, "run", lambda *a, **k: Out())
    versions, source = gvs.discover_preseed()
    assert versions == ["1.26.8"] and "PATH" in source


def test_discover_preseed_falls_back_to_toolcache(tmp_path, monkeypatch):
    monkeypatch.setattr(
        gvs.subprocess, "run",
        lambda *a, **k: (_ for _ in ()).throw(OSError("no go on PATH")),
    )
    monkeypatch.setattr(gvs, "FALLBACK_TOOLCACHE_ROOTS", ())
    tc = tmp_path / "toolcache" / "go"
    (tc / "1.26.8" / "x64").mkdir(parents=True)
    (tc / "1.26.9" / "x64").mkdir(parents=True)
    monkeypatch.setenv("RUNNER_TOOL_CACHE", str(tmp_path / "toolcache"))
    versions, source = gvs.discover_preseed()
    assert versions == ["1.26.8", "1.26.9"] and "toolcache" in source


def test_discover_preseed_nothing_found(tmp_path, monkeypatch):
    monkeypatch.setattr(
        gvs.subprocess, "run",
        lambda *a, **k: (_ for _ in ()).throw(OSError("no go on PATH")),
    )
    monkeypatch.setattr(gvs, "FALLBACK_TOOLCACHE_ROOTS", ())
    monkeypatch.setenv("RUNNER_TOOL_CACHE", str(tmp_path / "missing"))
    versions, source = gvs.discover_preseed()
    assert versions == [] and "scanned" in source


def test_discover_preseed_scans_fallback_roots(tmp_path, monkeypatch):
    monkeypatch.setattr(
        gvs.subprocess, "run",
        lambda *a, **k: (_ for _ in ()).throw(OSError("no go on PATH")),
    )
    monkeypatch.delenv("RUNNER_TOOL_CACHE", raising=False)
    fb = tmp_path / "opt" / "go"
    (fb / "1.26.8").mkdir(parents=True)
    monkeypatch.setattr(gvs, "FALLBACK_TOOLCACHE_ROOTS", (str(fb),))
    versions, source = gvs.discover_preseed()
    assert versions == ["1.26.8"] and "toolcache" in source


# ---------------------------------------------------------------------------
# CLI wiring
# ---------------------------------------------------------------------------

def test_main_check_passes(tmp_path, capsys):
    make_repo(tmp_path)
    assert gvs.main(["--root", str(tmp_path), "check"]) == 0
    assert "OK: all Go version references match" in capsys.readouterr().out


def test_main_check_fails_on_mismatch(tmp_path, capsys):
    make_repo(tmp_path, gomod="toolchain go1.26.8")
    assert gvs.main(["--root", str(tmp_path), "check"]) == 1
    assert "mismatch" in capsys.readouterr().err


def test_main_runner_drift_missing_go_version(tmp_path):
    assert gvs.main(["--root", str(tmp_path), "runner-drift"]) == 2


def test_main_runner_drift_fires_on_mismatch(tmp_path, monkeypatch, capsys):
    make_repo(tmp_path)
    monkeypatch.setattr(gvs, "discover_preseed", lambda: (["1.26.8"], "toolcache"))
    assert gvs.main(["--root", str(tmp_path), "runner-drift"]) == 1
    err = capsys.readouterr().err
    assert "::error::runner Go cache 1.26.8 != app runtime 1.26.9" in err


def test_main_runner_drift_passes_on_match(tmp_path, monkeypatch, capsys):
    make_repo(tmp_path)
    monkeypatch.setattr(
        gvs, "discover_preseed", lambda: (["1.26.8", "1.26.9"], "toolcache")
    )
    assert gvs.main(["--root", str(tmp_path), "runner-drift"]) == 0
    assert "OK: runner pre-seed contains go1.26.9" in capsys.readouterr().out


def test_main_runner_drift_loud_when_unlocatable(tmp_path, monkeypatch, capsys):
    make_repo(tmp_path)
    monkeypatch.setattr(gvs, "discover_preseed", lambda: ([], "scanned: nothing"))
    assert gvs.main(["--root", str(tmp_path), "runner-drift"]) == 1
    assert "could not locate runner Go pre-seed" in capsys.readouterr().err
