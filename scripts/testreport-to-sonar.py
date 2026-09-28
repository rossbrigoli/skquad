#!/usr/bin/env python3
"""Convert unit-test reports into SonarQube's generic test execution format.

SonarQube ingests test execution metadata (counts, failures, durations) from a
"generic test execution report" (root element <testExecutions version="1">).
See the official format reference:
  https://docs.sonarsource.com/sonarqube-server/analyzing-source-code/test-coverage/generic-test-data

Two input dialects are supported so every stack in this monorepo can publish
test results through one uniform pipeline:

  gojson  -- `go test -json` output (NDJSON). Test cases are attributed to the
             exact *_test.go file that declares them by scanning the package
             directory for top-level `func TestXxx(` declarations (Go enforces
             unique test names per package, so the mapping is unambiguous).
             Subtests ("TestX/sub") inherit the root test's file.

  junit   -- JUnit XML (e.g. vitest's junit reporter, unittest-xml-reporting).
             The test file path is taken from each testcase's classname
             (dotted Python module paths are converted to <path>.py).

Usage:
  testreport-to-sonar.py gojson --prefix control-plane \
      --out test-results/control-plane.xml go-test.json
  testreport-to-sonar.py junit --prefix web \
      --out test-results/web.xml web/coverage/junit.xml

The emitted <file> paths are relative to the repository root (the scanner's
base directory), prefixed with --prefix. Output is idempotent per invocation.
"""

import argparse
import json
import re
import sys
import xml.etree.ElementTree as ET
from pathlib import Path

GO_TEST_FUNC_RE = re.compile(r"^func (Test\w+)\(", re.MULTILINE)


def _ms(seconds: float) -> int:
    return max(0, int(round(float(seconds or 0) * 1000)))


def _write_generic(files: dict, out_path: Path) -> None:
    root = ET.Element("testExecutions", {"version": "1"})
    total = passed = failed = skipped = 0
    for file_path in sorted(files):
        file_el = ET.SubElement(root, "file", {"path": file_path})
        for name, status, duration in files[file_path]:
            attrs = {"name": name, "duration": str(duration)}
            tc = ET.SubElement(file_el, "testCase", attrs)
            if status == "failure":
                ET.SubElement(tc, "failure")
                failed += 1
            elif status == "error":
                ET.SubElement(tc, "error")
                failed += 1
            elif status == "skipped":
                ET.SubElement(tc, "skipped")
                skipped += 1
            else:
                passed += 1
            total += 1
    out_path.parent.mkdir(parents=True, exist_ok=True)
    ET.ElementTree(root).write(out_path, encoding="unicode", xml_declaration=False)
    print(
        f"[testreport-to-sonar] {out_path}: total={total} passed={passed} "
        f"failed={failed} skipped={skipped}"
    )


def _go_module_name(src_root: Path) -> str:
    for line in (src_root / "go.mod").read_text().splitlines():
        line = line.strip()
        if line.startswith("module "):
            return line.split(None, 1)[1].strip()
    raise SystemExit(f"go.mod not found or has no module directive under {src_root}")


def convert_gojson(args: argparse.Namespace) -> None:
    src_root = Path(args.src_root or ".")
    module = _go_module_name(src_root)

    # package -> {test name -> (status, duration_ms)}
    events: dict = {}
    with open(args.report, "r", encoding="utf-8") as fh:
        for raw in fh:
            raw = raw.strip()
            if not raw:
                continue
            try:
                ev = json.loads(raw)
            except json.JSONDecodeError:
                continue  # non-JSON preamble lines
            action = ev.get("Action")
            pkg = ev.get("Package")
            if action not in ("pass", "fail", "skip") or pkg is None:
                continue
            name = ev.get("Test")
            if name is None:
                continue  # package-level result without a named test
            events.setdefault(pkg, {})[name] = (
                {"pass": "ok", "fail": "failure", "skip": "skipped"}[action],
                _ms(ev.get("Elapsed", 0)),
            )

    files: dict = {}
    for pkg, tests in events.items():
        rel = pkg[len(module):].lstrip("/") if pkg.startswith(module) else pkg
        pkg_dir = src_root / rel
        name_to_file: dict = {}
        if pkg_dir.is_dir():
            for tf in sorted(pkg_dir.glob("*_test.go")):
                for m in GO_TEST_FUNC_RE.finditer(tf.read_text(encoding="utf-8")):
                    name_to_file[m.group(1)] = f"{args.prefix}/{rel}/{tf.name}".lstrip("/")
        for tname, (status, dur) in tests.items():
            root_name = tname.split("/", 1)[0]
            fpath = name_to_file.get(root_name)
            if fpath is None:
                # Unknown attribution: skip rather than emit a path SonarQube
                # cannot resolve (unresolvable files are dropped with warnings).
                print(
                    f"[testreport-to-sonar] warn: no test file found for "
                    f"{pkg}::{tname}; skipping",
                    file=sys.stderr,
                )
                continue
            files.setdefault(fpath, []).append((tname, status, dur))
    _write_generic(files, Path(args.out))


