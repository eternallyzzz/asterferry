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

# These are the two human-facing documents kept in the repository. They do
# not contain a literal product release version.
README_DOCUMENTS = (
    "README.md",
    "docs/README.zh-CN.md",
)


def read_text(root: Path, relative: str) -> str:
    return (root / relative).read_text(encoding="utf-8")


def repository_markdown_files(root: Path) -> set[str]:
    ignored_parts = {".codebuddy", ".git", "dist", "node_modules", "tmp"}
    return {
        path.relative_to(root).as_posix()
        for path in root.rglob("*.md")
        if not ignored_parts.intersection(path.parts)
    }


def validate_document_set(root: Path) -> None:
    expected = set(README_DOCUMENTS)
    actual = repository_markdown_files(root)
    missing = sorted(expected - actual)
    unexpected = sorted(actual - expected)
    if missing or unexpected:
        details = []
        if missing:
            details.append(f"missing={', '.join(missing)}")
        if unexpected:
            details.append(f"unexpected={', '.join(unexpected)}")
        fail("repository Markdown set is not consolidated: " + "; ".join(details))


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

    validate_document_set(root)

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

    mismatches = {path: value for path, value in sources.items() if value != canonical}
    if mismatches:
        details = ", ".join(f"{path}={value!r}" for path, value in mismatches.items())
        fail(f"release metadata disagrees with VERSION {canonical}: {details}")

    product_version_pattern = re.compile(
        r"(?<![A-Za-z0-9.])v?\d+\.\d+\.\d+(?:-rc\.\d+)?(?![A-Za-z0-9.])"
    )
    for relative in README_DOCUMENTS:
        text = read_text(root, relative)
        if product_version_pattern.search(text):
            fail(f"{relative} contains a hard-coded product release version")

    schema_source = read_text(root, "internal/controller/schema_contract.go")
    schema_match = re.search(
        r"const\s+CurrentDatabaseSchemaVersion\s+uint32\s*=\s*(\d+)", schema_source
    )
    if not schema_match:
        fail("CurrentDatabaseSchemaVersion is not declared in the expected form")
    schema_version = schema_match.group(1)
    document_expectations = {
        "README.md": {
            "AFDP/1": r"\bAFDP/1\b",
            "control/1": r"\bcontrol/1\b",
            "database schema": rf"\bdatabase\s+schema(?:\s+is)?\s+v{schema_version}\b",
        },
        "docs/README.zh-CN.md": {
            "AFDP/1": r"\bAFDP/1\b",
            "control/1": r"\bcontrol/1\b",
            "数据库 schema": rf"数据库\s+schema\s+v{schema_version}\b",
        },
    }
    for relative, expectations in document_expectations.items():
        document = read_text(root, relative)
        for description, pattern in expectations.items():
            if not re.search(pattern, document, re.IGNORECASE):
                fail(f"{relative} is missing required release marker: {description}")

    print(f"Release metadata check passed ({canonical}, database schema v{schema_version}).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
