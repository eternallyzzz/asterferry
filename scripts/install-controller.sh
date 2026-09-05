#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  local status="${1:-0}"
  cat >&2 <<'USAGE'
Usage: install-controller.sh --grpc-advertise HOST:PORT [options]

Install the latest AsterFerry Controller release and register it as a systemd service.

Required:
  --grpc-advertise HOST:PORT  Address that Nodes can reach (never 0.0.0.0)

Options:
  --repo OWNER/REPO           GitHub repository (default: eternallyzzz/asterferry)
  --version VERSION           Pin a release; otherwise use the newest published tag
  --arch amd64|arm64          Override automatic Linux architecture detection
  --release-base-url URL      HTTPS release mirror; requires --version
  --data-dir DIR              Controller data directory (default: /var/lib/asterferry)
  --http-listen HOST:PORT     HTTPS listen address (default: 0.0.0.0:8443)
  --grpc-listen HOST:PORT     mTLS gRPC listen address (default: 0.0.0.0:9443)
  --metrics-listen HOST:PORT  Metrics address (default: 127.0.0.1:9090)
  --username USER             Initial Admin username (default: admin)
  --password-file FILE        Protected file containing the initial Admin password
  -h, --help                  Show this help
USAGE
  exit "$status"
}

die() {
  echo "install-controller: $*" >&2
  exit 1
}

REPO="eternallyzzz/asterferry"
VERSION=""
VERSION_PINNED="false"
EXPECTED_ARCH=""
RELEASE_BASE_URL=""
DATA_DIR="/var/lib/asterferry"
HTTP_LISTEN="0.0.0.0:8443"
GRPC_LISTEN="0.0.0.0:9443"
METRICS_LISTEN="127.0.0.1:9090"
GRPC_ADVERTISE=""
USERNAME="admin"
PASSWORD_FILE=""

while (($# > 0)); do
  case "$1" in
    --repo) REPO="${2:?missing value for --repo}"; shift 2 ;;
    --version) VERSION="${2:?missing value for --version}"; VERSION_PINNED="true"; shift 2 ;;
    --arch) EXPECTED_ARCH="${2:?missing value for --arch}"; shift 2 ;;
    --release-base-url) RELEASE_BASE_URL="${2:?missing value for --release-base-url}"; shift 2 ;;
    --data-dir) DATA_DIR="${2:?missing value for --data-dir}"; shift 2 ;;
    --http-listen) HTTP_LISTEN="${2:?missing value for --http-listen}"; shift 2 ;;
    --grpc-listen) GRPC_LISTEN="${2:?missing value for --grpc-listen}"; shift 2 ;;
    --metrics-listen) METRICS_LISTEN="${2:?missing value for --metrics-listen}"; shift 2 ;;
    --grpc-advertise) GRPC_ADVERTISE="${2:?missing value for --grpc-advertise}"; shift 2 ;;
    --username) USERNAME="${2:?missing value for --username}"; shift 2 ;;
    --password-file) PASSWORD_FILE="${2:?missing value for --password-file}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown option: $1" >&2; usage 2 ;;
  esac
done

