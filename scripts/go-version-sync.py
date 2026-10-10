#!/usr/bin/env python3
"""Single source of truth for the Go runtime version (S-266).

Repo-root ``.go-version`` is authoritative. Two guards live here:

``check``  Derivation check (hosted, cheap): every go.mod ``toolchain``
           directive, every Dockerfile ``ARG GO_VERSION=`` default and every
           ci.yml ``go-version:`` must equal ``.go-version``. Fails with a
           per-file mismatch list so a version bump can be made consistent in
           one pass.

``runner-drift``  Runner pre-seed drift guard (self-hosted skquad runner,
           run BEFORE any setup-go override). K-37 pre-seeded Go 1.26.8 into
           the runner image; the app runtime later drifted to 1.26.9
           (GO-2026-66xx) and nothing noticed until the SCA job went red.
           This makes that drift loud and early:

               runner Go cache X != app runtime Y — refresh skquad runner
               image pre-seed  (K-45 owns the refresh runbook)

Exit codes:
  0  everything agrees / no drift
  1  mismatch or drift detected (loud, actionable)
  2  tool error: .go-version missing/unparseable, runner Go unreadable
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path

VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")

# Refresh target for the runner pre-seed (Kanbunny k3s-cluster board).
REFRESH_CARD = "K-45"


def read_expected(root: Path) -> str:
    """Return the version from ``.go-version``, or '' if missing/invalid."""
    path = root / ".go-version"
    if not path.exists():
        print("missing repo-root .go-version (single source of truth)", file=sys.stderr)
        return ""
    raw = path.read_text(encoding="utf-8").strip()
    value = raw[2:] if raw.startswith("go") else raw
    if not VERSION_RE.match(value):
        print(f".go-version is not X.Y.Z: {raw!r}", file=sys.stderr)
        return ""
    return value


def check_gomod(root: Path, expected: str) -> list[str]:
    """Every top-level go.mod must carry ``toolchain go<expected>``."""
    problems: list[str] = []
    for gomod in sorted(root.glob("*/go.mod")):
        text = gomod.read_text(encoding="utf-8")
        match = re.search(r"^toolchain\s+go(\S+)\s*$", text, re.MULTILINE)
        rel = gomod.relative_to(root)
        if match is None:
            problems.append(f"{rel}: missing 'toolchain go{expected}' directive")
        elif match.group(1) != expected:
            problems.append(
                f"{rel}: toolchain go{match.group(1)} != .go-version {expected}"
            )
    return problems


def check_dockerfiles(root: Path, expected: str) -> list[str]:
    """Every Dockerfile ``ARG GO_VERSION=`` default must equal ``.go-version``."""
    problems: list[str] = []
    for dockerfile in sorted(root.glob("*/Dockerfile*")):
        text = dockerfile.read_text(encoding="utf-8")
        match = re.search(r"^ARG\s+GO_VERSION=(\S+)\s*$", text, re.MULTILINE)
        if match is not None and match.group(1) != expected:
            problems.append(
                f"{dockerfile.relative_to(root)}: "
                f"ARG GO_VERSION={match.group(1)} != .go-version {expected}"
            )
    return problems


def check_ci(root: Path, expected: str) -> list[str]:
    """Every setup-go ``go-version:`` in ci.yml must equal ``.go-version``."""
    problems: list[str] = []
    ci = root / ".github" / "workflows" / "ci.yml"
    if not ci.exists():
        return [".github/workflows/ci.yml not found"]
    for lineno, line in enumerate(
        ci.read_text(encoding="utf-8").splitlines(), start=1
    ):
        match = re.search(r"""go-version:\s*['"]([^'"]+)['"]""", line)
        if match is not None and match.group(1) != expected:
            problems.append(
                f".github/workflows/ci.yml:{lineno}: "
                f"go-version '{match.group(1)}' != .go-version {expected}"
            )
    return problems


def check_sync(root: Path) -> tuple[int, list[str]]:
    """Run every derivation check. Returns (checked_count, problems)."""
    expected = read_expected(root)
    if not expected:
        return 0, [".go-version unreadable"]
    checks = [
        check_gomod(root, expected),
        check_dockerfiles(root, expected),
        check_ci(root, expected),
    ]
    problems = [p for group in checks for p in group]
    scanned = len(list(root.glob("*/go.mod"))) + len(list(root.glob("*/Dockerfile*")))
    print(f".go-version = {expected} (go.mod + Dockerfiles scanned: {scanned})")
    return scanned, problems


def parse_runner_go(output: str) -> str:
    """Extract the version from ``go version go1.26.8 linux/amd64``."""
    match = re.search(r"\bgo(\d+\.\d+\.\d+)\b", output)
    return match.group(1) if match else ""


def check_runner_drift(expected: str, runner_output: str) -> tuple[bool, str]:
    """Compare the runner's pre-seeded Go against ``.go-version``."""
    actual = parse_runner_go(runner_output)
    if not actual:
        return False, f"could not parse runner Go version from: {runner_output!r}"
    if actual != expected:
        return False, (
            f"runner Go cache {actual} != app runtime {expected} — "
            f"refresh skquad runner image pre-seed (Kanbunny {REFRESH_CARD})"
        )
    return True, f"OK: runner pre-seed go{actual} == .go-version {expected}"