def _junit_file_path(raw: str) -> str:
    """Map a JUnit classname/testsuite name to a repo-relative test file path.

    vitest junit: already a path like 'src/lib/status.ts'.
    unittest-xml-reporting: dotted 'tests.test_runtime.TestJournal' ->
    strip the trailing class segment (capitalised) and render as a .py path.
    """
    if raw.endswith((".ts", ".tsx", ".js", ".jsx", ".py")):
        return raw
    parts = raw.split(".")
    if parts and parts[-1][:1].isupper():
        parts = parts[:-1]  # drop the test class segment
    return "/".join(parts) + ".py"


def convert_junit(args: argparse.Namespace) -> None:
    files: dict = {}
    for report in args.reports:
        tree = ET.parse(report)
        root = tree.getroot()
        for suite in root.iter("testsuite"):
            suite_file = suite.get("name") or ""
            for tc in suite.findall("testcase"):
                # Prefer an explicit file attribute (unittest-xml-reporting
                # emits file="tests/test_x.py"); fall back to classname.
                raw_path = tc.get("file") or tc.get("classname") or suite_file
                if not raw_path:
                    continue
                fpath = f"{args.prefix}/{_junit_file_path(raw_path)}".lstrip("/")
                status = "ok"
                if tc.find("failure") is not None:
                    status = "failure"
                elif tc.find("error") is not None:
                    status = "error"
                elif tc.find("skipped") is not None:
                    status = "skipped"
                files.setdefault(fpath, []).append(
                    (tc.get("name", "?"), status, _ms(tc.get("time", 0)))
                )
    _write_generic(files, Path(args.out))


def convert_cobertura_prefix(args: argparse.Namespace) -> None:
    """Rewrite Cobertura filename="..." attributes to be repo-root-relative.

    coverage.py emits filenames relative to the component dir with an absolute
    <sources> entry pointing at the *build* machine. The scanner runs on a
    different host (self-hosted runner), so those absolute paths never resolve
    ("Cannot resolve the file path ... does not exist in all 'source'").
    Prefixing filenames with the component dir and neutralising <sources> to
    '.' makes the report portable across machines.
    """
    text = Path(args.report).read_text(encoding="utf-8")
    text = re.sub(
        r'filename="(?!' + re.escape(args.prefix) + r'/)',
        'filename="' + args.prefix + '/',
        text,
    )
    text = re.sub(
        r"<sources>.*?</sources>",
        "<sources><source>.</source></sources>",
        text,
        flags=re.S,
    )
    Path(args.report).write_text(text, encoding="utf-8")
    print(f"[testreport-to-sonar] {args.report}: filenames prefixed with {args.prefix}/")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="kind", required=True)

    p_go = sub.add_parser("gojson", help="convert `go test -json` output")
    p_go.add_argument("report")
    p_go.add_argument("--prefix", required=True, help="repo-relative component dir")
    p_go.add_argument("--src-root", help="component source root (default: cwd)")
    p_go.add_argument("--out", required=True)
    p_go.set_defaults(func=convert_gojson)

    p_junit = sub.add_parser("junit", help="convert JUnit XML (one or more files)")
    p_junit.add_argument("reports", nargs="+")
    p_junit.add_argument("--prefix", required=True, help="repo-relative component dir")
    p_junit.add_argument("--out", required=True)
    p_junit.set_defaults(func=convert_junit)

    p_cov = sub.add_parser(
        "cobertura-prefix",
        help="make coverage.py Cobertura portable: prefix filename attrs with the component dir",
    )
    p_cov.add_argument("report")
    p_cov.add_argument("--prefix", required=True, help="repo-relative component dir")
    p_cov.set_defaults(func=convert_cobertura_prefix)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
