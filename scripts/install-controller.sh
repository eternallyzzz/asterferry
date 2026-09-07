#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  local status="${1:-0}"
  local output_fd=2
  if [[ "$status" == "0" ]]; then output_fd=1; fi
  cat >&$output_fd <<'USAGE'
Usage: install-controller.sh --grpc-advertise HOST:PORT [options]

Install the latest AsterFerry Controller release and register it as a systemd
service. WSL without systemd uses a managed background process instead.

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

sanitize_proxy_environment() {
  local name value
  for name in http_proxy https_proxy all_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY; do
    value="${!name-}"
    if [[ "$value" =~ [[:space:]] ]]; then
      echo "install-controller: ignoring malformed $name containing whitespace" >&2
      unset "$name"
    fi
  done
}

url_host() {
  local authority="${1#https://}"
  authority="${authority%%/*}"
  if [[ "$authority" == \[*\]* ]]; then
    authority="${authority#\[}"
    printf '%s' "${authority%%\]*}"
    return
  fi
  printf '%s' "${authority%%:*}"
}

is_direct_host() {
  local host="${1,,}" second_octet
  [[ "$host" == "localhost" || "$host" == *.localhost ]] && return 0
  [[ "$host" == 127.* || "$host" == 10.* || "$host" == 192.168.* || "$host" == 169.254.* ]] && return 0
  [[ "$host" == "::1" || "$host" == fc*:* || "$host" == fd*:* || "$host" == fe80:* ]] && return 0
  if [[ "$host" =~ ^172\.([0-9]{1,3})\. ]]; then
    second_octet="${BASH_REMATCH[1]}"
    ((second_octet >= 16 && second_octet <= 31)) && return 0
  fi
  return 1
}

