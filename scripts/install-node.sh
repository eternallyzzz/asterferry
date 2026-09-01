#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-node.sh --role agent|gateway --node-id ID --controller HOST:PORT
  --token TOKEN --ca-pem-b64 BASE64 --release-base-url URL --version X.Y.Z --arch amd64|arm64
USAGE
  exit 2
}

ROLE=""
NODE_ID=""
CONTROLLER=""
TOKEN=""
CA_PEM_B64=""
RELEASE_BASE_URL=""
VERSION=""
EXPECTED_ARCH=""

while (($# > 0)); do
  case "$1" in
    --role) ROLE="${2:?missing value for --role}"; shift 2 ;;
    --node-id) NODE_ID="${2:?missing value for --node-id}"; shift 2 ;;
    --controller) CONTROLLER="${2:?missing value for --controller}"; shift 2 ;;
    --token) TOKEN="${2:?missing value for --token}"; shift 2 ;;
    --ca-pem-b64) CA_PEM_B64="${2:?missing value for --ca-pem-b64}"; shift 2 ;;
    --release-base-url) RELEASE_BASE_URL="${2:?missing value for --release-base-url}"; shift 2 ;;
    --version) VERSION="${2:?missing value for --version}"; shift 2 ;;
    --arch) EXPECTED_ARCH="${2:?missing value for --arch}"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

[[ "$ROLE" == "agent" || "$ROLE" == "gateway" ]] || { echo "role must be agent or gateway" >&2; exit 2; }
[[ "$EXPECTED_ARCH" == "amd64" || "$EXPECTED_ARCH" == "arm64" ]] || { echo "arch must be amd64 or arm64" >&2; exit 2; }
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must be X.Y.Z" >&2; exit 2; }
[[ -n "$NODE_ID" && -n "$CONTROLLER" && -n "$TOKEN" && -n "$CA_PEM_B64" && -n "$RELEASE_BASE_URL" ]] || usage
[[ "$(id -u)" -eq 0 ]] || { echo "run this installer as root (the generated command uses sudo)" >&2; exit 1; }

command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 1; }
command -v tar >/dev/null || { echo "tar is required" >&2; exit 1; }
command -v base64 >/dev/null || { echo "base64 is required" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd is required; enable systemd in WSL before running this command" >&2; exit 1; }

machine_arch="$(uname -m)"
case "$machine_arch" in
  x86_64|amd64) actual_arch="amd64" ;;
  aarch64|arm64) actual_arch="arm64" ;;
  *) echo "unsupported Linux architecture: $machine_arch" >&2; exit 1 ;;
esac
[[ "$actual_arch" == "$EXPECTED_ARCH" ]] || { echo "selected architecture $EXPECTED_ARCH does not match this host ($actual_arch)" >&2; exit 1; }

if ! id asterferry >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/asterferry --shell /usr/sbin/nologin asterferry
fi
install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
install -d -m 0755 /usr/local/bin

tmp_dir="$(mktemp -d -t asterferry-node.XXXXXX)"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT

archive="asterferry_${VERSION}_linux_${EXPECTED_ARCH}.tar.gz"
base="${RELEASE_BASE_URL%/}/v${VERSION}"
curl --fail --silent --show-error --location "$base/$archive" --output "$tmp_dir/$archive"
curl --fail --silent --show-error --location "$base/SHA256SUMS" --output "$tmp_dir/SHA256SUMS"
expected="$(awk -v name="$archive" '$2 == name { print $1; exit }' "$tmp_dir/SHA256SUMS")"
[[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || { echo "release checksum for $archive was not found" >&2; exit 1; }
printf '%s  %s\n' "$expected" "$tmp_dir/$archive" | sha256sum --check --status - || { echo "release checksum verification failed" >&2; exit 1; }
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
[[ -x "$tmp_dir/asterferry" ]] || { echo "release archive does not contain asterferry" >&2; exit 1; }
install -m 0755 "$tmp_dir/asterferry" /usr/local/bin/asterferry

ca_path=/var/lib/asterferry/controller-ca.crt
printf '%s' "$CA_PEM_B64" | base64 --decode > "$tmp_dir/controller-ca.crt"
install -o asterferry -g asterferry -m 0644 "$tmp_dir/controller-ca.crt" "$ca_path"
bootstrap_path="/var/lib/asterferry/${ROLE}-bootstrap.json"
cache_path=/var/lib/asterferry/snapshot.cache

if [[ ! -f "$bootstrap_path" ]]; then
  runuser -u asterferry -- /usr/local/bin/asterferry "$ROLE" enroll \
    --controller "$CONTROLLER" \
    --token "$TOKEN" \
    --node-id "$NODE_ID" \
    --ca "$ca_path" \
    --output "$bootstrap_path" \
    --cache "$cache_path"
else
  echo "existing $bootstrap_path found; enrollment skipped"
fi
chown asterferry:asterferry "$bootstrap_path" "$cache_path" 2>/dev/null || true
chmod 0600 "$bootstrap_path" "$cache_path" 2>/dev/null || true

unit_path="/etc/systemd/system/asterferry-${ROLE}.service"
cat > "$unit_path" <<UNIT
[Unit]
Description=AsterFerry ${ROLE} data-plane node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/asterferry ${ROLE} run --bootstrap ${bootstrap_path}
WorkingDirectory=/var/lib/asterferry
User=asterferry
Group=asterferry
UMask=0077
Restart=on-failure
RestartSec=2s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/asterferry

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$unit_path"
systemctl daemon-reload
systemctl enable --now "asterferry-${ROLE}.service"
echo "AsterFerry ${ROLE} ${NODE_ID} installed and started"