def discover_preseed() -> tuple[list[str], str]:
    """Locate the runner image's pre-seeded Go versions.

    Preference order:
      1. ``go`` on PATH (``go version``) — if the image exposes it.
      2. The Actions hosted toolcache manifests: ``$RUNNER_TOOL_CACHE/go/<ver>/x64``
         plus the common fixed locations. The skquad runner image (K-37) has NO
         ``go`` on PATH; its pre-seed lives in the toolcache that setup-go
         consults, so the manifest directories are the ground truth.

    Returns (sorted-unique versions, human-readable source description).
    """
    try:
        out = subprocess.run(
            ["go", "version"], capture_output=True, text=True, check=True
        ).stdout
        version = parse_runner_go(out)
        if version:
            return [version], "`go version` on PATH"
    except (OSError, subprocess.CalledProcessError):
        pass

    roots: list[Path] = []
    tool_cache = os.environ.get("RUNNER_TOOL_CACHE")
    if tool_cache:
        roots.append(Path(tool_cache) / "go")
    roots += [
        Path("/opt/hostedtoolcache/go"),
        Path.home() / "hostedtoolcache" / "go",
        Path.home() / "_work" / "_tool" / "go",
    ]
    found: list[str] = []
    for root_dir in roots:
        if not root_dir.is_dir():
            continue
        for entry in sorted(root_dir.iterdir()):
            if entry.is_dir() and VERSION_RE.match(entry.name) and entry.name not in found:
                found.append(entry.name)
    if found:
        return found, "hosted toolcache manifest (no `go` on PATH)"
    return [], "scanned: PATH, " + ", ".join(str(r) for r in roots)


def check_runner_drift(
    expected: str, versions: list[str], source: str
) -> tuple[bool, str]:
    """Compare the runner's pre-seeded Go versions against ``.go-version``.

    No drift means the toolcache already contains the app runtime, so setup-go
    resolves it locally instead of downloading.
    """
    if not versions:
        return False, (
            f"could not locate runner Go pre-seed ({source}) — "
            f"cannot verify drift against app runtime {expected}"
        )
    if expected in versions:
        return True, (
            f"OK: runner pre-seed contains go{expected} "
            f"(pre-seeded: {', '.join(versions)}; source: {source})"
        )
    return False, (
        f"runner Go cache {', '.join(versions)} != app runtime {expected} — "
        f"refresh skquad runner image pre-seed (Kanbunny {REFRESH_CARD})"
    )


def runner_drift(root: Path) -> int:
    expected = read_expected(root)
    if not expected:
        return 2
    # Deliberately BEFORE any setup-go override: the runner image's own
    # pre-seed (PATH go or hosted toolcache) is what we guard.
    versions, source = discover_preseed()
    ok, message = check_runner_drift(expected, versions, source)
    print(f"runner Go source: {source}")
    if ok:
        print(message)
        return 0
    print(f"::error::{message}", file=sys.stderr)
    return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--root", type=Path, default=Path.cwd(), help="repo root (default: cwd)"
    )
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("check", help="derivation: go.mod/Dockerfile/ci.yml == .go-version")
    sub.add_parser("runner-drift", help="runner pre-seeded Go == .go-version")
    args = parser.parse_args(argv)

    if args.cmd == "check":
        _, problems = check_sync(args.root)
        if problems:
            print(
                f"FAIL: {len(problems)} Go version mismatch(es) vs .go-version:",
                file=sys.stderr,
            )
            for p in problems:
                print(f"  - {p}", file=sys.stderr)
            return 1
        print("OK: all Go version references match .go-version")
        return 0
    if args.cmd == "runner-drift":
        return runner_drift(args.root)
    return 2


if __name__ == "__main__":
    sys.exit(main())
