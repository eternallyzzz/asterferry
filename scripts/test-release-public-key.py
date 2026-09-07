#!/usr/bin/env python3
"""Exercise the release-key checker with deterministic public fixtures."""

from __future__ import annotations

import base64
import os
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CHECKER = ROOT / "scripts/check-release-public-key.py"
FINGERPRINT_ENV = "ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256"
PLACEHOLDER_FINGERPRINT = "d45bc1981a2225280679a45f7266d60c074c113db69c0545b1f8554c25e67fc3"
VALID_FINGERPRINT = "9872445b8c94d7db3d43614b3f9276d5119c94b1d1813ffc8fe6e73f43f210fd"
VALID_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAED7dMsC2ApU9RGZbPJnVf9iMXCYMkkCHTVGE+V9tQfzJwJsHYoeL+RggeMA0l5KCQClm4Dzu3pW6pm6r1y/iViQ==
-----END PUBLIC KEY-----
"""


def run_checker(key: Path, expected: str) -> subprocess.CompletedProcess[str]:
    environment = os.environ.copy()
    environment[FINGERPRINT_ENV] = expected
    return subprocess.run(
        [sys.executable, str(CHECKER), "--key", str(key)],
        cwd=ROOT,
        env=environment,
        capture_output=True,
        text=True,
        check=False,
    )


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="asterferry-release-key-") as directory:
        root = Path(directory)
        valid = root / "valid.pem"
        valid.write_text(VALID_PUBLIC_KEY, encoding="ascii")
        accepted = run_checker(valid, VALID_FINGERPRINT)
        if accepted.returncode != 0:
            raise SystemExit(f"valid P-256 key was rejected: {accepted.stderr.strip()}")

        mismatch = run_checker(valid, "0" * 64)
        if mismatch.returncode == 0 or "does not match" not in mismatch.stderr:
            raise SystemExit("fingerprint mismatch was not rejected")

        malformed = root / "malformed.pem"
        malformed.write_text("-----BEGIN PUBLIC KEY-----\ninvalid\n-----END PUBLIC KEY-----\n", encoding="ascii")
        invalid = run_checker(malformed, VALID_FINGERPRINT)
        if invalid.returncode == 0:
            raise SystemExit("malformed public key was accepted")

        non_p256_der = base64.b64decode(VALID_PUBLIC_KEY.splitlines()[1])
        non_p256_der = non_p256_der.replace(bytes.fromhex("2a8648ce3d030107"), bytes.fromhex("2a8648ce3d030108"), 1)
        non_p256 = root / "non-p256.pem"
        non_p256.write_text(
            "-----BEGIN PUBLIC KEY-----\n"
            + base64.b64encode(non_p256_der).decode("ascii")
            + "\n-----END PUBLIC KEY-----\n",
            encoding="ascii",
        )
        unsupported = run_checker(non_p256, VALID_FINGERPRINT)
        if unsupported.returncode == 0 or "ECDSA P-256" not in unsupported.stderr:
            raise SystemExit("non-P-256 public key was accepted")

        placeholder = run_checker(ROOT / "internal/update/release-public-key.pem", PLACEHOLDER_FINGERPRINT)
        if placeholder.returncode == 0 or "development placeholder" not in placeholder.stderr:
            raise SystemExit("development placeholder key was accepted")

    print("Release public-key checker tests passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
