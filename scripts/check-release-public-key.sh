#!/usr/bin/env bash
set -euo pipefail

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(CDPATH= cd -- "$script_dir/.." && pwd)"
python_command=""
for candidate in python3 python py.exe python.exe; do
  if version="$($candidate --version 2>&1)" && [[ "$version" == Python* ]]; then
    python_command="$candidate"
    break
  fi
done
if [[ -z "$python_command" ]]; then
  echo "required command not found: python3, python, py.exe or python.exe" >&2
  exit 1
fi
cd "$repo_root"
python_args=(scripts/check-release-public-key.py)
if [[ -n "${ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256:-}" ]]; then
  python_args+=(--expected-fingerprint "$ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256")
fi
python_args+=("$@")
exec "$python_command" "${python_args[@]}"
