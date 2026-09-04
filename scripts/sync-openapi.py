#!/usr/bin/env python3
"""Copy the canonical public OpenAPI document into the Go embed location."""

from __future__ import annotations

import argparse
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="fail instead of updating the generated copy")
    args = parser.parse_args()

    root = Path(__file__).resolve().parents[1]
    source = root / "api" / "openapi.yaml"
    generated = root / "internal" / "controller" / "openapi.yaml"
    source_bytes = source.read_bytes()
    if generated.exists() and generated.read_bytes() == source_bytes:
        return 0
    if args.check:
        raise SystemExit(f"{generated} is stale; run: python scripts/sync-openapi.py")
    generated.write_bytes(source_bytes)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
