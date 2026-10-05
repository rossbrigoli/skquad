"""Unit tests for scripts/testreport-to-sonar.py (S-189, test-only)."""

import argparse
import json
import sys
import xml.etree.ElementTree as ET

import pytest

from helpers import load_script

tr = load_script("testreport_to_sonar", "testreport-to-sonar.py")


# ---------------------------------------------------------------------------
# small helpers
# ---------------------------------------------------------------------------

def test_ms_rounds_and_clamps():
    assert tr._ms(1.234) == 1234
    assert tr._ms(0.0004) == 0
    assert tr._ms(-5) == 0
    assert tr._ms(None) == 0
    assert tr._ms("0.5") == 500


def test_go_event_status():
    assert tr._go_event_status("pass") == "ok"
    assert tr._go_event_status("fail") == "failure"
    assert tr._go_event_status("skip") == "skipped"
    with pytest.raises(KeyError):
        tr._go_event_status("run")


# ---------------------------------------------------------------------------
# _write_generic
# ---------------------------------------------------------------------------

def test_write_generic_creates_dirs_and_counts(tmp_path, capsys):
    out = tmp_path / "nested" / "dir" / "report.xml"
    files = {
        "a/x_test.go": [("TestA", "ok", 10), ("TestB", "failure", 20)],
        "b/y_test.go": [("TestC", "skipped", 0), ("TestD", "error", 5)],
    }
    tr._write_generic(files, out)
    assert out.exists()
    root = ET.parse(out).getroot()
    assert root.tag == "testExecutions" and root.get("version") == "1"
    paths = [f.get("path") for f in root.findall("file")]
    assert paths == ["a/x_test.go", "b/y_test.go"]  # sorted
    xfail = root.find("file[@path='a/x_test.go']/testCase[@name='TestB']")
    assert xfail.find("failure") is not None
    derr = root.find("file[@path='b/y_test.go']/testCase[@name='TestD']")
    assert derr.find("error") is not None
    stdout = capsys.readouterr().out
    assert "total=4 passed=1 failed=2 skipped=1" in stdout


def test_write_generic_empty_report(tmp_path, capsys):
    out = tmp_path / "empty.xml"
    tr._write_generic({}, out)
    root = ET.parse(out).getroot()
    assert root.findall("file") == []
    assert "total=0" in capsys.readouterr().out


# ---------------------------------------------------------------------------
# gojson dialect
# ---------------------------------------------------------------------------

def test_go_module_name(tmp_path):
    (tmp_path / "go.mod").write_text("module example.com/skquad/x\n\ngo 1.22\n")
    assert tr._go_module_name(tmp_path) == "example.com/skquad/x"


def test_go_module_name_missing_file_raises(tmp_path):
    # No go.mod at all → plain FileNotFoundError from read_text().
    with pytest.raises(FileNotFoundError):
        tr._go_module_name(tmp_path)


def test_go_module_name_no_directive_exits(tmp_path):
    (tmp_path / "go.mod").write_text("go 1.22\n")
    with pytest.raises(SystemExit):
        tr._go_module_name(tmp_path)


def test_collect_go_events_skips_junk_and_package_level_lines(tmp_path):
    report = tmp_path / "go-test.json"
    lines = [
        "non-JSON preamble line",
        json.dumps({"Action": "run", "Package": "p", "Test": "TestX"}),  # run: ignored
        json.dumps({"Action": "pass", "Package": "p", "Test": "TestX", "Elapsed": 1.5}),
        json.dumps({"Action": "pass", "Package": "p"}),  # no Test: ignored
        json.dumps({"Action": "fail", "Package": "p", "Test": "TestY", "Elapsed": 0}),
        json.dumps({"Action": "skip", "Package": "q", "Test": "TestZ", "Elapsed": 2}),
        json.dumps({"Action": "output", "Package": "p", "Output": "noise"}),
    ]
    report.write_text("\n".join(lines) + "\n", encoding="utf-8")
    events = tr._collect_go_events(str(report))
    assert events == {
        "p": {"TestX": ("ok", 1500), "TestY": ("failure", 0)},
        "q": {"TestZ": ("skipped", 2000)},
    }


def test_go_name_to_file(tmp_path):
    pkg = tmp_path / "internal" / "foo"
    pkg.mkdir(parents=True)
    (pkg / "a_test.go").write_text(
        "package foo\n\nfunc TestAlpha(t *testing.T) {}\n\nfunc TestBeta(t *testing.T) {}\n"
        "// func TestCommented( not a decl at line start? it is commented, but regex matches line\n"
    )
    (pkg / "notatest.go").write_text("func TestNope(t *testing.T) {}\n")
    mapping = tr._go_name_to_file(pkg, "cp/internal/foo")
    assert mapping["TestAlpha"] == "cp/internal/foo/a_test.go"
    assert mapping["TestBeta"] == "cp/internal/foo/a_test.go"
    assert "TestNope" not in mapping


