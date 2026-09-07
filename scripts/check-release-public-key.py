#!/usr/bin/env python3
"""Validate the embedded release verification key and protected fingerprint."""

from __future__ import annotations

import argparse
import base64
import hashlib
import os
import re
import sys
from pathlib import Path


FINGERPRINT_ENV = "ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256"
PLACEHOLDER_FINGERPRINT = "d45bc1981a2225280679a45f7266d60c074c113db69c0545b1f8554c25e67fc3"
EC_PUBLIC_KEY_OID = bytes.fromhex("2a8648ce3d0201")
P256_OID = bytes.fromhex("2a8648ce3d030107")
ROOT = Path(__file__).resolve().parents[1]
DEFAULT_KEY = ROOT / "internal/update/release-public-key.pem"


class KeyCheckError(Exception):
    """A user-facing release-key validation failure."""


def read_pem(path: Path) -> bytes:
    try:
        lines = [line.strip() for line in path.read_text(encoding="ascii").splitlines() if line.strip()]
    except (OSError, UnicodeError) as exc:
        raise KeyCheckError(f"release verification key cannot be read: {path}: {exc}") from exc
    if len(lines) < 3 or lines[0] != "-----BEGIN PUBLIC KEY-----" or lines[-1] != "-----END PUBLIC KEY-----":
        raise KeyCheckError("release verification key must be a PEM PUBLIC KEY")
    encoded = "".join(lines[1:-1])
    try:
        return base64.b64decode(encoded, validate=True)
    except ValueError as exc:
        raise KeyCheckError("release verification key contains invalid PEM data") from exc


def read_der_value(data: bytes, offset: int, expected_tag: int) -> tuple[bytes, int]:
    if offset >= len(data) or data[offset] != expected_tag:
        raise KeyCheckError("release verification key contains an invalid SubjectPublicKeyInfo")
    offset += 1
    if offset >= len(data):
        raise KeyCheckError("release verification key contains a truncated DER length")
    length_byte = data[offset]
    offset += 1
    if length_byte & 0x80:
        length_size = length_byte & 0x7F
        if length_size == 0 or length_size > 4 or offset + length_size > len(data):
            raise KeyCheckError("release verification key contains an invalid DER length")
        if data[offset] == 0:
            raise KeyCheckError("release verification key contains a non-minimal DER length")
        length = int.from_bytes(data[offset : offset + length_size], "big")
        offset += length_size
        if length < 128:
            raise KeyCheckError("release verification key contains a non-minimal DER length")
    else:
        length = length_byte
    end = offset + length
    if end > len(data):
        raise KeyCheckError("release verification key contains truncated DER data")
    return data[offset:end], end


def validate_spki(der: bytes) -> None:
    outer, offset = read_der_value(der, 0, 0x30)
    if offset != len(der):
        raise KeyCheckError("release verification key contains trailing DER data")

    algorithm, offset = read_der_value(outer, 0, 0x30)
    algorithm_oid, algorithm_offset = read_der_value(algorithm, 0, 0x06)
    parameters_oid, algorithm_offset = read_der_value(algorithm, algorithm_offset, 0x06)
    if algorithm_offset != len(algorithm) or algorithm_oid != EC_PUBLIC_KEY_OID or parameters_oid != P256_OID:
        raise KeyCheckError("release verification key must be an ECDSA P-256 public key")

    bit_string, offset = read_der_value(outer, offset, 0x03)
    if offset != len(outer) or len(bit_string) != 66 or bit_string[0] != 0 or bit_string[1] != 0x04:
        raise KeyCheckError("release verification key must be an ECDSA P-256 public key")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--key", type=Path, default=DEFAULT_KEY, help="embedded PEM public-key path")
    parser.add_argument("--expected-fingerprint", help="expected SPKI SHA-256 fingerprint")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    environment_expected = os.environ.get(FINGERPRINT_ENV, "").strip().lower()
    argument_expected = (args.expected_fingerprint or "").strip().lower()
    if environment_expected and argument_expected and environment_expected != argument_expected:
        raise KeyCheckError(f"{FINGERPRINT_ENV} disagrees with --expected-fingerprint")
    expected = environment_expected or argument_expected
    if not re.fullmatch(r"[0-9a-f]{64}", expected):
        raise KeyCheckError(f"{FINGERPRINT_ENV} must be a 64-character hexadecimal SPKI fingerprint")
    der = read_pem(args.key)
    validate_spki(der)
    actual = hashlib.sha256(der).hexdigest()
    if actual == PLACEHOLDER_FINGERPRINT:
        raise KeyCheckError("release verification key is still the development placeholder")
    if actual != expected:
        raise KeyCheckError("release verification key does not match the protected production fingerprint")
    print("release verification key fingerprint matches the protected production key")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyCheckError as exc:
        print(f"release verification key check failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
