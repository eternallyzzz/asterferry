# AsterFerry End-to-End Quick Start (English)

This is a copy-and-run deployment guide. The example uses three servers:

```text
                         C: Controller
                 REST/Dashboard 8443/tcp
                       mTLS gRPC 9443/tcp
                              ▲
                    A and B actively connect to C:9443
                              │
                              │ control plane: identity, config, snapshots, scheduling
                              │
 A: private Agent  ───── UDP 4433 ─────>  B: public Gateway
 private service                                  public service port 28080/tcp
 127.0.0.1:18080                                  public service port 28081/udp

                   Business traffic does not pass through C
 Client ───────────────> B:28080 ── AFDP/2 ──> A:18080
```

## 1. Roles and ports

| Node | Responsibility | Required connectivity |
| --- | --- | --- |
| C · Controller | Identity, RBAC, configuration, snapshots, scheduling, Dashboard and audit | Admin clients reach `8443/tcp`; A and B reach `9443/tcp` |
| B · Gateway | Listens for AFDP/2 and public service ports; accepts public connections | Receives A's Agent connection on `4433/udp`; receives clients on service ports such as `28080/tcp` and `28081/udp` |
| A · Agent | Connects to C and B, and reaches private services on A | Outbound access to `C:9443` and `B:4433/udp`; access to `127.0.0.1:18080` |

Do not confuse the ports:

- `C:8443` is the Controller HTTPS REST API and Dashboard, not a relay port.
- `C:9443` is the mTLS gRPC control channel used by Agents and Gateways.
- `B:4433/udp` is the Gateway AFDP/2 endpoint. The Agent actively dials it.
- `B:28080/tcp` is a public service port; `A:18080` is the private target on the Agent host.
- The Controller does not carry service payloads. The data path is client → Gateway → AFDP/2 → Agent → private service.

Replace these example values with real addresses:

| Placeholder | Example | Meaning |
| --- | --- | --- |
| `C_HOST` | `controller.example.com` | Stable DNS name or fixed IP for C |
| `B_HOST` | `gateway.example.com` | Public DNS name or public IP for B |
| `A_SERVICE` | `127.0.0.1:18080` | Private service running on A |

## 2. Prepare the binary and network

Use a release package or build from source. Building from source requires Go `1.26.7`; building the Dashboard separately requires Node.js 24 and npm 11+.

Build from the repository:

```powershell
# Windows
go build -o asterferry.exe ./cmd/asterferry
```

```sh
# Linux / WSL
go build -o asterferry ./cmd/asterferry
```

Install the binary on C, B and A, and run the same release version on all three hosts. The current release supports AFDP/2 only; AFDP/1 and AFDP/2 are not wire-compatible.

In the commands below, `asterferry` means this binary. On Windows PowerShell,
replace it with `.\asterferry.exe`; on Linux/WSL, use `./asterferry`. Do not
mix the PowerShell and POSIX shell blocks.

Configure DNS or `/etc/hosts` so that C and B resolve from the relevant machines. For example:

```text
192.0.2.30    controller.example.com
198.51.100.20 gateway.example.com
```

At minimum, configure the firewalls as follows:

- C: allow `8443/tcp` from the admin client and `9443/tcp` from A and B.
- B: allow `4433/udp` from A and the configured TCP/UDP service ports from clients.
- A: allow the Agent process to reach `A_SERVICE`. If the target binds to `127.0.0.1`, the Agent must run on the same host.

## 3. Initialize and start the Controller on C

### 3.1 Create the Admin password

The CLI can generate a random initial password, or read one from a protected file. With `--password-file`, the file is read only during initialization:

```powershell
# Windows PowerShell; the input is not echoed. Write BOM-free UTF-8 for compatibility with Windows PowerShell 5.1.
$AdminPassword = Read-Host "Controller Admin password"
$Utf8NoBom = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
[System.IO.File]::WriteAllText((Join-Path (Get-Location) 'admin-password.txt'), $AdminPassword, $Utf8NoBom)
Remove-Variable AdminPassword, Utf8NoBom
```

