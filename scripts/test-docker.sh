#!/usr/bin/env bash
set -Eeuo pipefail

root="${ASTERFERRY_DOCKER_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
output_dir="${ASTERFERRY_DOCKER_TEST_OUTPUT_DIR:-$root/tmp/test/docker}"
mkdir -p "$output_dir"

fail() {
  echo "test-docker: $*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "Docker CLI is required"
docker info >/dev/null 2>&1 || fail "Docker Engine is unavailable"

go_version="${ASTERFERRY_EXPECTED_GO_VERSION:-}"
if [[ -z "$go_version" && -f "$root/.toolchain.json" ]]; then
  if command -v python3 >/dev/null 2>&1; then
    go_version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["release"]["go"])' "$root/.toolchain.json")"
  else
    go_version="$(sed -nE 's/^[[:space:]]*"go":[[:space:]]*"([^"]+)".*/\1/p' "$root/.toolchain.json" | head -n 1)"
  fi
fi
[[ "$go_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "could not determine the pinned Go version"

log_file="$output_dir/test.log"
exec > >(tee "$log_file") 2>&1

short_commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo local)"
image="asterferry:docker-e2e-${short_commit}-$$"
test_image="${image}-test"
cleanup() {
  docker image rm "$image" >/dev/null 2>&1 || true
  docker image rm "$test_image" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cd "$root"
echo "== Docker production image build =="
docker build --network host --platform linux/amd64 \
  --build-arg "VERSION=${ASTERFERRY_DOCKER_VERSION:-dev}" \
  --build-arg "COMMIT=${short_commit}" \
  --build-arg "BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --tag "$image" .

echo "== Docker production image version =="
version_output="$(docker run --rm "$image" version --short)"
[[ -n "$version_output" ]] || fail "production image returned an empty version"

echo "== Docker production image initialization =="
docker run --rm --read-only --tmpfs /tmp:rw,noexec,nosuid,size=64m "$image" \
  controller init \
  --dir /tmp/controller \
  --http-listen 127.0.0.1:8443 \
  --grpc-listen 127.0.0.1:9443 \
  --grpc-advertise 127.0.0.1:9443 \
  --password docker-e2e-password >/dev/null

echo "== Docker Compose configuration =="
docker compose -f deploy/docker/compose.yaml config --quiet

echo "== Docker Linux Controller/Gateway/Agent/data-plane integration =="
docker build --network host --platform linux/amd64 --target build \
  --build-arg "VERSION=${ASTERFERRY_DOCKER_VERSION:-dev}" \
  --build-arg "COMMIT=${short_commit}" \
  --build-arg "BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --tag "$test_image" .
docker run --rm "$test_image" \
  sh -lc 'set -eu; export PATH=/usr/local/go/bin:/usr/local/bin:$PATH; go version; go test -tags=integration -count=1 -timeout=5m ./internal/integration'

echo "Docker Linux verification passed"
