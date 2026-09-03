#!/usr/bin/env python3
"""Fail a CI job when a Go coverage profile is below a floor.

`go test ./... -coverpkg=./...` writes one profile per test binary, so the same
code block appears once per binary with different hit counts. This merges those
duplicate blocks (a block counts as covered if any binary hit it) and reports the
same total that `go tool cover -func` prints.

Usage:
    go-coverage-gate.py <coverprofile> --min <percent>
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path


def merge_profile(path: Path) -> tuple[int, int]:
    statements: dict[str, int] = {}
    hit: dict[str, bool] = {}

    with path.open() as handle:
        for line in handle:
            line = line.strip()
            if not line or line.startswith("mode:"):
                continue
            # "<file>:<start>.<col>,<end>.<col> <numStmt> <count>"
            block, _, rest = line.rpartition(" ")
            prefix, _, num_stmt = block.rpartition(" ")
            try:
                count = int(rest)
                statements_in_block = int(num_stmt)
            except ValueError:
                print(f"unexpected coverage line: {line}", file=sys.stderr)
                return -1, -1
            statements.setdefault(prefix, statements_in_block)
            hit[prefix] = hit.get(prefix, False) or count > 0

    total = sum(statements.values())
    covered = sum(count for block, count in statements.items() if hit[block])
    return covered, total


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("profile", type=Path)
    parser.add_argument("--min", type=float, required=True, help="minimum coverage percentage")
    args = parser.parse_args()

    if not args.profile.exists():
        print(f"coverage profile not found: {args.profile}", file=sys.stderr)
        return 2

    covered, total = merge_profile(args.profile)
    if total < 0:
        return 2
    if total == 0:
        print("coverage profile contained no statements", file=sys.stderr)
        return 2

    percent = covered / total * 100
    print(f"coverage: {covered}/{total} statements = {percent:.2f}% (floor {args.min:.2f}%)")
    if percent < args.min:
        print(
            f"FAIL: coverage {percent:.2f}% is below the agreed floor of {args.min:.2f}%",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