```sh
# Linux / WSL
umask 077
read -r -s ADMIN_PASSWORD
printf '%s\n' "$ADMIN_PASSWORD" > ./admin-password.txt
unset ADMIN_PASSWORD
```

Do not put the password directly in the command line, where it may enter shell history. Protect the password file after initialization and remove the temporary copy according to your security policy.

### 3.2 Initialize

The host portions of `--http-listen` and `--grpc-listen` should be stable names or IPs that C can bind and that clients will actually use. The generated Controller certificate includes these names in its SAN. Do not replace the stable name with `0.0.0.0` in this remote deployment example.

```powershell
# Windows; replace the example host with C's real DNS name or fixed IP
asterferry.exe controller init `
  --dir .\controller `
  --username admin `
  --password-file .\admin-password.txt `
  --http-listen controller.example.com:8443 `
  --grpc-listen controller.example.com:9443
```

```sh
# Linux / WSL
./asterferry controller init \
  --dir ./controller \
  --username admin \
  --password-file ./admin-password.txt \
  --http-listen controller.example.com:8443 \
  --grpc-listen controller.example.com:9443
```

If neither `--password-file` nor `--password` is supplied, the CLI generates a random password and prints it once in the initialization output. The password is not stored in `controller.json`.

### 3.3 Start and check

```powershell
asterferry.exe controller run --config .\controller\controller.json
```

```sh
./asterferry controller run --config ./controller/controller.json
```

From another terminal, check health:

```sh
asterferry healthcheck \
  --url https://controller.example.com:8443/healthz \
  --insecure-tls
```

`--insecure-tls` is for a local health probe only. Open the Dashboard at:

```text
https://controller.example.com:8443/dashboard/
```

Initialization creates a self-signed Controller CA. In production, install `controller/ca/ca.crt` in the admin client's trust store. For temporary development, you may accept the browser warning manually. Never copy `ca.key`, `master.key`, or the Controller TLS private key to A or B.

## 4. Register nodes and configure their specs in the Dashboard

Log in to the Dashboard and follow this order. All business configuration is written to the Controller; A and B do not read business YAML.

### 4.1 Register the Gateway and Agent

Open the Nodes page and create:

| Node ID | Role | Name | Labels |
| --- | --- | --- | --- |
| `gw-public` | Gateway | Public Gateway | `{"site":"public"}` |
| `agent-internal` | Agent | Internal Agent | `{"site":"internal"}` |

Node IDs become identity names after enrollment and should be treated as immutable. Certificate state may initially be `pending`; it becomes `active` after enrollment.

### 4.2 Configure the Gateway spec

Open `gw-public` → **Spec** and save this minimum working example. Replace `gateway.example.com` with B's public address; B must accept UDP `4433`.

```json
{
  "node_id": "gw-public",
  "public_endpoints": ["gateway.example.com:4433"],
  "listeners": [],
  "labels": {"site": "public"},
  "capacity": {
    "max_agents": 128,
    "max_connections": 4096,
    "max_services": 4096
  },
  "port_pool": {
    "tcp": [{"min": 28080, "max": 28080}],
    "udp": [{"min": 28081, "max": 28081}]
  },
  "transport": {
    "alpn": "asterferry-data/2",
    "max_streams": 1024,
    "max_frame_bytes": 65536,
    "max_datagram_bytes": 65536,
    "handshake_timeout_seconds": 10,
    "idle_timeout_seconds": 300
  },
  "obfuscation": {
    "mode": "standard",
    "max_padding_bytes": 0,
    "handshake_shaping": false
  },
  "egress": {
    "enabled": false,
    "max_connections": 0
  }
}
```

Notes:

- `public_endpoints` is the AFDP/2 address that the Agent uses to reach B, not a public service port.
- `listeners` is normally empty. After a Service is created, the Controller sends Assignment bindings to the Gateway.
- `port_pool` must cover ports available for automatic service allocation. This example reserves `28080/tcp` and `28081/udp`.
- The current protocol ALPN is `asterferry-data/2`; do not use the AFDP/1 `/1` value.
- If the Dashboard spec editor is prefilled with `asterferry-data/1`, change it to `asterferry-data/2` before saving.

### 4.3 Configure the Agent spec

Open `agent-internal` → **Spec** and save:

```json
{
  "node_id": "agent-internal",
  "gateway_selector": {
    "match_labels": {"site": "public"}
  },
  "proxies": [],
  "routes": [],
  "limits": {
    "max_connections": 4096,
    "max_streams": 1024,
    "max_buffer_bytes": 67108864
  },
  "egress": {
    "enabled": false,
    "max_connections": 0
  },
  "logging": {
    "level": "info",
    "format": "json"
  }
}
```

`gateway_selector.match_labels` must match the Gateway labels, otherwise the Agent has no eligible Gateway.

## 5. Create enrollment tokens on C

An enrollment token is single-use, role-bound, and valid for at most 15 minutes by default. Run these commands on C; the plaintext appears only in the command output:

```sh
asterferry enroll-token create \
  --config ./controller/controller.json \
  --role gateway

