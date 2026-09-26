#!/usr/bin/env python3
"""Compute the Skquad release version (S-141).

SemVer shape is ``major.minor.build``. ``major``/``minor`` are authored by hand in
``version.json``; ``build`` is derived from the release pipeline's run number so it
auto-increments on every release without anyone editing a file (and without CI
committing back to git, which would create push-trigger loops):

    build = buildBase + (run_number - runBase)

``runBase``/``buildBase`` are the baseline pair recorded in ``version.json``:
``runBase`` is the last pipeline run number that already produced a release, and
``buildBase`` is the build number of that release. The first release after the
baseline (run runBase+1) therefore gets buildBase+1.

The script fails loudly when ``run_number <= runBase``: that means an old pipeline
run is being replayed after the baseline moved, and publishing it would move the
build number *backwards*. Monotonicity of the build number is the property the
About page and support triage rely on.

Output (stdout, ``key=value`` lines) is meant for ``$GITHUB_OUTPUT``.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

MAX_COMPONENT = 2_147_483_647  # sane upper bound; semver allows arbitrary, this catches typos


class VersionError(Exception):
    """Raised for a bad baseline file or an out-of-range run number."""


def load_baseline(path: Path) -> tuple[int, int, int, int]:
    """Return (major, minor, build_base, run_base) from a version.json file."""
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise VersionError(f"version file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise VersionError(f"{path} is not valid JSON: {exc}") from exc

    missing = [key for key in ("major", "minor", "buildBase", "runBase") if key not in raw]
    if missing:
        raise VersionError(f"{path} missing required key(s): {', '.join(missing)}")

    out: list[int] = []
    for key in ("major", "minor", "buildBase", "runBase"):
        value = raw[key]
        if isinstance(value, bool) or not isinstance(value, int):
            raise VersionError(f"{path}: {key} must be an integer, got {value!r}")
        if value < 0 or value > MAX_COMPONENT:
            raise VersionError(f"{path}: {key} out of range 0..{MAX_COMPONENT}: {value}")
        out.append(value)
    return out[0], out[1], out[2], out[3]


def compute_build(build_base: int, run_base: int, run_number: int) -> int:
    """Derive the build number, refusing a non-monotonic replay."""
    if run_number <= run_base:
        raise VersionError(
            f"run_number {run_number} <= runBase {run_base}: replaying an old run would move "
            "the build number backwards. Update runBase/buildBase in version.json first."
        )
    build = build_base + (run_number - run_base)
    if build > MAX_COMPONENT:
        raise VersionError(f"computed build {build} exceeds {MAX_COMPONENT}")
    return build


def normalize_commit(commit: str) -> tuple[str, str]:
    """Return (full, short) commit; short is 7 chars, 'unknown' passes through."""
    cleaned = (commit or "").strip()
    if not cleaned:
        return "unknown", "unknown"
    if re.fullmatch(r"[0-9a-fA-F]{7,40}", cleaned):
        cleaned = cleaned.lower()
        return cleaned, cleaned[:7]
    raise VersionError(f"commit must be a hex SHA (7-40 chars), got {commit!r}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--file", default="version.json", help="baseline version file (default: version.json)")
    parser.add_argument("--run-number", required=True, type=int, help="release pipeline run number")
    parser.add_argument("--commit", default="", help="full commit SHA being released")
    args = parser.parse_args(argv)

    try:
        major, minor, build_base, run_base = load_baseline(Path(args.file))
        build = compute_build(build_base, run_base, args.run_number)
        full_commit, short_commit = normalize_commit(args.commit)
    except VersionError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    version = f"{major}.{minor}.{build}"
    print(f"major={major}")
    print(f"minor={minor}")
    print(f"build={build}")
    print(f"version={version}")
    print(f"version_tag=v{version}")
    print(f"commit={full_commit}")
    print(f"commit_short={short_commit}")
    print(f"baseline={major}.{minor}.{build_base}@run{run_base}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
