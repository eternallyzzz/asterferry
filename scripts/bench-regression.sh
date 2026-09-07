#!/usr/bin/env bash
set -euo pipefail

# Compare protocol benchmarks from the pull request with its base commit on
# the same runner. An absolute machine-specific baseline would be misleading;
# a default regression above 10% is a release blocker.
base_branch="${1:-${GITHUB_BASE_REF:-}}"
if [[ -z "$base_branch" ]]; then
  echo "usage: $0 <base-branch> (or set GITHUB_BASE_REF)" >&2
  exit 2
fi
base_branch="${base_branch#refs/heads/}"
threshold="${ASTERFERRY_BENCH_REGRESSION_THRESHOLD:-0.10}"
bench_time="${ASTERFERRY_BENCHTIME:-2s}"
bench_count="${ASTERFERRY_BENCHCOUNT:-5}"
go_bin="${GO_BIN:-go}"

git fetch --no-tags origin "$base_branch" --depth=1
base_commit="$(git rev-parse "origin/$base_branch")"
temp_root="$(mktemp -d)"
base_root="$temp_root/base"
base_output="$temp_root/base.txt"
head_output="$temp_root/head.txt"
cleanup() {
  git worktree remove --force "$base_root" >/dev/null 2>&1 || true
  rm -rf -- "$temp_root"
}
trap cleanup EXIT
git worktree add --detach "$base_root" "$base_commit" >/dev/null

run_benchmarks() {
  local root="$1"
  local output="$2"
  (
    cd "$root"
    "$go_bin" test ./internal/afdp ./internal/controlwire ./internal/dataplane \
      -run '^$' -bench '^Benchmark' -benchmem \
      "-benchtime=$bench_time" "-count=$bench_count"
  ) >"$output"
}

echo "Benchmarking base $base_commit"
run_benchmarks "$base_root" "$base_output"
echo "Benchmarking head $(git rev-parse HEAD)"
run_benchmarks "$(git rev-parse --show-toplevel)" "$head_output"

python3 - "$base_output" "$head_output" "$threshold" <<'PY'
import re
import statistics
import sys

base_path, head_path, threshold_text = sys.argv[1:]
threshold = float(threshold_text)
pattern = re.compile(r"^(Benchmark\S+)\s+\d+\s+([0-9.]+)\s+ns/op")


def read(path):
    values = {}
    with open(path, encoding="utf-8", errors="replace") as source:
        for line in source:
            match = pattern.match(line)
            if match:
                name = re.sub(r"-\d+$", "", match.group(1))
                values.setdefault(name, []).append(float(match.group(2)))
    return {name: statistics.median(samples) for name, samples in values.items()}


base = read(base_path)
head = read(head_path)
common = sorted(set(base) & set(head))
if not common:
    print("No common benchmarks in base and head; baseline activates after this change.")
    raise SystemExit(0)

failures = []
for name in common:
    change = head[name] / base[name] - 1.0
    print(f"{name}: base={base[name]:.2f} ns/op head={head[name]:.2f} ns/op change={change:+.2%}")
    if change > threshold:
        failures.append((name, change))
if failures:
    details = ", ".join(f"{name} {change:+.2%}" for name, change in failures)
    raise SystemExit(f"benchmark regression exceeds {threshold:.0%}: {details}")
PY
