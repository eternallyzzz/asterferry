#!/usr/bin/env bash
set -euo pipefail

root="${ASTERFERRY_WSL_ROOT:-$(git rev-parse --show-toplevel)}"
output_dir="${ASTERFERRY_TEST_OUTPUT_DIR:-$root/tmp/test/wsl}"
toolchain_go_version=""
if [[ -z "${ASTERFERRY_EXPECTED_GO_VERSION:-}" && -f "$root/.toolchain.json" ]]; then
  if command -v python3 >/dev/null 2>&1; then
    toolchain_go_version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["release"]["go"])' "$root/.toolchain.json")"
  else
    toolchain_go_version="$(sed -nE 's/^[[:space:]]*"go":[[:space:]]*"([^"]+)".*/\1/p' "$root/.toolchain.json" | head -n 1)"
  fi
fi
if [[ -n "${ASTERFERRY_EXPECTED_GO_VERSION:-}" ]]; then
  expected_go_version="$ASTERFERRY_EXPECTED_GO_VERSION"
elif [[ -n "$toolchain_go_version" ]]; then
  expected_go_version="go$toolchain_go_version"
else
  echo "test-wsl: unable to read release Go version from $root/.toolchain.json" >&2
  exit 1
fi
skip_race="${ASTERFERRY_SKIP_RACE:-0}"
fallback_bin_dir="${ASTERFERRY_WSL_TEST_BIN_DIR:-}"
mkdir -p "$output_dir"

# The WSL runner invokes this script as a non-login shell. Keep the standard
# system Go installation available even when /etc/profile is not sourced.
if [[ -d /usr/local/go/bin ]]; then
  export PATH="/usr/local/go/bin:$PATH"
fi

log_file="$output_dir/test.log"
exec > >(tee "$log_file") 2>&1

fail() {
  echo "test-wsl: $*" >&2
  exit 1
}

commit="$(git -C "$root" rev-parse HEAD)"
cd "$root"

if command -v python3 >/dev/null 2>&1; then
  python3 scripts/check-source-layout.py
  python3 scripts/check-toolchain.py
  python3 scripts/secret-scan.py
fi

cleanup_frontend_scratch() {
  [[ -d "$root/tmp" ]] || return 0
  find "$root/tmp" -mindepth 1 -maxdepth 1 -type d \( \
    -name 'release-check-frontend-*' -o \
    -name 'release-check-worktree-*' -o \
    -name 'web-dashboard-check-*' \
  \) -exec rm -rf -- {} +
}

cleanup_frontend_scratch

bash scripts/test-installers.sh

if ! command -v go >/dev/null 2>&1; then
  [[ "$skip_race" == "1" ]] || fail "Go is not installed; install Go $expected_go_version or rerun with -SkipRace for the functional fallback"
  [[ -n "$fallback_bin_dir" && -d "$fallback_bin_dir" ]] || fail "WSL test binaries are missing: $fallback_bin_dir"
  manifest_file="$fallback_bin_dir/manifest.tsv"
  [[ -s "$manifest_file" ]] || fail "WSL test binary manifest is missing: $manifest_file"
  shopt -s nullglob
  test_bins=("$fallback_bin_dir"/*.test)
  ((${#test_bins[@]} > 0)) || fail "no cross-compiled WSL test binaries found in $fallback_bin_dir"
  {
    printf 'timestamp_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'commit=%s\n' "$commit"
    printf 'go=cross-compiled test binaries\n'
    printf 'goos=linux\n'
    printf 'goarch=amd64\n'
    printf 'kernel=%s\n' "$(uname -a)"
    printf 'race=skipped\n'
  } > "$output_dir/metadata.txt"
  while IFS=$'\t' read -r index package relative_directory; do
    index="${index//$'\r'/}"
    package="${package//$'\r'/}"
    relative_directory="${relative_directory%$'\r'}"
    [[ -z "$index$package$relative_directory" ]] && continue
    [[ "$index" =~ ^[0-9]{3}$ && -n "$package" && -n "$relative_directory" ]] || fail "invalid WSL test binary manifest row"
    test_bin="$fallback_bin_dir/$index.test"
    [[ -x "$test_bin" ]] || fail "WSL test binary is missing: $test_bin"
    echo "running $package in $relative_directory"
    (cd "$root/$relative_directory" && "$test_bin" -test.count=1)
  done < "$manifest_file"
  if [[ "${ASTERFERRY_SKIP_DOCKER:-0}" != "1" ]]; then
    if command -v docker >/dev/null 2>&1; then
      if docker info >/dev/null 2>&1; then
        ASTERFERRY_DOCKER_ROOT="$root" ASTERFERRY_DOCKER_TEST_OUTPUT_DIR="$output_dir/docker" bash scripts/test-docker.sh
      elif [[ "${ASTERFERRY_REQUIRE_DOCKER:-0}" == "1" ]]; then
        fail "Docker CLI is present but Docker Engine is unavailable"
      else
        echo "Docker Engine is unavailable; Docker verification skipped"
      fi
    elif [[ "${ASTERFERRY_REQUIRE_DOCKER:-0}" == "1" ]]; then
      fail "Docker CLI is required for the requested Docker verification"
    else
      echo "Docker CLI is unavailable; Docker verification skipped"
    fi
  fi
  echo "WSL functional verification passed using cross-compiled test binaries"
  exit 0
fi

go mod tidy -diff

go_version="$(go version)"
case "$go_version" in
  *"$expected_go_version"*) ;;
  *) fail "expected Go $expected_go_version, got: $go_version" ;;
esac

if [[ "$skip_race" != "1" ]]; then
  command -v gcc >/dev/null 2>&1 || fail "gcc is required for the Go race detector"
  [[ "$(go env CGO_ENABLED)" == "1" ]] || fail "CGO_ENABLED must be 1 for the WSL race test"
fi

{
  printf 'timestamp_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'commit=%s\n' "$commit"
  printf 'go=%s\n' "$go_version"
  printf 'goos=%s\n' "$(go env GOOS)"
  printf 'goarch=%s\n' "$(go env GOARCH)"
  printf 'gomaxprocs=%s\n' "$(go env GOMAXPROCS)"
  printf 'kernel=%s\n' "$(uname -a)"
  printf 'race=%s\n' "$(if [[ "$skip_race" == "1" ]]; then echo skipped; else echo enabled; fi)"
} > "$output_dir/metadata.txt"

mapfile -d '' go_files < <(find . -type f -name '*.go' \
  -not -path './.git/*' \
  -not -path './tmp/*' \
  -print0)
if ((${#go_files[@]} == 0)); then
  fail "no Go files found"
fi

unformatted="$(gofmt -l "${go_files[@]}")"
if [[ -n "$unformatted" ]]; then
  printf 'Unformatted Go files:\n%s\n' "$unformatted" >&2
  exit 1
fi

go vet ./cmd/... ./internal/...
if command -v staticcheck >/dev/null 2>&1; then
  staticcheck -checks=all,-SA1019 ./cmd/... ./internal/...
fi
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./cmd/... ./internal/...
fi
go test -count=1 ./cmd/... ./internal/...
go test -count=1 ./internal/afdp ./internal/controller ./internal/dataplane ./internal/duplex ./internal/node -run 'Contract|StateMachine'
go test -tags=integration -count=1 -timeout=5m ./internal/integration
go test ./internal/afdp -run '^$' -fuzz FuzzDecodeAFDPFrames -fuzztime 10s
go test ./internal/controlwire -run '^$' -fuzz FuzzControlwireDecoders -fuzztime 10s
if [[ "$skip_race" != "1" ]]; then
  go test -race -count=1 ./cmd/... ./internal/...
fi

CGO_ENABLED=0 go build -trimpath -o "$output_dir/asterferry-linux" ./cmd/asterferry
CGO_ENABLED=0 go build -trimpath -o "$output_dir/asterferry-bench-linux" ./cmd/asterferry-bench

if [[ "${ASTERFERRY_SKIP_DOCKER:-0}" != "1" ]]; then
  if command -v docker >/dev/null 2>&1; then
    if docker info >/dev/null 2>&1; then
      ASTERFERRY_DOCKER_ROOT="$root" ASTERFERRY_DOCKER_TEST_OUTPUT_DIR="$output_dir/docker" bash scripts/test-docker.sh
    elif [[ "${ASTERFERRY_REQUIRE_DOCKER:-0}" == "1" ]]; then
      fail "Docker CLI is present but Docker Engine is unavailable"
    else
      echo "Docker Engine is unavailable; Docker verification skipped"
    fi
  elif [[ "${ASTERFERRY_REQUIRE_DOCKER:-0}" == "1" ]]; then
    fail "Docker CLI is required for the requested Docker verification"
  else
    echo "Docker CLI is unavailable; Docker verification skipped"
  fi
fi

if [[ "$skip_race" == "1" ]]; then
  echo "WSL functional verification passed (race test skipped)"
else
  echo "WSL full verification passed"
fi
cleanup_frontend_scratch
