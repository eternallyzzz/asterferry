#!/usr/bin/env bash
set -euo pipefail

key_path="${1:-internal/update/release-public-key.pem}"
expected="${ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256:-}"
placeholder="d45bc1981a2225280679a45f7266d60c074c113db69c0545b1f8554c25e67fc3"

if [[ ! -s "$key_path" ]]; then
  echo "release verification key is missing or empty: $key_path" >&2
  exit 1
fi
if [[ ! "$expected" =~ ^[0-9a-fA-F]{64}$ ]]; then
  echo "ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256 must be a 64-character hexadecimal SPKI fingerprint" >&2
  exit 1
fi

key_details="$(openssl pkey -pubin -in "$key_path" -text -noout)"
if [[ "$key_details" != *"ASN1 OID: prime256v1"* ]]; then
  echo "release verification key must be an ECDSA P-256 public key" >&2
  exit 1
fi
actual="$(openssl pkey -pubin -in "$key_path" -outform DER | openssl dgst -sha256 -r | awk '{print tolower($1)}')"
if [[ "$actual" == "$placeholder" ]]; then
  echo "release verification key is still the development placeholder" >&2
  exit 1
fi
if [[ "$actual" != "${expected,,}" ]]; then
  echo "release verification key does not match the protected production fingerprint" >&2
  exit 1
fi

echo "release verification key fingerprint matches the protected production key"
