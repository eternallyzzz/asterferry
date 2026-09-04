#!/usr/bin/env python3
"""Check that release toolchain pins do not drift across build inputs."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    toolchain = json.loads((root / ".toolchain.json").read_text(encoding="utf-8"))
    release = toolchain["release"]
    go = release["go"]
    node = release["node"]
    npm = release["npm"]
    postgres = release["postgres"]
    compatibility = toolchain["compatibility"]
    compatibility_node = compatibility["node"]
    compatibility_npm = compatibility["npm"]

    checks = {
        "go.mod": (root / "go.mod").read_text(encoding="utf-8"),
        "package.json": (root / "web/dashboard/package.json").read_text(encoding="utf-8"),
        "Dockerfile": (root / "Dockerfile").read_text(encoding="utf-8"),
        "workflow": (root / ".github/workflows/container.yml").read_text(encoding="utf-8"),
    }
    expected = {
        "go.mod": [f"go {go}"],
        "package.json": [
            f'"node": ">={compatibility_node.split(".")[0]} <{int(node.split(".")[0]) + 1}"',
            f'"npm": ">={int(compatibility_npm.split(".")[0])} <{int(npm.split(".")[0]) + 1}"',
        ],
        "Dockerfile": [
            f"node:{node.split('.')[0]}-alpine@sha256:",
            f"golang:{go}-alpine@sha256:",
            f"npm@{npm}",
        ],
    }
    missing = []
    for name, needles in expected.items():
        for needle in needles:
            if needle not in checks[name]:
                missing.append(f"{name}: {needle}")
    workflow_values = {
        "go-version": (
            re.findall(r"(?m)^\s*go-version:\s*[\"']?([^\"'\s]+)", checks["workflow"]),
            {go},
        ),
        "node-version": (
            re.findall(r"(?m)^\s*node-version:\s*[\"']?([^\"'\s]+)", checks["workflow"]),
            {node, compatibility_node},
        ),
        "npm": (re.findall(r"npm@([0-9]+\.[0-9]+\.[0-9]+)", checks["workflow"]), {npm, compatibility_npm}),
        "postgres": (re.findall(r"postgres:([0-9]+)-alpine", checks["workflow"]), {postgres}),
    }
    for name, (values, allowed) in workflow_values.items():
        if not values:
            missing.append(f"workflow: no {name} entries found")
        elif set(values) != allowed:
            missing.append(f"workflow: {name} values {sorted(set(values))} != {sorted(allowed)}")
    if missing:
        print("toolchain drift detected:", file=sys.stderr)
        print("\n".join(missing), file=sys.stderr)
        return 1
    print(
        f"Toolchain pin check passed (Go {go}, Node {node}, npm {npm}; "
        f"compatibility Node {compatibility_node}, npm {compatibility_npm})."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
