#!/usr/bin/env bash
set -euo pipefail

root="${ASTERFERRY_WSL_ROOT:-$(git rev-parse --show-toplevel)}"
output_dir="${ASTERFERRY_COVERAGE_OUTPUT_DIR:-$root/tmp/coverage/wsl}"
expected_go_version="${ASTERFERRY_EXPECTED_GO_VERSION:-go1.26.7}"
mkdir -p "$output_dir"

# The WSL runner invokes this script as a non-login shell. Keep the standard
# system Go installation available even when /etc/profile is not sourced.
if [[ -d /usr/local/go/bin ]]; then
  export PATH="/usr/local/go/bin:$PATH"
fi

log_file="$output_dir/coverage.log"
exec > >(tee "$log_file") 2>&1

fail() {
  echo "coverage-wsl: $*" >&2
  exit 1
}

cd "$root"
command -v go >/dev/null 2>&1 || fail "Go is not installed; install $expected_go_version"
go_version="$(go version)"
case "$go_version" in
  *"$expected_go_version"*) ;;
  *) fail "expected Go $expected_go_version, got: $go_version" ;;
esac

mapfile -t runtime_packages < <(go list -f '{{if .GoFiles}}{{.ImportPath}}{{end}}' ./...)
filtered_packages=()
for package in "${runtime_packages[@]}"; do
  case "$package" in
    */cmd/asterferry-bench) ;;
    "") ;;
    *) filtered_packages+=("$package") ;;
  esac
done
((${#filtered_packages[@]} > 0)) || fail "no runtime packages were found"

coverpkg="$(IFS=,; echo "${filtered_packages[*]}")"
profile_path="$output_dir/coverage.out"
function_path="$output_dir/functions.txt"
html_path="$output_dir/coverage.html"

printf 'timestamp_utc=%s\ncommit=%s\ngo=%s\ngoos=%s\ngoarch=%s\nincluded_packages=%s\nexcluded_packages=cmd/asterferry-bench\n' \
  "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  "$(git rev-parse HEAD)" \
  "$go_version" \
  "$(go env GOOS)" \
  "$(go env GOARCH)" \
  "$coverpkg" > "$output_dir/metadata.txt"

go test -count=1 -covermode=atomic "-coverpkg=$coverpkg" "-coverprofile=$profile_path" ./...
go tool cover "-func=$profile_path" > "$function_path"
grep '^total:' "$function_path" | tail -n 1
go tool cover "-html=$profile_path" "-o=$html_path"

[[ -s "$profile_path" ]] || fail "coverage profile is missing or empty"
[[ -s "$html_path" ]] || fail "coverage HTML report is missing or empty"
echo "WSL coverage reports written under $output_dir"