REPO="eternallyzzz/asterferry"
VERSION=""
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
    --version) VERSION="${2:?missing value for --version}"; shift 2 ;;
    --arch) EXPECTED_ARCH="${2:?missing value for --arch}"; shift 2 ;;
    --release-base-url) RELEASE_BASE_URL="${2:?missing value for --release-base-url}"; shift 2 ;;
    --data-dir) DATA_DIR="${2:?missing value for --data-dir}"; shift 2 ;;
    --http-listen) HTTP_LISTEN="${2:?missing value for --http-listen}"; shift 2 ;;
    --grpc-listen) GRPC_LISTEN="${2:?missing value for --grpc-listen}"; shift 2 ;;
    --metrics-listen)
      (($# >= 2)) || die "missing value for --metrics-listen"
      METRICS_LISTEN="$2"
      shift 2
      ;;
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

sanitize_proxy_environment

[[ "$(id -u)" -eq 0 ]] || die "run this installer as root (the generated command uses sudo)"

is_wsl=0
if grep -qi microsoft /proc/version 2>/dev/null || [[ -n "${WSL_INTEROP:-}" ]] || [[ -e /run/WSL ]]; then
  is_wsl=1
fi
if [[ -d /run/systemd/system ]]; then
  service_mode="systemd"
elif [[ "$is_wsl" -eq 1 ]]; then
  service_mode="wsl"
else
  die "systemd is required but is not running on this Linux host"
fi

required_commands=(awk chmod cp curl grep id install mktemp rm runuser sha256sum tar useradd uname)
if [[ "$service_mode" == "systemd" ]]; then
  required_commands+=(systemctl)
else
  required_commands+=(nohup sleep tr)
fi
for command_name in "${required_commands[@]}"; do
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
  response="$(curl --disable --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
    -H 'Accept: application/vnd.github+json' \
    -H 'User-Agent: asterferry-installer' \
    "https://api.github.com/repos/${REPO}/releases/latest")" || die "cannot query GitHub stable release for ${REPO}"
  latest_tag="$(printf '%s\n' "$response" | awk -F'"' '/"tag_name"[[:space:]]*:/ { print $4; exit }')"
  [[ "$latest_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no published stable release was found for ${REPO}"
  VERSION="${latest_tag#v}"
}

[[ -n "$VERSION" ]] || resolve_latest_version

if ! id asterferry >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin asterferry
fi
install -d -o asterferry -g asterferry -m 0700 "$DATA_DIR"
binary_dir="$DATA_DIR/bin"
binary_path="$binary_dir/asterferry"
install -d -o asterferry -g asterferry -m 0750 "$binary_dir"

tmp_dir="$(mktemp -d -t asterferry-controller.XXXXXX)"
password_copy=""
cleanup() {
  if [[ -n "$password_copy" ]]; then
    rm -f "$password_copy"
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

wsl_pid_path="$DATA_DIR/controller.pid"
wsl_log_path="$DATA_DIR/controller.log"
wsl_launcher_path="/usr/local/sbin/asterferry-controller-wsl-start"
wsl_boot_path="/usr/local/sbin/asterferry-controller-wsl-boot"
wsl_boot_state_dir="/etc/asterferry"
wsl_previous_command_path="$wsl_boot_state_dir/wsl-boot-previous-command"

archive="asterferry_${VERSION}_linux_${EXPECTED_ARCH}.tar.gz"
if [[ -n "$RELEASE_BASE_URL" ]]; then
  release_base="${RELEASE_BASE_URL%/}/v${VERSION}"
else
  release_base="https://github.com/${REPO}/releases/download/v${VERSION}"
fi
release_proxy_args=()
release_host="$(url_host "$release_base")"
if is_direct_host "$release_host"; then
  release_proxy_args=(--noproxy "$release_host")
fi
curl --disable --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
  --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 \
  "${release_proxy_args[@]}" \
  "$release_base/$archive" --output "$tmp_dir/$archive"
curl --disable --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
  --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 \
  "${release_proxy_args[@]}" \
  "$release_base/SHA256SUMS" --output "$tmp_dir/SHA256SUMS"
expected="$(awk -v name="$archive" '{ checksum_name=$2; sub(/\r$/, "", checksum_name); if (checksum_name == name || checksum_name == "*" name) { print $1; exit } }' "$tmp_dir/SHA256SUMS")"
[[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || die "release checksum for $archive was not found"
printf '%s  %s\n' "$expected" "$tmp_dir/$archive" | sha256sum --check --status - || die "release checksum verification failed"
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
[[ -f "$tmp_dir/asterferry" ]] || die "release archive does not contain asterferry"
chmod 0755 "$tmp_dir/asterferry"
install -o asterferry -g asterferry -m 0755 "$tmp_dir/asterferry" "$binary_path"

download_release_file() {
  local name="$1"
  local destination="$tmp_dir/$name"
  curl --disable --fail --silent --show-error --location --proto '=https' --tlsv1.3 \
    --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 \
    "${release_proxy_args[@]}" \
    "$release_base/$name" --output "$destination"
  local expected
  expected="$(awk -v name="$name" '{ checksum_name=$2; sub(/\r$/, "", checksum_name); if (checksum_name == name || checksum_name == "*" name) { print $1; exit } }' "$tmp_dir/SHA256SUMS")"
  [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || die "release checksum for $name was not found"
  printf '%s  %s\n' "$expected" "$destination" | sha256sum --check --status - || die "release checksum verification failed for $name"
}

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
  runuser -u asterferry -- "$binary_path" "${init_args[@]}"
else
  echo "existing Controller configuration found; initialization skipped"
fi

node_installers_dir="$DATA_DIR/node-installers"
install -d -o asterferry -g asterferry -m 0750 "$node_installers_dir"
for node_asset in install-node.sh install-node.ps1 node-release.json; do
  download_release_file "$node_asset"
  install -o asterferry -g asterferry -m 0640 "$tmp_dir/$node_asset" "$node_installers_dir/$node_asset"
done

write_wsl_launcher() {
  local launcher_source="$tmp_dir/asterferry-controller-wsl-start"
  cat > "$launcher_source" <<'WSL_LAUNCHER'
#!/usr/bin/env bash
set -Eeuo pipefail

action="${1:-start}"
data_dir="${2:?data directory is required}"
config_path="$data_dir/controller.json"
pid_path="$data_dir/controller.pid"
log_path="$data_dir/controller.log"
binary_path="$data_dir/bin/asterferry"

matching_process() {
  local pid="$1" command_line
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  [[ -r "/proc/$pid/cmdline" ]] || return 1
  command_line="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
  [[ "$command_line" == *"asterferry controller run"* && "$command_line" == *"--config $config_path"* ]]
}

read_pid() {
  local pid=""
  if [[ -f "$pid_path" ]]; then
    read -r pid < "$pid_path" || true
  fi
  printf '%s' "$pid"
}

stop_controller() {
  local pid="$(read_pid)"
  if [[ -n "$pid" ]] && matching_process "$pid"; then
    kill "$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5; do
      matching_process "$pid" || break
      sleep 1
    done
    if matching_process "$pid"; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  fi
  rm -f "$pid_path"
}

case "$action" in
  stop)
    stop_controller
    exit 0
    ;;
  status)
    pid="$(read_pid)"
    if [[ -n "$pid" ]] && matching_process "$pid"; then
      echo "running ($pid)"
      exit 0
    fi
    echo "stopped"
    exit 3
    ;;
  start) ;;
  *)
    echo "usage: $0 {start|stop|status} DATA_DIR" >&2
    exit 2
    ;;
esac

[[ -f "$config_path" ]] || exit 0
pid="$(read_pid)"
if [[ -n "$pid" ]] && matching_process "$pid"; then
  exit 0
fi
rm -f "$pid_path"
if [[ ! -e "$log_path" ]]; then
  : > "$log_path"
fi
chown asterferry:asterferry "$log_path"
chmod 0600 "$log_path"
runuser -u asterferry -- sh -c 'nohup "$1" controller run --config "$2" --service-mode wsl >>"$3" 2>&1 </dev/null & printf "%s\n" "$!" > "$4"' sh "$binary_path" "$config_path" "$log_path" "$pid_path"
for _ in 1 2 3 4 5; do
  pid="$(read_pid)"
  if [[ -n "$pid" ]] && matching_process "$pid"; then
    exit 0
  fi
  sleep 1
done
echo "AsterFerry Controller did not stay running; inspect $log_path" >&2
exit 1
WSL_LAUNCHER
  chmod 0755 "$launcher_source"
  install -d -m 0755 /usr/local/sbin
  install -o root -g root -m 0755 "$launcher_source" "$wsl_launcher_path"
}

configure_wsl_boot() {
  local wsl_conf="/etc/wsl.conf"
  local updated_conf="$tmp_dir/wsl.conf"
  local existing_command=""
  local previous_command=""
  if [[ -f "$wsl_conf" ]]; then
    existing_command="$(awk '
      BEGIN { in_boot=0 }
      /^[[:space:]]*#/ { next }
      /^[[:space:]]*\[/ {
        in_boot=(tolower($0) ~ /^[[:space:]]*\[boot\][[:space:]]*$/)
        next
      }
      in_boot && $0 ~ /^[[:space:]]*command[[:space:]]*=/ {
        sub(/^[^=]*=[[:space:]]*/, "", $0)
        sub(/\r$/, "", $0)
        print
        exit
      }
    ' "$wsl_conf")"
  fi
  if [[ "$existing_command" == "$wsl_boot_path" && -x "$wsl_boot_path" ]]; then
    return
  fi
  install -d -o root -g root -m 0755 "$wsl_boot_state_dir"
  if [[ ! -f "$wsl_previous_command_path" ]]; then
    previous_command="$existing_command"
    if [[ "$previous_command" == "/usr/local/sbin/asterferry-controller-wsl-boot" || "$previous_command" == "/usr/local/sbin/asterferry-node-wsl-boot" ]]; then
      previous_command=""
    fi
    printf '%s' "$previous_command" > "$tmp_dir/wsl-boot-previous-command"
    install -o root -g root -m 0600 "$tmp_dir/wsl-boot-previous-command" "$wsl_previous_command_path"
  fi
  {
    printf '%s\n' '#!/usr/bin/env bash' 'set -Eeuo pipefail'
    if [[ -n "$existing_command" ]]; then
      printf '%s\n' "if ! /usr/bin/env bash -c $(printf '%q' "$existing_command"); then" \
        '  echo "existing WSL boot command failed; continuing with AsterFerry Controller" >&2' \
        'fi'
    fi
    printf 'exec %q start %q\n' "$wsl_launcher_path" "$DATA_DIR"
  } > "$tmp_dir/asterferry-controller-wsl-boot"
  chmod 0755 "$tmp_dir/asterferry-controller-wsl-boot"
  install -d -m 0755 /usr/local/sbin
  install -o root -g root -m 0755 "$tmp_dir/asterferry-controller-wsl-boot" "$wsl_boot_path"

  if [[ -f "$wsl_conf" ]]; then
    awk -v command="$wsl_boot_path" '
      BEGIN { in_boot=0; saw_boot=0; replaced=0 }
      /^[[:space:]]*\[/ {
        if (in_boot && !replaced) {
          print "command=" command
          replaced=1
        }
        in_boot=(tolower($0) ~ /^[[:space:]]*\[boot\][[:space:]]*$/)
        if (in_boot) saw_boot=1
        print
        next
      }
      in_boot && $0 ~ /^[[:space:]]*command[[:space:]]*=/ {
        if (!replaced) print "command=" command
        replaced=1
        next
      }
      { print }
      END {
        if (in_boot && !replaced) print "command=" command
        if (!saw_boot) {
          print ""
          print "[boot]"
          print "command=" command
        }
      }
    ' "$wsl_conf" > "$updated_conf"
  else
    printf '%s\n' '[boot]' "command=$wsl_boot_path" > "$updated_conf"
  fi
  if ! cmp -s "$updated_conf" "$wsl_conf" 2>/dev/null; then
    local recovery_dir="$DATA_DIR/recovery/$(date -u +%Y%m%d%H%M%S)"
    install -d -o asterferry -g asterferry -m 0700 "$recovery_dir"
    if [[ -f "$wsl_conf" ]]; then
      cp -p "$wsl_conf" "$recovery_dir/wsl.conf"
    fi
    install -o root -g root -m 0644 "$updated_conf" "$wsl_conf"
    echo "configured WSL boot autostart in $wsl_conf"
    echo "the setting takes effect after 'wsl --shutdown' from Windows"
  fi
}

remove_wsl_boot_hook() {
  local wsl_conf="/etc/wsl.conf"
  local updated_conf="$tmp_dir/wsl.conf"
  local existing_command=""
  local previous_command=""
  if [[ ! -f "$wsl_conf" ]]; then
    return
  fi
  existing_command="$(awk '
    BEGIN { in_boot=0 }
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*\[/ {
      in_boot=(tolower($0) ~ /^[[:space:]]*\[boot\][[:space:]]*$/)
      next
    }
    in_boot && $0 ~ /^[[:space:]]*command[[:space:]]*=/ {
      sub(/^[^=]*=[[:space:]]*/, "", $0)
      sub(/\r$/, "", $0)
      print
      exit
    }
  ' "$wsl_conf")"
  if [[ "$existing_command" != "/usr/local/sbin/asterferry-controller-wsl-boot" && "$existing_command" != "/usr/local/sbin/asterferry-node-wsl-boot" ]]; then
    return
  fi
  if [[ -f "$wsl_previous_command_path" ]]; then
    previous_command="$(<"$wsl_previous_command_path")"
  fi
  awk -v replacement="$previous_command" '
    BEGIN { in_boot=0 }
    /^[[:space:]]*\[/ {
      in_boot=(tolower($0) ~ /^[[:space:]]*\[boot\][[:space:]]*$/)
      print
      next
    }
    in_boot && $0 ~ /^[[:space:]]*command[[:space:]]*=/ {
      if (replacement != "") print "command=" replacement
      next
    }
    { print }
  ' "$wsl_conf" > "$updated_conf"
  if ! cmp -s "$updated_conf" "$wsl_conf"; then
    local recovery_dir="$DATA_DIR/recovery/$(date -u +%Y%m%d%H%M%S)"
    install -d -o asterferry -g asterferry -m 0700 "$recovery_dir"
    cp -p "$wsl_conf" "$recovery_dir/wsl.conf"
    install -o root -g root -m 0644 "$updated_conf" "$wsl_conf"
    echo "removed AsterFerry WSL boot autostart because systemd is available"
  fi
  rm -f "$wsl_previous_command_path"
}

unit_path="/etc/systemd/system/asterferry-controller.service"
if [[ "$service_mode" == "systemd" ]]; then
remove_wsl_boot_hook
cat > "$unit_path" <<UNIT
[Unit]
Description=AsterFerry Controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$binary_path controller run --config $config_path --service-mode systemd
WorkingDirectory=$DATA_DIR
User=asterferry
Group=asterferry
UMask=0077
Restart=on-failure
RestartSec=2s
KillMode=process
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
else
  write_wsl_launcher
  configure_wsl_boot
  "$wsl_launcher_path" stop "$DATA_DIR"
  "$wsl_launcher_path" start "$DATA_DIR"
fi

echo "AsterFerry Controller ${VERSION} installed and started"
echo "config: $config_path"
if [[ "$service_mode" == "systemd" ]]; then
  echo "service: asterferry-controller.service"
else
  echo "mode: WSL managed background process (auto-start on WSL launch)"
  echo "status: $wsl_launcher_path status $DATA_DIR"
  echo "log: $wsl_log_path"
fi
