#!/usr/bin/env python3
"""Validate the repository's release and database-schema metadata."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


STABLE_VERSION_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")
RELEASE_VERSION_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)(?:-rc\.\d+)?$")
TAG_RE = re.compile(r"^v(\d+\.\d+\.\d+(?:-rc\.\d+)?)$")

# These files describe the current release policy. They intentionally do not
# contain a literal product version; the changelog and generated metadata are
# checked separately below.
POLICY_DOCUMENTS = (
    "README.md",
    "SECURITY.md",
    ".github/CODEOWNERS",
    "docs/architecture.md",
    "docs/architecture-internals.md",
    "docs/compatibility.md",
    "docs/geoip.md",
    "docs/operations.en.md",
    "docs/operations.zh-CN.md",
    "docs/release-runbook.md",
    "docs/support-matrix.md",
)


def read_text(root: Path, relative: str) -> str:
    return (root / relative).read_text(encoding="utf-8")


def stable_part(version: str) -> str:
    match = RELEASE_VERSION_RE.fullmatch(version)
    if not match:
        raise ValueError(f"invalid release version {version!r}")
    return ".".join(match.group(index) for index in range(1, 4))


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tag", help="release tag to validate, for example v<VERSION>-rc.N")
    parser.add_argument("--version", help="release version to validate without a leading v")
    return parser.parse_args()


def fail(message: str) -> None:
    raise SystemExit(f"release metadata check failed: {message}")


def main() -> int:
    args = parse_args()
    root = Path(__file__).resolve().parents[1]
    canonical = read_text(root, "VERSION").strip()
    if not STABLE_VERSION_RE.fullmatch(canonical):
        fail("VERSION must contain one stable MAJOR.MINOR.PATCH value without a leading v")

    requested = args.version.strip().lstrip("v") if args.version else canonical
    if not RELEASE_VERSION_RE.fullmatch(requested):
        fail(f"invalid requested release version {requested!r}")
    if stable_part(requested) != canonical:
        fail(f"requested release {requested} does not use canonical VERSION {canonical}")

    if args.tag:
        tag_match = TAG_RE.fullmatch(args.tag.strip())
        if not tag_match:
            fail("release tags must match vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-rc.N")
        if stable_part(tag_match.group(1)) != canonical:
            fail(f"release tag {args.tag} does not use canonical VERSION {canonical}")

    sources: dict[str, str] = {}
    for relative in ("api/openapi.yaml", "internal/controller/openapi.yaml"):
        match = re.search(r"(?m)^  version:\s*(\S+)\s*$", read_text(root, relative))
        if not match:
            fail(f"{relative} does not declare info.version")
        sources[relative] = match.group(1)

    package = json.loads(read_text(root, "web/dashboard/package.json"))
    package_lock = json.loads(read_text(root, "web/dashboard/package-lock.json"))
    sources["web/dashboard/package.json"] = str(package.get("version", ""))
    sources["web/dashboard/package-lock.json"] = str(
        package_lock.get("packages", {}).get("", {}).get("version", "")
    )

    for chart in ("deploy/helm/asterferry-controller", "deploy/helm/asterferry-node"):
        chart_text = read_text(root, f"{chart}/Chart.yaml")
        chart_match = re.search(r"(?m)^version:\s*([^\s]+)", chart_text)
        app_match = re.search(r"(?m)^appVersion:\s*[\"']?([^\"'\s]+)", chart_text)
        if not chart_match or not app_match:
            fail(f"{chart}/Chart.yaml is missing version or appVersion")
        sources[f"{chart}/Chart.yaml version"] = chart_match.group(1)
        sources[f"{chart}/Chart.yaml appVersion"] = app_match.group(1)

    changelog = read_text(root, "CHANGELOG.md")
    if not re.search(
        rf"(?m)^## \[{re.escape(canonical)}\] - (?:Unreleased|\d{{4}}-\d{{2}}-\d{{2}})\s*$",
        changelog,
    ):
        fail(f"CHANGELOG.md has no current entry for {canonical}")

    mismatches = {path: value for path, value in sources.items() if value != canonical}
    if mismatches:
        details = ", ".join(f"{path}={value!r}" for path, value in mismatches.items())
        fail(f"release metadata disagrees with VERSION {canonical}: {details}")

    product_version_pattern = re.compile(
        r"(?<![A-Za-z0-9.])v?\d+\.\d+\.\d+(?:-rc\.\d+)?(?![A-Za-z0-9.])"
    )
    for relative in POLICY_DOCUMENTS:
        text = read_text(root, relative)
        if product_version_pattern.search(text):
            fail(f"{relative} contains a hard-coded product release version")

    compatibility = read_text(root, "docs/compatibility.md")
    if "current release line" not in compatibility:
        fail("docs/compatibility.md does not refer to the current release line")

    schema_source = read_text(root, "internal/controller/schema_contract.go")
    schema_match = re.search(
        r"const\s+CurrentDatabaseSchemaVersion\s+uint32\s*=\s*(\d+)", schema_source
    )
    if not schema_match:
        fail("CurrentDatabaseSchemaVersion is not declared in the expected form")
    schema_version = schema_match.group(1)
    schema_expectations = {
        "CHANGELOG.md": rf"database schema v{schema_version}\b",
        "docs/architecture.md": rf"database schema v{schema_version}\b",
        "docs/architecture-internals.md": rf"\bv{schema_version} relational tables\b",
        "docs/compatibility.md": rf"Database schema v{schema_version}\b",
        "docs/operations.en.md": rf"\bpre-v{schema_version} databases\b",
        "docs/operations.zh-CN.md": rf"v{schema_version} 之前的数据库",
    }
    for relative, pattern in schema_expectations.items():
        if not re.search(pattern, read_text(root, relative), re.IGNORECASE):
            fail(f"{relative} does not describe the current database schema v{schema_version}")

    print(f"Release metadata check passed ({canonical}, database schema v{schema_version}).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
