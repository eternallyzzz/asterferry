#!/usr/bin/env python3
"""Reject CJK characters in canonical source comments.

User-visible Dashboard strings and the localized operations documents are not
comments, so they are intentionally outside this check.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path


COMMENT_MARKERS = {".go": "//", ".ts": "//", ".tsx": "//", ".vue": "//"}
HASH_COMMENT_EXTENSIONS = {".py", ".ps1", ".sh"}
CJK_RE = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")
SOURCE_ROOTS = ("cmd", "internal", "scripts", "web/dashboard/src")
EXCLUDED_PARTS = {"node_modules", "generated", "dist"}


def source_files(root: Path) -> list[Path]:
    files = []
    for relative_root in SOURCE_ROOTS:
        source_root = root / relative_root
        for path in sorted(source_root.rglob("*")):
            if not path.is_file() or path.suffix not in set(COMMENT_MARKERS) | HASH_COMMENT_EXTENSIONS:
                continue
            if EXCLUDED_PARTS.intersection(path.parts):
                continue
            files.append(path)
    return files


def comment_text(path: Path, line: str, in_block: bool) -> tuple[str, bool]:
    suffix = path.suffix.lower()
    if suffix in HASH_COMMENT_EXTENSIONS:
        marker = line.find("#")
        return (line[marker + 1 :] if marker >= 0 else "", in_block)

    marker = COMMENT_MARKERS[suffix]
    comments: list[str] = []
    cursor = 0
    while cursor < len(line):
        if in_block:
            end = line.find("*/", cursor)
            if end < 0:
                comments.append(line[cursor:])
                return "".join(comments), True
            comments.append(line[cursor:end])
            cursor = end + 2
            in_block = False
            continue
        line_marker = line.find(marker, cursor)
        block_marker = line.find("/*", cursor)
        if line_marker < 0 and block_marker < 0:
            break
        if line_marker >= 0 and (block_marker < 0 or line_marker < block_marker):
            comments.append(line[line_marker + len(marker) :])
            break
        comments.append(line[block_marker + 2 :])
        cursor = block_marker + 2
        in_block = True
    return "".join(comments), in_block


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    violations = []
    for path in source_files(root):
        in_block = False
        in_vue_script = path.suffix.lower() != ".vue"
        for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if path.suffix.lower() == ".vue":
                if re.search(r"<script\b", line):
                    in_vue_script = True
                if re.search(r"</script>", line):
                    in_vue_script = False
                    continue
                if not in_vue_script:
                    continue
            comments, in_block = comment_text(path, line, in_block)
            if CJK_RE.search(comments):
                violations.append(f"{path.relative_to(root).as_posix()}:{line_number}")
    if violations:
        print("canonical source comments must use English:", file=sys.stderr)
        print("\n".join(violations), file=sys.stderr)
        return 1
    print("Source language check passed (source comments are English).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
