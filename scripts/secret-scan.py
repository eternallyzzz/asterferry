#!/usr/bin/env python3
"""Check tracked repository inputs for credential and local-state material."""

from __future__ import annotations

import re
import subprocess
from pathlib import Path


FORBIDDEN_EXTENSIONS = {".key", ".crt", ".pem", ".token", ".db", ".sqlite", ".sqlite3", ".mmdb"}
PRIVATE_KEY = re.compile(r"BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY")
LONG_TOKEN = re.compile(
    r"(?:api[_-]?token|master[_-]?key|client[_-]?secret)\s*[:=]\s*[\"']?[A-Za-z0-9+/_=-]{32,}[\"']?",
    re.IGNORECASE,
)


def tracked_files(root: Path) -> list[str]:
    result = subprocess.run(
        ["git", "ls-files"], cwd=root, check=True, capture_output=True, text=True
    )
    return [line for line in result.stdout.splitlines() if line]


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    files = tracked_files(root)
    forbidden = []
    matches = []
    for name in files:
        normalized = name.replace("\\", "/")
        suffix = Path(name).suffix.lower()
        if (
            suffix in FORBIDDEN_EXTENSIONS
            or re.search(r"(?i)(\.db|\.sqlite3?)(-|$)", normalized)
            or re.search(r"(^|/)(secrets?|credentials?)(/|$)", normalized, re.IGNORECASE)
        ):
            forbidden.append(name)
            continue
        path = root / name
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise SystemExit(f"cannot read tracked file {name}: {exc}") from exc
        if b"\x00" in data:
            continue
        text = data.decode("utf-8", errors="replace")
        for line_number, line in enumerate(text.splitlines(), 1):
            if PRIVATE_KEY.search(line) or LONG_TOKEN.search(line):
                matches.append(f"{name}:{line_number}:{line}")

    if forbidden:
        raise SystemExit("tracked credential, database or GeoIP material is forbidden: " + ", ".join(forbidden))
    if matches:
        raise SystemExit("high-signal credential material found in tracked files:\n" + "\n".join(matches))
    print(f"Tracked-file secret scan passed ({len(files)} files checked).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
