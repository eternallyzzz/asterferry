#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  local status="${1:-0}"
  local output_fd=2
  if [[ "$status" == "0" ]]; then output_fd=1; fi
  cat >&$output_fd <<'USAGE'
Usage: install-node.sh --node-id ID --controller HOST:PORT --bootstrap-url HTTPS_URL --token TOKEN --ca-pem-b64 BASE64 [options]

Install the AsterFerry Node release selected by the Controller and register it as a
systemd service. WSL without systemd uses a managed background process instead.

Required (or run interactively and leave them out to be prompted):
  --node-id ID                Immutable Node ID
  --controller HOST:PORT      Controller mTLS gRPC address
  --bootstrap-url HTTPS_URL   Controller HTTPS bootstrap address
  --token TOKEN               One-time enrollment token
  --ca-pem-b64 BASE64         Controller CA certificate encoded as base64

Options:
  --data-dir DIR              Node data directory (default: /var/lib/asterferry)
  --service-name NAME         systemd service name (default: asterferry-node)
  --force, --re-enroll        Replace the existing Node identity using the new one-time token
  -h, --help                  Show this help
USAGE
  exit "$status"
}

die() {
  echo "install-node: $*" >&2
  exit 1
}

step() {
  echo "==> $*"
}

info() {
  echo "    $*"
}

validate_host_port() {
  local value="$1" label="$2" host port
  [[ "$value" != *[[:space:]]* ]] || die "$label must not contain whitespace"
  [[ "$value" =~ ^([^:]+|\[[^]]+\]):[0-9]+$ ]] || die "$label must be host:port (for example controller.example.com:9443)"
  host="${value%:*}"
  port="${value##*:}"
  host="${host#\[}"
  host="${host%\]}"
  [[ -n "$host" && "$host" != "0.0.0.0" && "$host" != "::" ]] || die "$label must identify a reachable host, not an unspecified address"
  (( port >= 1 && port <= 65535 )) || die "$label port must be between 1 and 65535"
}

sanitize_proxy_environment() {
  local name value
  for name in http_proxy https_proxy all_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY; do
    value="${!name-}"
    if [[ "$value" =~ [[:space:]] ]]; then
      echo "install-node: ignoring malformed $name containing whitespace" >&2
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
  local host="${1,,}" controller_host="${2,,}" second_octet
  [[ "$host" == "$controller_host" || "$host" == "localhost" || "$host" == *.localhost ]] && return 0
  [[ "$host" == 127.* || "$host" == 10.* || "$host" == 192.168.* || "$host" == 169.254.* ]] && return 0
  [[ "$host" == "::1" || "$host" == fc*:* || "$host" == fd*:* || "$host" == fe80:* ]] && return 0
  if [[ "$host" =~ ^172\.([0-9]{1,3})\. ]]; then
    second_octet="${BASH_REMATCH[1]}"
    ((second_octet >= 16 && second_octet <= 31)) && return 0
  fi
  return 1
}

DATA_DIR="/var/lib/asterferry"
SERVICE_NAME="asterferry-node"
NODE_ID=""
CONTROLLER=""
BOOTSTRAP_URL=""
TOKEN=""
CA_PEM_B64=""
FORCE=0

while (($# > 0)); do
  case "$1" in
    --data-dir) DATA_DIR="${2:?missing value for --data-dir}"; shift 2 ;;
    --service-name) SERVICE_NAME="${2:?missing value for --service-name}"; shift 2 ;;
    --node-id) NODE_ID="${2:?missing value for --node-id}"; shift 2 ;;
    --controller) CONTROLLER="${2:?missing value for --controller}"; shift 2 ;;
    --bootstrap-url) BOOTSTRAP_URL="${2:?missing value for --bootstrap-url}"; shift 2 ;;
    --token) TOKEN="${2:?missing value for --token}"; shift 2 ;;
    --ca-pem-b64) CA_PEM_B64="${2:?missing value for --ca-pem-b64}"; shift 2 ;;
    --force|--re-enroll) FORCE=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown option: $1" >&2; usage 2 ;;
  esac
done