asterferry enroll-token create \
  --config ./controller/controller.json \
  --role agent
```

Save the `token:` value from each output separately. Do not use the Gateway token for the Agent. Securely copy the matching token and `controller/ca/ca.crt` to B and A; copy only the CA certificate, never the CA private key.

## 6. Enroll and start the nodes on B and A

### 6.1 B: Gateway

On B, create private directories and place C's `ca.crt` at `./ca/ca.crt`:

```sh
mkdir -p ./ca ./state
./asterferry gateway enroll \
  --controller controller.example.com:9443 \
  --server-name controller.example.com \
  --token '<gateway-token>' \
  --node-id gw-public \
  --ca ./ca/ca.crt \
  --output ./gateway-bootstrap.json \
  --cache ./state/snapshot.cache

./asterferry gateway run --bootstrap ./gateway-bootstrap.json
```

Windows PowerShell example:

```powershell
New-Item -ItemType Directory -Force .\ca, .\state | Out-Null
.\asterferry.exe gateway enroll `
  --controller controller.example.com:9443 `
  --server-name controller.example.com `
  --token '<gateway-token>' `
  --node-id gw-public `
  --ca .\ca\ca.crt `
  --output .\gateway-bootstrap.json `
  --cache .\state\snapshot.cache

.\asterferry.exe gateway run --bootstrap .\gateway-bootstrap.json
```

The Gateway first connects to C for a snapshot, then listens on the UDP endpoint represented by `gateway.example.com:4433` and on the service ports from applied Assignments.

### 6.2 A: Agent

On A, create private directories and place C's `ca.crt` at `./ca/ca.crt`:

```sh
mkdir -p ./ca ./state
./asterferry agent enroll \
  --controller controller.example.com:9443 \
  --server-name controller.example.com \
  --token '<agent-token>' \
  --node-id agent-internal \
  --ca ./ca/ca.crt \
  --output ./agent-bootstrap.json \
  --cache ./state/snapshot.cache

./asterferry agent run --bootstrap ./agent-bootstrap.json
```

The Agent actively connects to C and then actively connects to B's AFDP/2 endpoint when the Controller creates an Assignment. A does not need to expose a management port to the Internet.

## 7. Create the first TCP service

First make sure the real service on A listens at `127.0.0.1:18080`. For a temporary test server:

```sh
python3 -m http.server 18080 --bind 127.0.0.1
```

In the Dashboard, open **Services** → **Create service**:

| Field | Value |
| --- | --- |
| Service ID | `internal-web` |
| Agent | `agent-internal` |
| Protocol | `TCP` |
| Public port | `28080` |
| Local target | `127.0.0.1:18080` |
| Public bind | `0.0.0.0` |
| Gateway selector JSON | `{"match_labels":{"site":"public"}}` |
| Enabled | checked |

After saving, the Controller selects a matching healthy Gateway. If no Assignment appears, open **Assignments** and click **Reschedule** for `agent-internal`.

Setting `public_port` to `0` asks the Gateway port pool to allocate a port automatically. This example uses `28080`, so clients connect to `B:28080`.

Configure a UDP service the same way, changing the protocol to UDP and the port to `28081`, and make sure the UDP target on A is reachable from the Agent process.

## 8. Verify the path

In the Dashboard, check:

1. **Nodes** → Gateway/Agent → **Observed**: the node should be healthy and its applied Generation should be greater than zero.
2. **Assignments**: the Assignment should be `applied`, its endpoint should be `gateway.example.com:4433`, and its bindings should include `28080/tcp`.
3. **Activity**: node, service, scheduling and runtime events should be visible.

From a client that is not A, connect to B:

```sh
curl http://gateway.example.com:28080/
```

If the directory page from A's Python server appears, the complete path is:

```text
client → B:28080 → B Gateway → AFDP/2 UDP 4433 → A Agent → A:18080
```

Useful checks:

```powershell
# Windows client: check Controller TCP ports
Test-NetConnection controller.example.com -Port 8443
Test-NetConnection controller.example.com -Port 9443

# Check the TCP service port on the Gateway
Test-NetConnection gateway.example.com -Port 28080
```

UDP cannot be validated with a TCP probe. Use Dashboard Assignment/Observed state, node logs and firewall packet capture together.

## 9. WSL and Windows host networking

### 9.1 Simplest local development layout

If Controller, Gateway and Agent all run inside the same WSL instance:

- Initialize the Controller with `localhost:8443` and `localhost:9443`.
- Use `127.0.0.1:4433` as the Gateway `public_endpoints` value.
- Windows browsers can usually open `https://localhost:8443/dashboard/` for a service running in WSL.
- A Windows client can use `localhost:28080` when WSL localhost forwarding exposes that TCP port.

If A, B and C are different machines or network namespaces, do not use `127.0.0.1` as a cross-host address. Use a reachable DNS name or IP instead.

### 9.2 WSL reaching a Windows-hosted service

In the default WSL2 NAT mode, Linux processes normally reach a Windows service through the host IP:

```sh
WINDOWS_HOST=$(ip route show | awk '/default/ {print $3; exit}')
curl "http://${WINDOWS_HOST}:18080/"
```

Therefore, if the Agent runs in WSL but the private service runs on Windows, set the Dashboard `Local target` to the reachable host address, such as `${WINDOWS_HOST}:18080`, instead of assuming `127.0.0.1:18080`. The Windows service must listen on an address reachable from WSL, not only on the Windows loopback interface.

Windows 11 22H2 and later can also use WSL mirrored networking so Windows and WSL share network interfaces. With mirrored mode, IPv4 `localhost` can be used according to the local WSL configuration, but Windows Firewall rules still apply.

Enable it in `%USERPROFILE%\.wslconfig`, then restart WSL:

```ini
[wsl2]
networkingMode=mirrored
```

```powershell
wsl --shutdown
```

### 9.3 Exposing WSL TCP services to the LAN

The WSL2 NAT address may change after a restart. To publish TCP ports from the Windows host, run the following from an elevated PowerShell:

```powershell
$WslIp = (wsl.exe hostname -I).Trim().Split()[0]
foreach ($Port in @(8443, 9443, 28080)) {
  netsh interface portproxy add v4tov4 `
    listenaddress=0.0.0.0 `
    listenport=$Port `
    connectaddress=$WslIp `
    connectport=$Port
}

netsh interface portproxy show all
```

