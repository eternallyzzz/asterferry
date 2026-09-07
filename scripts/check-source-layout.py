#!/usr/bin/env python3
"""Enforce readable boundaries for handwritten production Go sources."""

from __future__ import annotations

import sys
from pathlib import Path


MAX_LINES = 600
GENERATED_NAMES = {"assets_generated.go"}


def production_sources(root: Path) -> list[Path]:
    result = []
    for source_root in (root / "cmd", root / "internal"):
        for path in sorted(source_root.rglob("*.go")):
            if path.name.endswith("_test.go") or path.name.endswith(".pb.go"):
                continue
            if path.name in GENERATED_NAMES:
                continue
            result.append(path)
    return result


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    violations = []
    checked = 0
    for path in production_sources(root):
        checked += 1
        lines = len(path.read_text(encoding="utf-8").splitlines())
        if lines > MAX_LINES:
            violations.append(f"{lines}\t{path.relative_to(root).as_posix()}")
    if violations:
        print("handwritten production Go files exceed the 600-line limit:", file=sys.stderr)
        print("\n".join(violations), file=sys.stderr)
        return 1
    print(f"Source layout check passed ({checked} handwritten production Go files).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
