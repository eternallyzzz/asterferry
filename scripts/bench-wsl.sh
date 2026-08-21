#!/usr/bin/env bash
set -euo pipefail

if ! command -v go >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
  export PATH="/usr/local/go/bin:$PATH"
fi
command -v go >/dev/null 2>&1 || { echo "Go 1.26.5 is required" >&2; exit 1; }

root="${ASTERFERRY_WSL_ROOT:-$(git rev-parse --show-toplevel)}"
cd "$root"
bench_time="${ASTERFERRY_BENCHTIME:-15s}"
count="${ASTERFERRY_BENCHCOUNT:-5}"
output_dir="$root/tmp/perf/wsl"
mkdir -p "$output_dir"

go version
go env GOOS GOARCH GOMAXPROCS
sysctl net.core.rmem_max net.core.wmem_max net.core.rmem_default net.core.wmem_default 2>/dev/null || true
git rev-parse HEAD
go test ./internal/transport ./internal/relay ./internal/integration \
  -run '^$' \
  -bench 'Benchmark(QUICStream|ConnRoundTrip|AsterFerryProxy)' \
  -benchmem \
  -benchtime="$bench_time" \
  -count="$count" \
  | tee "$output_dir/bench.txt"
