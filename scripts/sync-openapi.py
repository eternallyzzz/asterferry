#!/usr/bin/env python3
"""Copy the canonical embedded OpenAPI document to the public API location."""

from __future__ import annotations

import argparse
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="check the generated copy without updating it")
    args = parser.parse_args()

    root = Path(__file__).resolve().parents[1]
    source = root / "internal" / "controller" / "openapi.yaml"
    generated = root / "api" / "openapi.yaml"
    source_text = source.read_text(encoding="utf-8").replace("\r\n", "\n").replace("\r", "\n")
    generated_header = "# Code generated from internal/controller/openapi.yaml; DO NOT EDIT.\n"
    generated_bytes = (generated_header + source_text).encode("utf-8")
    if generated.exists() and generated.read_bytes() == generated_bytes:
        return 0
    if args.check:
        raise SystemExit(f"{generated} is stale; run: python scripts/sync-openapi.py")
    generated.write_bytes(generated_bytes)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
