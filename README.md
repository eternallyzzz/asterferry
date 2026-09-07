# AsterFerry

AsterFerry is a self-hosted private-network forwarding system. A Controller
handles identity, access control, configuration, scheduling and audit. Each
data-plane host runs one generic Node; the Dashboard assigns it a Gateway or
Agent behavior after enrollment.

```text
Dashboard / CLI -- HTTPS --> Controller -- mTLS gRPC --> Node
                                      |
                                      +-- SQLite or PostgreSQL

Gateway <========== AFDP/2 over QUIC ==========> Agent
```

## 3-minute Linux quickstart

You need a Linux Controller host and one or more Linux Node hosts. The
Controller installer creates the service, a local CA and the first admin
account. Replace `<CONTROLLER_IP>` with an address reachable by the Nodes.

Run this as root. If you are not root, replace the final `bash` with
`sudo bash`.

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | bash -s -- --grpc-advertise <CONTROLLER_IP>:9443
```

Open `https://<CONTROLLER_IP>:8443/dashboard/` and save the one-time admin
password printed by the installer. In **Nodes**, create an installation task
for each host and run the generated command on that host. The command carries
the one-time enrollment token and the Controller CA; do not edit its Node ID.

After enrollment, select one Node as a Gateway and configure its public
endpoint. Select another as an Agent and bind it to that Gateway. Create a
Service, then verify traffic through the assigned public endpoint.

The default ports are:

| Port | Purpose |
| --- | --- |
| TCP 8443 | HTTPS API and Dashboard |
| TCP 9443 | Node mTLS control connection |
| UDP 4433 | Default AFDP/2 Gateway data port |
| TCP 9090 | Optional loopback Prometheus listener |

Open only the ports required by your topology. Keep the Controller CA private
key, master key, database and backups protected. Native self-update verifies a
Cosign-signed release manifest and its SHA-256 asset digests before replacing
an executable; container and Helm deployments roll out a new image instead.

## Install from a release

The command above uses the source installer entry point and resolves the
latest stable release. For a pinned release, download the matching
`install-controller.sh` or `install-controller.ps1` asset. Windows users can
run the PowerShell installer from an elevated PowerShell session. The
Dashboard-generated Node command supports Linux, Windows, amd64 and arm64
where the selected release publishes that asset.

## Build from source

The release toolchain is recorded in [`.toolchain.json`](.toolchain.json).
Node.js is needed only to build the Dashboard; the production binary does not
need Node.js.

```bash
npm --prefix web/dashboard ci --audit=false
npm --prefix web/dashboard run build
CGO_ENABLED=0 go build -tags=dashboard_assets -trimpath \
  -ldflags="-s -w" -o dist/asterferry ./cmd/asterferry
```

For source-tree deployment, initialize the Controller with
`asterferry controller init`, copy the same Node binary to each Node host, and
use a Dashboard installation task to enroll them. The full native, Windows,
WSL and PostgreSQL procedures are in
[`docs/deployment.en.md`](docs/deployment.en.md).

## Operations and development

- [Deployment guide](docs/deployment.en.md) — native, Windows, WSL,
  PostgreSQL, backup and upgrade procedures.
- [Runtime operations](docs/operations.en.md) — API endpoints, observability
  and advanced controls.
- [中文运维指南](docs/operations.zh-CN.md) — Chinese localized operations
  guide.
- [Architecture contract](docs/architecture.md) and
  [implementation map](docs/architecture-internals.md).
- [Compatibility and support matrix](docs/compatibility.md) and
  [support matrix](docs/support-matrix.md).
- [Release runbook](docs/release-runbook.md) and [security policy](SECURITY.md).

Before contributing, run the focused Go tests, Dashboard checks and release
metadata checks described in [`CONTRIBUTING.md`](CONTRIBUTING.md).