if [[ -t 0 ]]; then
  if [[ -z "$NODE_ID" ]]; then
    read -r -p "Node ID: " NODE_ID || true
  fi
  if [[ -z "$CONTROLLER" ]]; then
    read -r -p "Controller address (host:port): " CONTROLLER || true
  fi
  if [[ -z "$BOOTSTRAP_URL" ]]; then
    read -r -p "Controller HTTPS bootstrap URL: " BOOTSTRAP_URL || true
  fi
  if [[ -z "$TOKEN" ]]; then
    read -r -s -p "Enrollment token: " TOKEN || true
    echo
  fi
  if [[ -z "$CA_PEM_B64" ]]; then
    read -r -s -p "Controller CA certificate (base64): " CA_PEM_B64 || true
    echo
  fi
fi
[[ -n "$NODE_ID" && -n "$CONTROLLER" && -n "$BOOTSTRAP_URL" && -n "$TOKEN" && -n "$CA_PEM_B64" ]] || {
  echo "--node-id, --controller, --bootstrap-url, --token and --ca-pem-b64 are required" >&2
  usage 2
}
validate_host_port "$CONTROLLER" "controller address"
[[ "$BOOTSTRAP_URL" == https://* && "$BOOTSTRAP_URL" != *[[:space:]]* ]] || die "bootstrap URL must be an absolute HTTPS URL"
BOOTSTRAP_URL="${BOOTSTRAP_URL%/}"
BOOTSTRAP_HOST="$(url_host "$BOOTSTRAP_URL")"
[[ -n "$BOOTSTRAP_HOST" ]] || die "bootstrap URL does not contain a host"
[[ "$DATA_DIR" = /* && "$DATA_DIR" != *[[:space:]]* ]] || die "data directory must be an absolute path without whitespace"
[[ "$SERVICE_NAME" =~ ^[A-Za-z0-9_.@:-]+$ ]] || die "service name contains unsupported characters"

sanitize_proxy_environment

[[ "$(id -u)" -eq 0 ]] || die "run this installer as root, for example: sudo bash $0"

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

required_commands=(awk base64 chmod chown cmp cp curl date grep id install mktemp rm runuser sha256sum tar useradd uname)
if [[ "$service_mode" == "systemd" ]]; then
  required_commands+=(systemctl)
else
  required_commands+=(nohup sleep tr)
fi
for command_name in "${required_commands[@]}"; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done

case "$(uname -m)" in
  x86_64|amd64) NODE_ARCH="amd64" ;;
  aarch64|arm64) NODE_ARCH="arm64" ;;
  *) die "unsupported Linux architecture: $(uname -m)" ;;
esac

step "Preparing Node data directory"
if ! id asterferry >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin asterferry
fi
install -d -o asterferry -g asterferry -m 0700 "$DATA_DIR"
binary_dir="$DATA_DIR/bin"
binary_path="$binary_dir/asterferry"
install -d -o asterferry -g asterferry -m 0750 "$binary_dir"

tmp_dir="$(mktemp -d -t asterferry-node.XXXXXX)"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT

ca_path="$DATA_DIR/controller-ca.crt"
bootstrap_path="$DATA_DIR/node-bootstrap.json"
cache_path="$DATA_DIR/snapshot.cache"
cache_key_path="$DATA_DIR/snapshot.key"
decommission_marker_path="$bootstrap_path.decommissioned"
unit_path="/etc/systemd/system/${SERVICE_NAME}.service"
wsl_pid_path="$DATA_DIR/node.pid"
wsl_log_path="$DATA_DIR/node.log"
wsl_launcher_path="/usr/local/sbin/asterferry-node-wsl-start"
wsl_boot_path="/usr/local/sbin/asterferry-node-wsl-boot"
wsl_boot_state_dir="/etc/asterferry"
wsl_previous_command_path="$wsl_boot_state_dir/wsl-boot-previous-command"

if [[ -f "$decommission_marker_path" && "$FORCE" -ne 1 ]]; then
  die "node is decommissioned; rerun with --force/--re-enroll and a fresh enrollment token"
fi
printf '%s' "$CA_PEM_B64" | base64 --decode > "$tmp_dir/controller-ca.crt" || die "invalid Controller CA base64"
chmod 0600 "$tmp_dir/controller-ca.crt"
has_existing_identity=0
for state_path in "$bootstrap_path" "$cache_path" "$cache_key_path" "$decommission_marker_path" "$unit_path" "$wsl_pid_path"; do
  if [[ -e "$state_path" ]]; then
    has_existing_identity=1
    break
  fi
done
existing_ca_differs=0
if [[ -f "$ca_path" ]]; then
  if ! cmp -s "$tmp_dir/controller-ca.crt" "$ca_path"; then
    existing_ca_differs=1
  fi
fi
replace_orphaned_ca=0
if [[ "$existing_ca_differs" -eq 1 && "$has_existing_identity" -eq 0 ]]; then
  replace_orphaned_ca=1
fi
if [[ "$existing_ca_differs" -eq 1 && "$has_existing_identity" -eq 1 && "$FORCE" -ne 1 ]]; then
  die "this machine already has a Node identity for a different Controller; rerun with --force/--re-enroll and a fresh enrollment token to replace it"
fi

release_metadata="$tmp_dir/node-release.json"
step "Requesting Node release from Controller"
curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.3 \
  --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 \
  --noproxy "$BOOTSTRAP_HOST" \
  --cacert "$tmp_dir/controller-ca.crt" \
  --header "X-AsterFerry-Enrollment-Token: $TOKEN" \
  "$BOOTSTRAP_URL/bootstrap/node/release" --output "$release_metadata"

json_field() {
  local key="$1"
  awk -F'"' -v key="$key" '$2 == key { print $4; exit }' "$release_metadata"
}

VERSION="$(json_field version)"
RELEASE_BASE_URL="$(json_field release_base_url)"
artifact_key="linux/$NODE_ARCH"
ARCHIVE="$(awk -F'"' -v key="$artifact_key" '$2 == key { print $4; exit }' "$release_metadata")"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || die "Controller returned an invalid Node release version"
[[ "$RELEASE_BASE_URL" == https://* && "$RELEASE_BASE_URL" != *[[:space:]]* ]] || die "Controller returned an invalid Node release URL"
[[ "$ARCHIVE" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "Controller did not provide a valid Linux Node artifact"
info "Node release ${VERSION} (${NODE_ARCH})"
RELEASE_HOST="$(url_host "$RELEASE_BASE_URL")"
[[ -n "$RELEASE_HOST" ]] || die "Controller returned a Node release URL without a host"
release_proxy_args=()
if is_direct_host "$RELEASE_HOST" "$BOOTSTRAP_HOST"; then
  release_proxy_args=(--noproxy "$RELEASE_HOST")
fi
release_tls_args=()
if [[ "${RELEASE_HOST,,}" == "${BOOTSTRAP_HOST,,}" ]]; then
  release_tls_args=(--cacert "$tmp_dir/controller-ca.crt")
fi

release_base="${RELEASE_BASE_URL%/}/v${VERSION}"
archive_path="$tmp_dir/$ARCHIVE"
step "Downloading and verifying Node ${NODE_ARCH} release"
curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.3 \
  --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 \
  "${release_proxy_args[@]}" \
  "${release_tls_args[@]}" \
  "$release_base/$ARCHIVE" --output "$archive_path"
curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.3 \
  --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 \
  "${release_proxy_args[@]}" \
  "${release_tls_args[@]}" \
  "$release_base/SHA256SUMS" --output "$tmp_dir/SHA256SUMS"
expected="$(awk -v name="$ARCHIVE" '{ checksum_name=$2; sub(/\r$/, "", checksum_name); if (checksum_name == name || checksum_name == "*" name) { print $1; exit } }' "$tmp_dir/SHA256SUMS")"
[[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || die "release checksum for $ARCHIVE was not found"
printf '%s  %s\n' "$expected" "$archive_path" | sha256sum --check --status - || die "release checksum verification failed"
tar -xzf "$archive_path" -C "$tmp_dir"
[[ -f "$tmp_dir/asterferry" ]] || die "release archive does not contain asterferry"
chmod 0755 "$tmp_dir/asterferry"

write_wsl_launcher() {
  local launcher_source="$tmp_dir/asterferry-node-wsl-start"
  cat > "$launcher_source" <<'WSL_LAUNCHER'
#!/usr/bin/env bash
set -Eeuo pipefail

action="${1:-start}"
data_dir="${2:?data directory is required}"
bootstrap_path="$data_dir/node-bootstrap.json"
pid_path="$data_dir/node.pid"
log_path="$data_dir/node.log"
binary_path="$data_dir/bin/asterferry"

matching_process() {
  local pid="$1" command_line
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  [[ -r "/proc/$pid/cmdline" ]] || return 1
  command_line="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
  [[ "$command_line" == *"asterferry node run"* && "$command_line" == *"--bootstrap $bootstrap_path"* ]]
}

read_pid() {
  local pid=""
  if [[ -f "$pid_path" ]]; then
    read -r pid < "$pid_path" || true
  fi
  printf '%s' "$pid"
}

stop_node() {
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
    stop_node
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

[[ -f "$bootstrap_path" ]] || exit 0
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
  runuser -u asterferry -- sh -c 'nohup "$1" node run --bootstrap "$2" --service-mode wsl >>"$3" 2>&1 </dev/null & printf "%s\n" "$!" > "$4"' sh "$binary_path" "$bootstrap_path" "$log_path" "$pid_path"
for _ in 1 2 3 4 5; do
  pid="$(read_pid)"
  if [[ -n "$pid" ]] && matching_process "$pid"; then
    exit 0
  fi
  sleep 1
done
echo "AsterFerry Node did not stay running; inspect $log_path" >&2
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
        '  echo "existing WSL boot command failed; continuing with AsterFerry Node" >&2' \
        'fi'
    fi
    printf 'exec %q start %q\n' "$wsl_launcher_path" "$DATA_DIR"
  } > "$tmp_dir/asterferry-node-wsl-boot"
  chmod 0755 "$tmp_dir/asterferry-node-wsl-boot"
  install -d -m 0755 /usr/local/sbin
  install -o root -g root -m 0755 "$tmp_dir/asterferry-node-wsl-boot" "$wsl_boot_path"

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

if [[ "$service_mode" == "wsl" ]]; then
  write_wsl_launcher
  "$wsl_launcher_path" stop "$DATA_DIR"
fi

if [[ "$FORCE" -eq 1 ]]; then
  if [[ "$service_mode" == "systemd" ]] && systemctl is-active --quiet "$SERVICE_NAME.service"; then
    systemctl stop "$SERVICE_NAME.service"
  fi
fi
if [[ "$FORCE" -eq 1 || "$replace_orphaned_ca" -eq 1 ]]; then
  recovery_dir="$DATA_DIR/recovery/$(date -u +%Y%m%d%H%M%S)"
  install -d -o asterferry -g asterferry -m 0700 "$recovery_dir"
  for state_path in "$ca_path" "$bootstrap_path" "$cache_path" "$cache_key_path" "$decommission_marker_path"; do
    if [[ -e "$state_path" ]]; then
      cp -p "$state_path" "$recovery_dir/$(basename "$state_path")"
    fi
  done
fi
if [[ "$FORCE" -eq 1 ]]; then
  rm -f "$bootstrap_path" "$cache_path" "$cache_key_path" "$decommission_marker_path"
fi
if [[ "$replace_orphaned_ca" -eq 1 ]]; then
  echo "replacing Controller CA left by an incomplete Node installation"
fi
if [[ ! -f "$ca_path" || "$existing_ca_differs" -eq 1 ]]; then
  install -o asterferry -g asterferry -m 0644 "$tmp_dir/controller-ca.crt" "$ca_path"
fi
chown asterferry:asterferry "$ca_path"
chmod 0644 "$ca_path"
install -o asterferry -g asterferry -m 0755 "$tmp_dir/asterferry" "$binary_path"

if [[ "$FORCE" -eq 1 || ! -f "$bootstrap_path" ]]; then
  step "Enrolling Node with Controller"
  runuser -u asterferry -- "$binary_path" node enroll \
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

step "Registering and starting Node service"
if [[ "$service_mode" == "systemd" ]]; then
remove_wsl_boot_hook
cat > "$unit_path" <<UNIT
[Unit]
Description=AsterFerry Node data-plane service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$binary_path node run --bootstrap $bootstrap_path --service-mode systemd
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
if systemctl is-active --quiet "$SERVICE_NAME.service"; then
  systemctl restart "$SERVICE_NAME.service"
else
  systemctl enable --now "$SERVICE_NAME.service"
fi
else
  configure_wsl_boot
  "$wsl_launcher_path" start "$DATA_DIR"
fi

if [[ "$FORCE" -eq 1 ]]; then
  echo "AsterFerry Node ${NODE_ID} ${VERSION} re-enrolled and started"
else
  echo "AsterFerry Node ${NODE_ID} ${VERSION} installed and started"
fi
if [[ "$service_mode" == "systemd" ]]; then
  echo "service: ${SERVICE_NAME}.service"
  echo "status: systemctl status ${SERVICE_NAME}.service"
  echo "logs:   journalctl -u ${SERVICE_NAME}.service -f"
else
  echo "mode: WSL managed background process (auto-start on WSL launch)"
  echo "status: $wsl_launcher_path status $DATA_DIR"
  echo "log: $wsl_log_path"
fi
