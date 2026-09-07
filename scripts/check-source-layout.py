#!/usr/bin/env python3
"""Enforce readable boundaries for handwritten production Go sources."""

from __future__ import annotations

import fnmatch
import json
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


def check_controller_boundaries(root: Path) -> list[str]:
    controller_root = root / "internal" / "controller"
    manifest_path = controller_root / "boundaries.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    areas = manifest.get("areas", {})
    sources = [
        path
        for path in sorted(controller_root.rglob("*.go"))
        if not path.name.endswith("_test.go")
        and not path.name.endswith(".pb.go")
        and path.name not in GENERATED_NAMES
    ]
    violations = []
    for path in sources:
        relative = path.relative_to(controller_root).as_posix()
        owners = [
            area
            for area, patterns in areas.items()
            if any(fnmatch.fnmatch(relative, pattern) for pattern in patterns)
        ]
        if len(owners) != 1:
            owner_text = ", ".join(owners) if owners else "none"
            violations.append(f"{relative}: expected exactly one logical owner, got {owner_text}")

    declared = {
        pattern
        for patterns in areas.values()
        for pattern in patterns
    }
    for pattern in sorted(declared):
        if not any(fnmatch.fnmatch(path.relative_to(controller_root).as_posix(), pattern) for path in sources):
            violations.append(f"boundary pattern matches no production source: {pattern}")
    return violations


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    violations = check_controller_boundaries(root)
    checked = 0
    for path in production_sources(root):
        checked += 1
        lines = len(path.read_text(encoding="utf-8").splitlines())
        if lines > MAX_LINES:
            violations.append(f"{lines}\t{path.relative_to(root).as_posix()}")
    if violations:
        print("source layout violations:", file=sys.stderr)
        print("\n".join(violations), file=sys.stderr)
        return 1
    print(f"Source layout check passed ({checked} handwritten production Go files; Controller ownership map is complete).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