def test_go_name_to_file_missing_dir(tmp_path):
    assert tr._go_name_to_file(tmp_path / "ghost", "cp/ghost") == {}


def make_go_project(tmp_path):
    src = tmp_path / "src"
    pkg = src / "internal" / "foo"
    pkg.mkdir(parents=True)
    (src / "go.mod").write_text("module example.com/x\n")
    (pkg / "a_test.go").write_text(
        "package foo\n\nfunc TestAlpha(t *testing.T) {}\nfunc TestBeta(t *testing.T) {}\n"
    )
    report = tmp_path / "go-test.json"
    report.write_text(
        "\n".join(
            [
                json.dumps({"Action": "pass", "Package": "example.com/x/internal/foo", "Test": "TestAlpha", "Elapsed": 0.25}),
                json.dumps({"Action": "pass", "Package": "example.com/x/internal/foo", "Test": "TestAlpha/sub", "Elapsed": 0.1}),
                json.dumps({"Action": "fail", "Package": "example.com/x/internal/foo", "Test": "TestBeta", "Elapsed": 1}),
                json.dumps({"Action": "pass", "Package": "example.com/x/internal/foo", "Test": "TestGhost", "Elapsed": 0}),
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    return src, report


def test_convert_gojson_end_to_end(tmp_path, capsys):
    src, report = make_go_project(tmp_path)
    out = tmp_path / "out" / "cp.xml"
    args = argparse.Namespace(report=str(report), prefix="control-plane", src_root=str(src), out=str(out))
    tr.convert_gojson(args)
    root = ET.parse(out).getroot()
    files = {f.get("path"): f for f in root.findall("file")}
    assert list(files) == ["control-plane/internal/foo/a_test.go"]
    names = [tc.get("name") for tc in files["control-plane/internal/foo/a_test.go"].findall("testCase")]
    assert names == ["TestAlpha", "TestAlpha/sub", "TestBeta"]  # subtest inherits root file
    beta = files["control-plane/internal/foo/a_test.go"].find("testCase[@name='TestBeta']")
    assert beta.find("failure") is not None
    assert beta.get("duration") == "1000"
    assert "TestGhost" not in "".join(n for n in names)
    err = capsys.readouterr().err
    assert "no test file found for example.com/x/internal/foo::TestGhost" in err


def test_convert_gojson_default_src_root_is_cwd(tmp_path, monkeypatch, capsys):
    src, report = make_go_project(tmp_path)
    monkeypatch.chdir(src)
    out = tmp_path / "cwd.xml"
    args = argparse.Namespace(report=str(report), prefix="cp", src_root=None, out=str(out))
    tr.convert_gojson(args)  # must not raise; finds go.mod in cwd
    assert out.exists()
    root = ET.parse(out).getroot()
    assert root.find("file[@path='cp/internal/foo/a_test.go']") is not None


# ---------------------------------------------------------------------------
# junit dialect
# ---------------------------------------------------------------------------

def test_junit_file_path_passthrough_known_extensions():
    assert tr._junit_file_path("src/lib/status.ts") == "src/lib/status.ts"
    assert tr._junit_file_path("a/b.test.tsx") == "a/b.test.tsx"
    assert tr._junit_file_path("tests/x.py") == "tests/x.py"
    assert tr._junit_file_path("a/b.js") == "a/b.js"
    assert tr._junit_file_path("a/b.jsx") == "a/b.jsx"


def test_junit_file_path_python_dotted_class_stripped():
    assert tr._junit_file_path("tests.test_runtime.TestJournal") == "tests/test_runtime.py"
    assert tr._junit_file_path("tests.TestX") == "tests.py"


def test_junit_file_path_no_uppercase_tail_keeps_all_parts():
    # No capitalised final segment → treat every segment as a module path.
    assert tr._junit_file_path("some.lowercase.thing") == "some/lowercase/thing.py"


def test_junit_status_from_child_elements():
    def tc(xml):
        return ET.fromstring(xml)

    assert tr._junit_status(tc("<testcase/>")) == "ok"
    assert tr._junit_status(tc("<testcase><failure msg='x'/></testcase>")) == "failure"
    assert tr._junit_status(tc("<testcase><error msg='x'/></testcase>")) == "error"
    assert tr._junit_status(tc("<testcase><skipped/></testcase>")) == "skipped"


JUNIT_XML = """<testsuites>
  <testsuite name="SuiteOne" tests="3">
    <testcase classname="tests.test_a.TestA" name="test_pass" time="0.5" file="tests/test_a.py"/>
    <testcase classname="tests.test_b.TestB" name="test_fail" time="1.25">
      <failure message="boom"/>
    </testcase>
    <testcase classname="tests.test_c.TestC" name="test_skip" time="0">
      <skipped/>
    </testcase>
  </testsuite>
  <testsuite name="src/lib/d.test.ts" tests="1">
    <testcase classname="" name="renders" time="2"/>
  </testsuite>
  <testsuite name="" tests="1">
    <testcase name="no-path-anywhere" time="0"/>
  </testsuite>
</testsuites>
"""


def test_convert_junit_end_to_end(tmp_path):
    report = tmp_path / "junit.xml"
    report.write_text(JUNIT_XML, encoding="utf-8")
    out = tmp_path / "gen.xml"
    args = argparse.Namespace(reports=[str(report)], prefix="agent-runtime", out=str(out))
    tr.convert_junit(args)
    root = ET.parse(out).getroot()
    files = {f.get("path"): f for f in root.findall("file")}
    # explicit file= attribute wins over classname
    assert "agent-runtime/tests/test_a.py" in files
    # classname fallback (class segment stripped)
    assert "agent-runtime/tests/test_b.py" in files
    assert files["agent-runtime/tests/test_b.py"].find(
        "testCase[@name='test_fail']/failure"
    ) is not None
    assert "agent-runtime/tests/test_c.py" in files
    # empty classname falls back to suite name (a vitest-style path)
    assert "agent-runtime/src/lib/d.test.ts" in files
    # no path anywhere → testcase dropped
    assert all(
        tc.get("name") != "no-path-anywhere" for f in files.values() for tc in f.findall("testCase")
    )
    dur = files["agent-runtime/tests/test_b.py"].find("testCase[@name='test_fail']").get("duration")
    assert dur == "1250"


def test_convert_junit_multiple_reports_merge(tmp_path):
    r1 = tmp_path / "j1.xml"
    r2 = tmp_path / "j2.xml"
    r1.write_text(
        '<testsuite name="s"><testcase classname="a.TestA" name="t1" time="0"/></testsuite>',
        encoding="utf-8",
    )
    r2.write_text(
        '<testsuite name="s"><testcase classname="a.TestA" name="t2" time="0"/></testsuite>',
        encoding="utf-8",
    )
    out = tmp_path / "merged.xml"
    args = argparse.Namespace(reports=[str(r1), str(r2)], prefix="llm-gateway", out=str(out))
    tr.convert_junit(args)
    root = ET.parse(out).getroot()
    f = root.find("file[@path='llm-gateway/a.py']")
    assert [tc.get("name") for tc in f.findall("testCase")] == ["t1", "t2"]


# ---------------------------------------------------------------------------
# cobertura-prefix
# ---------------------------------------------------------------------------

COBERTURA = """<?xml version="1.0" ?>
<coverage line-rate="0.9">
  <sources>
    <source>/home/ross/build/agent-runtime</source>
    <source>/tmp/other</source>
  </sources>
  <packages>
    <package name="pkg">
      <classes>
        <class name="mod" filename="pkg/mod.py" line-rate="1"/>
        <class name="other" filename="agent-runtime/already_prefixed.py" line-rate="1"/>
      </classes>
    </package>
  </packages>
</coverage>
"""


def test_cobertura_prefix_rewrites_and_neutralises_sources(tmp_path):
    report = tmp_path / "coverage.xml"
    report.write_text(COBERTURA, encoding="utf-8")
    args = argparse.Namespace(report=str(report), prefix="agent-runtime")
    tr.convert_cobertura_prefix(args)
    text = report.read_text(encoding="utf-8")
    assert 'filename="agent-runtime/pkg/mod.py"' in text
    # already-prefixed entries are left untouched (negative lookahead)
    assert 'filename="agent-runtime/already_prefixed.py"' in text
    assert "<sources><source>.</source></sources>" in text
    assert "/home/ross/build" not in text


def test_cobertura_prefix_is_idempotent(tmp_path):
    report = tmp_path / "coverage.xml"
    report.write_text(COBERTURA, encoding="utf-8")
    args = argparse.Namespace(report=str(report), prefix="agent-runtime")
    tr.convert_cobertura_prefix(args)
    once = report.read_text(encoding="utf-8")
    tr.convert_cobertura_prefix(args)
    assert report.read_text(encoding="utf-8") == once


# ---------------------------------------------------------------------------
# CLI wiring
# ---------------------------------------------------------------------------

def test_main_dispatches_junit_subcommand(tmp_path, monkeypatch):
    report = tmp_path / "j.xml"
    report.write_text(
        '<testsuite name="s"><testcase classname="a.TestA" name="t1" time="0"/></testsuite>',
        encoding="utf-8",
    )
    out = tmp_path / "cli.xml"
    monkeypatch.setattr(
        sys,
        "argv",
        ["testreport-to-sonar.py", "junit", str(report), "--prefix", "web", "--out", str(out)],
    )
    tr.main()
    assert out.exists()


def test_main_requires_subcommand(monkeypatch):
    monkeypatch.setattr(sys, "argv", ["testreport-to-sonar.py"])
    with pytest.raises(SystemExit) as exc:
        tr.main()
    assert exc.value.code == 2