[[ -n "$GRPC_ADVERTISE" ]] || { echo "--grpc-advertise is required" >&2; usage 2; }
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "repo must be OWNER/REPO"
[[ "$DATA_DIR" = /* && "$DATA_DIR" != *[[:space:]]* ]] || die "data directory must be an absolute path without whitespace"
[[ -n "$USERNAME" ]] || die "username must not be empty"
if [[ -n "$PASSWORD_FILE" && ! -f "$PASSWORD_FILE" ]]; then
  die "password file does not exist: $PASSWORD_FILE"
fi
if [[ -n "$RELEASE_BASE_URL" && "$RELEASE_BASE_URL" != https://* ]]; then
  die "release base URL must use HTTPS"
fi
if [[ -n "$VERSION" ]]; then
  VERSION="${VERSION#v}"
  [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || die "version must be X.Y.Z or X.Y.Z-rc.N"
elif [[ -n "$RELEASE_BASE_URL" ]]; then
  die "--version is required when --release-base-url is used"
fi

[[ "$(id -u)" -eq 0 ]] || die "run this installer as root (the generated command uses sudo)"

for command_name in awk chmod curl id install mktemp rm runuser sha256sum systemctl tar useradd uname; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done

if [[ -z "$EXPECTED_ARCH" ]]; then
  case "$(uname -m)" in
    x86_64|amd64) EXPECTED_ARCH="amd64" ;;
    aarch64|arm64) EXPECTED_ARCH="arm64" ;;
    *) die "unsupported Linux architecture: $(uname -m)" ;;
  esac
fi
[[ "$EXPECTED_ARCH" == "amd64" || "$EXPECTED_ARCH" == "arm64" ]] || die "arch must be amd64 or arm64"

resolve_latest_version() {
  local response latest_tag
  response="$(curl --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
    -H 'Accept: application/vnd.github+json' \
    -H 'User-Agent: asterferry-installer' \
    "https://api.github.com/repos/${REPO}/releases?per_page=100")" || die "cannot query GitHub releases for ${REPO}"
  latest_tag="$(printf '%s\n' "$response" | awk -F'"' '/"tag_name"[[:space:]]*:/ { if ($4 ~ /^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$/) { print $4; exit } }')"
  [[ -n "$latest_tag" ]] || die "no published semantic release was found for ${REPO}"
  VERSION="${latest_tag#v}"
}

[[ -n "$VERSION" ]] || resolve_latest_version

if ! id asterferry >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin asterferry
fi
install -d -o asterferry -g asterferry -m 0700 "$DATA_DIR"
install -d -m 0755 /usr/local/bin

tmp_dir="$(mktemp -d -t asterferry-controller.XXXXXX)"
password_copy=""
cleanup() {
  if [[ -n "$password_copy" ]]; then
    rm -f "$password_copy"
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

archive="asterferry_${VERSION}_linux_${EXPECTED_ARCH}.tar.gz"
if [[ -n "$RELEASE_BASE_URL" ]]; then
  release_base="${RELEASE_BASE_URL%/}/v${VERSION}"
else
  release_base="https://github.com/${REPO}/releases/download/v${VERSION}"
fi
curl --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
  "$release_base/$archive" --output "$tmp_dir/$archive"
curl --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
  "$release_base/SHA256SUMS" --output "$tmp_dir/SHA256SUMS"
expected="$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1; exit }' "$tmp_dir/SHA256SUMS")"
[[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || die "release checksum for $archive was not found"
printf '%s  %s\n' "$expected" "$tmp_dir/$archive" | sha256sum --check --status - || die "release checksum verification failed"
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
[[ -x "$tmp_dir/asterferry" ]] || die "release archive does not contain asterferry"
install -m 0755 "$tmp_dir/asterferry" /usr/local/bin/asterferry

config_path="$DATA_DIR/controller.json"
if [[ ! -f "$config_path" ]]; then
  init_args=(
    controller init
    --dir "$DATA_DIR"
    --http-listen "$HTTP_LISTEN"
    --grpc-listen "$GRPC_LISTEN"
    --grpc-advertise "$GRPC_ADVERTISE"
    --username "$USERNAME"
  )
  if [[ -n "$METRICS_LISTEN" ]]; then
    init_args+=(--metrics-listen "$METRICS_LISTEN")
  else
    init_args+=(--metrics-listen "")
  fi
  if [[ -n "$PASSWORD_FILE" ]]; then
    password_copy="$DATA_DIR/.admin-password-installer"
    install -o asterferry -g asterferry -m 0600 "$PASSWORD_FILE" "$password_copy"
    init_args+=(--password-file "$password_copy")
  fi
  if [[ -n "$RELEASE_BASE_URL" ]]; then
    init_args+=(--release-base-url "$RELEASE_BASE_URL" --release-version "$VERSION")
  elif [[ "$VERSION_PINNED" == "true" ]]; then
    init_args+=(--release-version "$VERSION")
  fi
  runuser -u asterferry -- /usr/local/bin/asterferry "${init_args[@]}"
else
  echo "existing Controller configuration found; initialization skipped"
fi

unit_path="/etc/systemd/system/asterferry-controller.service"
cat > "$unit_path" <<UNIT
[Unit]
Description=AsterFerry Controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/asterferry controller run --config $config_path
WorkingDirectory=$DATA_DIR
User=asterferry
Group=asterferry
UMask=0077
Restart=on-failure
RestartSec=2s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$unit_path"
systemctl daemon-reload
if systemctl is-active --quiet asterferry-controller.service; then
  systemctl restart asterferry-controller.service
else
  systemctl enable --now asterferry-controller.service
fi

echo "AsterFerry Controller ${VERSION} installed and started"
echo "config: $config_path"
echo "service: asterferry-controller.service"
