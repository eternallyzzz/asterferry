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

The host portions of `--http-listen`, `--grpc-listen` and `--grpc-advertise` should be stable C names or IPs. The generated Controller certificate includes these addresses in its SAN. Do not replace the stable name with `0.0.0.0` in this remote deployment example.

```powershell
# Windows; replace the example host with C's real DNS name or fixed IP
asterferry.exe controller init `
  --dir .\controller `
  --username admin `
  --password-file .\admin-password.txt `
  --http-listen controller.example.com:8443 `
  --grpc-listen controller.example.com:9443 `
  --grpc-advertise controller.example.com:9443 `
  --release-version 1.0.0
```

```sh
# Linux / WSL
./asterferry controller init \
  --dir ./controller \
  --username admin \
  --password-file ./admin-password.txt \
  --http-listen controller.example.com:8443 \
  --grpc-listen controller.example.com:9443 \
  --grpc-advertise controller.example.com:9443 \
  --release-version 1.0.0
```

`--grpc-advertise` is the address that A and B use to reach C. Replace
`--release-version` with a version already published under `release_base_url`.
Initialization includes the advertised address in the Controller certificate
SAN. A private release mirror can be selected with `--release-base-url`, but
it must use HTTPS.

If an existing Controller was initialized without an advertised address, stop
it and repair the configuration without replacing its database, CA, master key
or Admin account:

```powershell
asterferry.exe controller configure `
  --config .\controller\controller.json `
  --grpc-advertise controller.example.com:9443
```

If the old configuration also has no published `release_version`, add
`--release-version 1.0.0`; add `--release-base-url` when using a private HTTPS
release mirror.

The command reissues the Controller certificate with the new address in its
SAN. Restart the Controller before generating Node installation commands. For
local Windows-to-WSL testing, `172.28.80.1:9443` is commonly reachable from
WSL; remote Nodes must use C's stable LAN or DNS address instead.

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

## 4. Generate one-click installation commands from the Dashboard

Open `https://controller.example.com:8443/dashboard/` and go to **Nodes**.
The Controller remains the only owner of business configuration; A and B do
not read business YAML.

### 4.1 Create Node installation tasks

Click **Generate install command**, enter the Node ID, name and labels, then
select the target platform and architecture. Do not choose Gateway or Agent and
do not enter an AFDP endpoint here. This creates a pending installation intent;
it does not create an enrolled node yet.

Generate one command for B and one for A. Copy each command to its target host
and run it in a root shell on Linux or an elevated PowerShell on Windows. The
target host executes the command, reaches the Controller and completes Enroll;
only then does the Node appear in the enrolled-node list.

### 4.2 Choose behavior in Node details

After a Node is online, open **Node details** → **Spec**, choose a behavior and
save it:

- On B choose `Gateway` and configure `public_endpoints`, listeners and TCP/UDP
  port pools. `public_endpoints` is the data-plane address A uses to reach B,
  not the Controller address.
- On A choose `Agent` and configure `gateway_selector`, proxy entrances and
  routes.

The Controller sends a data-plane snapshot only after a behavior spec is saved.
Deleting the spec returns the Node to an unconfigured state; delete the current
spec before switching behavior.

The installer downloads the exact release configured by the Controller,
verifies its SHA-256 checksum, writes the CA and bootstrap/cache files into a
private state directory, uses the node-bound enrollment token to obtain the
certificate, and creates, enables and starts the system service:

- Linux: `asterferry-Node.service`.
- Windows: `AsterFerry-Node` Windows service.

The token is single-use and valid for 15 minutes. It can only enroll its own
Node ID. Do not paste the command into public chats, tickets or logs.
If it expires or is lost, reissue it from the Dashboard's pending installation
list. B and A do not need a manually copied CA, edited bootstrap JSON or second
start command.

### 4.3 Legacy/manual enrollment

The `enroll-token create`, `node enroll`, `node run` and system-service
templates remain available for offline images, private release mirrors and
custom service accounts. The legacy `gateway`/`agent` commands remain compatible
with existing role-bound bootstrap files.

## 5. Create the first TCP service

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

After saving, the Controller automatically selects a matching healthy Gateway and creates the Assignment; normally no manual reschedule is needed. If a Gateway is not online yet or its port pool is temporarily full, the service remains pending and the Controller retries when resources recover.

Setting `public_port` to `0` asks the Gateway port pool to allocate a port automatically. This example uses `28080`, so clients connect to `B:28080`.

Configure a UDP service the same way, changing the protocol to UDP and the port to `28081`, and make sure the UDP target on A is reachable from the Agent process.

## 6. Verify the path

In the Dashboard, check:

1. **Nodes** → the corresponding Node → **Observed**: the node should be healthy and its applied Generation should be greater than zero; the behavior spec should show Gateway or Agent.
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

## 7. WSL and Windows host networking

### 7.1 Simplest local development layout

If Controller, Gateway and Agent all run inside the same WSL instance:

- Initialize the Controller with `localhost:8443` and `localhost:9443`.
- Use `127.0.0.1:4433` as the Gateway `public_endpoints` value.
- Windows browsers can usually open `https://localhost:8443/dashboard/` for a service running in WSL.
- A Windows client can use `localhost:28080` when WSL localhost forwarding exposes that TCP port.

If A, B and C are different machines or network namespaces, do not use `127.0.0.1` as a cross-host address. Use a reachable DNS name or IP instead.

### 7.2 WSL reaching a Windows-hosted service

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

### 7.3 Exposing WSL TCP services to the LAN

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

## 8. Troubleshooting

### The Dashboard does not open

Check C's `8443/tcp`, verify that the Controller is running, and make sure the admin client trusts `controller/ca/ca.crt`. The Dashboard URL is `/dashboard/`, not `/`.

### Enrollment reports a TLS or certificate-name error

Verify that:

- enrollment uses C's CA certificate, not a node certificate from A or B;
- `--server-name` matches the Controller certificate SAN;
- `controller init` used stable C hostnames or IPs in `--http-listen`, `--grpc-listen` and `--grpc-advertise`; the advertised address is also included in the certificate SAN;
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

## 9. Backup, upgrade and shutdown

Back up the complete Controller installation before upgrades:

```sh
asterferry controller backup \
  --config ./controller/controller.json \
  --output ./backups
```

For a v3/v4/v5/v6 database upgrade, stop the Controller, run a dry-run, then publish the migration to the current schema v7:

```sh
asterferry controller migrate --config ./controller/controller.json --dry-run
asterferry controller migrate --config ./controller/controller.json
```

When the Controller is temporarily unavailable, nodes continue using their encrypted last-known-good snapshot. New configuration, scheduling and certificate operations require the Controller to recover. Do not delete `controller/`; it contains the database, CA, TLS identity and master key.

See [`deploy/README.md`](../deploy/README.md) for systemd, Docker Compose and Kubernetes deployment details.

## 10. Completion checklist

- [ ] C's `8443/tcp` and `9443/tcp` are listening and reachable.
- [ ] The Dashboard accepts login and both nodes have completed installation and enrollment.
- [ ] The Gateway `public_endpoints` value is a reachable B address on UDP `4433`.
- [ ] The Agent selector matches the Gateway labels.
- [ ] A and B completed Node enrollment with one-time tokens and each has the intended behavior spec.
- [ ] Node certificates are active, Observed is healthy, and the Assignment is applied.
- [ ] The Service `local_target` is reachable from the Agent process on A.
- [ ] A client can reach the public service port on B.
- [ ] Bootstrap files, CA material and the Controller data directory are protected and backed up.
