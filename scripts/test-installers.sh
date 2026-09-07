#!/usr/bin/env bash
set -Eeuo pipefail

root="${ASTERFERRY_WSL_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
output_dir="${ASTERFERRY_INSTALLER_TEST_OUTPUT_DIR:-$root/tmp/test/installers-wsl}"
mkdir -p "$output_dir"

fail() {
  echo "test-installers: $*" >&2
  exit 1
}

for script in scripts/install-controller.sh scripts/install-node.sh; do
  bash -n "$root/$script" || fail "shell installer syntax check failed: $script"
done

gate_status=0
old_fingerprint="${ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256-}"
export ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256=d45bc1981a2225280679a45f7266d60c074c113db69c0545b1f8554c25e67fc3
gate_output="$(bash "$root/scripts/check-release-public-key.sh" 2>&1)" || gate_status=$?
if [[ "$gate_status" -eq 0 ]]; then
  fail "the development release verification key was accepted"
fi
[[ "$gate_output" == *"development placeholder"* ]] || fail "the release-key gate did not execute its key validation"
if [[ -n "$old_fingerprint" ]]; then
  export ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256="$old_fingerprint"
else
  unset ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256
fi

key_test_python=""
for candidate in python3 python py.exe python.exe; do
  if version="$($candidate --version 2>&1)" && [[ "$version" == Python* ]]; then
    key_test_python="$candidate"
    break
  fi
done
[[ -n "$key_test_python" ]] || fail "required command not found: python3, python, py.exe or python.exe"
(cd "$root" && "$key_test_python" scripts/test-release-public-key.py) || fail "release public-key checker tests failed"

node_help="$(bash "$root/scripts/install-node.sh" --help 2>&1)" || fail "Linux Node installer help failed"
[[ "$node_help" == *"Usage: install-node.sh"* ]] || fail "Linux Node installer help contract failed"
controller_help="$(bash "$root/scripts/install-controller.sh" --help 2>&1)" || fail "Linux Controller installer help failed"
[[ "$controller_help" == *"Usage: install-controller.sh"* ]] || fail "Linux Controller installer help contract failed"

grep -q -- '--disable' "$root/scripts/install-controller.sh" || fail "Controller installer must disable .curlrc"
grep -q -- '--disable' "$root/scripts/install-node.sh" || fail "Node installer must disable .curlrc"
grep -q -- '--service-mode' "$root/scripts/install-node.sh" || fail "Node installer must report service mode"
grep -q -- 'detect_advertise_ip' "$root/scripts/install-controller.sh" || fail "Controller installer must auto-detect a default gRPC advertise address"
grep -q -- '--force' "$root/scripts/install-controller.sh" || fail "Controller installer must force-initialize a non-empty data directory"
grep -q -- 'Controller gRPC advertise address (required)' "$root/scripts/install-controller.sh" || fail "Controller installer must prompt for the required gRPC advertise address"
grep -q -- 'Release version \[empty for latest stable\]' "$root/scripts/install-controller.sh" || fail "Controller installer must show default values in brackets"

if bash "$root/scripts/install-controller.sh" --grpc-advertise invalid >/dev/null 2>&1 </dev/null; then
  fail "Controller installer must reject an invalid grpc advertise address"
fi
if bash "$root/scripts/install-node.sh" --node-id n --controller invalid --bootstrap-url https://controller.example --token t --ca-pem-b64 e >/dev/null 2>&1 </dev/null; then
  fail "Node installer must reject an invalid controller address"
fi

{
  printf 'timestamp_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'platform=wsl-linux\n'
  printf 'shell_syntax=passed\n'
  printf 'runtime_install=not-run-by-default; use an isolated WSL distribution\n'
} > "$output_dir/report.txt"
echo "WSL installer syntax and contract verification passed"
