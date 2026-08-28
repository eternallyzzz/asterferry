#!/usr/bin/env bash
set -euo pipefail

if ! command -v go >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
  export PATH="/usr/local/go/bin:$PATH"
fi

root="${ASTERFERRY_WSL_ROOT:-$(git rev-parse --show-toplevel)}"
cd "$root"
bench_time="${ASTERFERRY_BENCHTIME:-30s}"
count="${ASTERFERRY_BENCHCOUNT:-5}"
suite="${ASTERFERRY_BENCH_SUITE:-smoke}"
if [ -n "${ASTERFERRY_BENCHREGEX_B64:-}" ]; then
  bench_regex="$(printf '%s' "$ASTERFERRY_BENCHREGEX_B64" | base64 -d)"
else
  if [ "$suite" = "full" ]; then
    default_regex='Benchmark(.*)'
  else
    default_regex='Benchmark(.*)'
  fi
  bench_regex="${ASTERFERRY_BENCHREGEX:-$default_regex}"
fi
output_dir="$root/tmp/perf/wsl"
mkdir -p "$output_dir"

if command -v go >/dev/null 2>&1; then
  bench_cmd=(go test ./internal/afdp ./internal/dataplane -run '^$' -bench "$bench_regex" -benchmem "-benchtime=$bench_time" "-count=$count")
  go_version="$(go version)"
  goos="$(go env GOOS)"
  goarch="$(go env GOARCH)"
  gomaxprocs="$(go env GOMAXPROCS)"
else
  binary_dir="${ASTERFERRY_BENCH_BINARY_DIR:-$root/tmp/perf/wsl/bin}"
  afdp_bin="$binary_dir/afdp.test"
  dataplane_bin="$binary_dir/dataplane.test"
  for binary in "$afdp_bin" "$dataplane_bin"; do
    if [ ! -x "$binary" ]; then
      echo "Go is unavailable and benchmark binary is missing: $binary" >&2
      exit 1
    fi
  done
  bench_cmd=()
  go_version="prebuilt benchmark binaries"
  goos="linux"
  goarch="$(uname -m)"
  gomaxprocs="$(nproc 2>/dev/null || true)"
fi

printf 'go=%s\ngoos=%s\ngoarch=%s\ngomaxprocs=%s\ncommit=%s\nsuite=%s\nregex=%s\n' \
  "$go_version" "$goos" "$goarch" "$gomaxprocs" "$(git rev-parse HEAD)" "$suite" "$bench_regex" \
  > "$output_dir/metadata.txt"
sysctl net.core.rmem_max net.core.wmem_max net.core.rmem_default net.core.wmem_default 2>/dev/null \
  | tee -a "$output_dir/metadata.txt" || true

if [ ${#bench_cmd[@]} -gt 0 ]; then
  "${bench_cmd[@]}" 2>&1 | tee "$output_dir/bench.txt"
else
  : > "$output_dir/bench.txt"
  for binary in "$afdp_bin" "$dataplane_bin"; do
    "$binary" -test.run '^$' -test.bench "$bench_regex" -test.benchmem \
      "-test.benchtime=$bench_time" "-test.count=$count" 2>&1 | tee -a "$output_dir/bench.txt"
  done
fi

if command -v python3 >/dev/null 2>&1; then
python3 - "$output_dir/bench.txt" "$output_dir/summary.json" "$output_dir/metadata.txt" <<'PY'
import json, re, sys
results = []
pattern = re.compile(r"^(Benchmark\S+)\s+\d+\s+([0-9.]+)\s+ns/op(?:\s+([0-9.]+)\s+MB/s)?")
with open(sys.argv[1], encoding="utf-8", errors="replace") as source:
    for line in source:
        match = pattern.match(line)
        if match:
            results.append({"benchmark": match.group(1), "ns_per_op": float(match.group(2)), "mbps": float(match.group(3)) if match.group(3) else None})
metadata = {}
with open(sys.argv[3], encoding="utf-8", errors="replace") as source:
    for line in source:
        if "=" in line:
            key, value = line.rstrip().split("=", 1)
            metadata[key.strip()] = value.strip()
with open(sys.argv[2], "w", encoding="utf-8") as target:
    json.dump({"metadata": metadata, "results": results}, target, indent=2)
    target.write("\n")
PY
fi
