#!/usr/bin/env python3
"""Copy the canonical embedded OpenAPI document to the public API location."""

from __future__ import annotations

import argparse
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="fail instead of updating the generated copy")
    args = parser.parse_args()

    root = Path(__file__).resolve().parents[1]
    source = root / "internal" / "controller" / "openapi.yaml"
    generated = root / "api" / "openapi.yaml"
    generated_header = b"# Code generated from internal/controller/openapi.yaml; DO NOT EDIT.\n"
    source_bytes = source.read_bytes()
    generated_bytes = generated_header + source_bytes
    if generated.exists() and generated.read_bytes() == generated_bytes:
        return 0
    if args.check:
        raise SystemExit(f"{generated} is stale; run: python scripts/sync-openapi.py")
    generated.write_bytes(generated_bytes)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