Expose only the required ports and restrict their source networks with Windows Firewall. Update the rules when the WSL IP changes. To remove them:

```powershell
netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=8443
netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=9443
netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=28080
```

`netsh interface portproxy` is TCP forwarding. It cannot forward the Gateway's `4433/udp` or UDP service ports. To expose a Gateway UDP endpoint from WSL, choose one of these approaches:

1. Use mirrored networking on Windows 11 22H2+ and allow the UDP ports in Windows Firewall.
2. Use a dedicated NAT/forwarder that supports UDP.
3. Run the Gateway on the real public host B, and keep WSL for development or the Agent.

See [Microsoft's WSL networking documentation](https://learn.microsoft.com/en-us/windows/wsl/networking) for WSL networking modes, localhost forwarding and `netsh portproxy`.

## 10. Troubleshooting

### The Dashboard does not open

Check C's `8443/tcp`, verify that the Controller is running, and make sure the admin client trusts `controller/ca/ca.crt`. The Dashboard URL is `/dashboard/`, not `/`.

### Enrollment reports a TLS or certificate-name error

Verify that:

- enrollment uses C's CA certificate, not a node certificate from A or B;
- `--server-name` matches the Controller certificate SAN;
- `controller init` used a stable C hostname or IP in `--http-listen` and `--grpc-listen`, rather than generating a certificate containing only `localhost`;
- A and B reach `C_HOST:9443` and TCP 9443 is allowed.

Use `--insecure` only for a temporary local experiment, never for production enrollment.

### The Assignment remains pending

Check that the Gateway is registered, its certificate is active, the node is enabled, the Gateway labels match the Agent/Service selectors, and the Gateway port pool has an available port. You can also use **Assignments** → **Reschedule**.

### The Gateway port is not listening

The Gateway creates service listeners only after it receives and applies a valid snapshot and the Assignment becomes `applied`. Check Observed and Assignments first, then inspect B's UDP `4433` and service-port firewall rules.

### Requests to B:28080 fail

Confirm that `127.0.0.1:18080` is reachable from the Agent process on A. `local_target` is evaluated from the Agent's point of view, not from the Controller or client. Then check the UDP 4433 path from A to B.

### A configuration change is not visible

Nodes receive snapshots with a generation and checksum over the mTLS gRPC control channel. Check the Controller and node logs and the applied Generation under Observed. Do not edit the database or bootstrap JSON directly.

## 11. Backup, upgrade and shutdown

Back up the complete Controller installation before upgrades:

```sh
asterferry controller backup \
  --config ./controller/controller.json \
  --output ./backups
```

For a v3/v4 database upgrade, stop the Controller, run a dry-run, then publish the migration:

```sh
asterferry controller migrate --config ./controller/controller.json --dry-run
asterferry controller migrate --config ./controller/controller.json
```

When the Controller is temporarily unavailable, nodes continue using their encrypted last-known-good snapshot. New configuration, scheduling and certificate operations require the Controller to recover. Do not delete `controller/`; it contains the database, CA, TLS identity and master key.

See [`deploy/README.md`](../deploy/README.md) for systemd, Docker Compose and Kubernetes deployment details.

## 12. Completion checklist

- [ ] C's `8443/tcp` and `9443/tcp` are listening and reachable.
- [ ] The Dashboard accepts login and both nodes are registered.
- [ ] The Gateway `public_endpoints` value is a reachable B address on UDP `4433`.
- [ ] The Agent selector matches the Gateway labels.
- [ ] A and B completed enrollment with role-matching one-time tokens.
- [ ] Node certificates are active, Observed is healthy, and the Assignment is applied.
- [ ] The Service `local_target` is reachable from the Agent process on A.
- [ ] A client can reach the public service port on B.
- [ ] Bootstrap files, CA material and the Controller data directory are protected and backed up.
