# Deployment guide

This guide contains the longer procedures behind the short
[`README.md`](../README.md) quickstart. AsterFerry has one Controller and one
generic Node process per data-plane host. The Controller publishes a typed
Node Spec that selects Gateway or Agent behavior after enrollment.

## Prerequisites and network layout

Use a supported native Linux or Windows host for the Controller and Nodes.
Container and Helm deployments are operator-built image deployments; the
project does not publish an image registry artifact. PostgreSQL is required
for an active/standby Controller pair; SQLite is the default single-replica
store.

| Port | Direction | Purpose |
| --- | --- | --- |
| TCP 8443 | clients → Controller | HTTPS API and Dashboard |
| TCP 9443 | Nodes → Controller | mTLS control connection |
| UDP 4433 | clients ↔ Gateway | Default AFDP/2 data port |
| TCP 9090 | Prometheus → Controller | Optional loopback metrics listener |

The Controller may listen on `0.0.0.0`, but `--grpc-advertise` must be an
address that Nodes can actually reach. Do not advertise `0.0.0.0`. Restrict
the HTTPS, gRPC and metrics listeners with the host firewall and cloud
security groups.

## Native Linux Controller

### Release installer

The source installer resolves the latest stable release and verifies the
Controller archive before installing it. Use a pinned release asset when
reproducibility is required.

Run this as root. If you are not root, replace the final `bash` with
`sudo bash`.

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | bash -s -- --grpc-advertise <CONTROLLER_IP>:9443
```

The installer creates `asterferry-controller.service`, initializes the data
directory and prints the first admin password once. It also stores the Node
installer scripts and release metadata used by Dashboard-generated install
commands. It does not pre-download Node binaries.

### Source deployment

Build the Dashboard and binary as described in the README, then create a
dedicated service account and data directory:

```bash
sudo useradd --system --home-dir /var/lib/asterferry \
  --shell /usr/sbin/nologin asterferry 2>/dev/null || true
sudo install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
sudo install -d -o asterferry -g asterferry -m 0750 /var/lib/asterferry/bin
sudo install -o asterferry -g asterferry -m 0755 \
  dist/asterferry /var/lib/asterferry/bin/asterferry

sudo -u asterferry /var/lib/asterferry/bin/asterferry controller init \
  --dir /var/lib/asterferry \
  --http-listen 0.0.0.0:8443 \
  --grpc-listen 0.0.0.0:9443 \
  --grpc-advertise <CONTROLLER_IP>:9443

sudo install -m 0644 deploy/asterferry-controller.service \
  /etc/systemd/system/asterferry-controller.service
sudo systemctl daemon-reload
sudo systemctl enable --now asterferry-controller.service
```

The initialization output contains the one-time admin password. Store the
Controller CA certificate where Node hosts can read it, but never copy the CA
private key. Keep the database, CA, TLS identity and master key together in
backups.

## Native Node enrollment

In Dashboard → **Nodes**, create an installation task and select platform,
architecture and installer source. `Controller` is the preferred source for
an internal deployment. The generated command is the only command needed on
the Node host; it supplies the immutable Node ID, one-time token, Controller
address and CA. Run the Linux command as root or through `sudo` and run the
Windows command in an elevated PowerShell session.

For a source-tree build that has not been published, copy the same Node binary
to each host and run the equivalent enrollment command:

```bash
sudo useradd --system --home-dir /var/lib/asterferry \
  --shell /usr/sbin/nologin asterferry 2>/dev/null || true
sudo install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
sudo install -d -o asterferry -g asterferry -m 0750 /var/lib/asterferry/bin
sudo install -o asterferry -g asterferry -m 0755 \
  dist/asterferry-linux-amd64 /var/lib/asterferry/bin/asterferry
sudo install -o asterferry -g asterferry -m 0644 controller-ca.crt \
  /var/lib/asterferry/controller-ca.crt

sudo -u asterferry /var/lib/asterferry/bin/asterferry node enroll \
  --controller <CONTROLLER_IP>:9443 \
  --token '<ENROLLMENT_TOKEN>' \
  --node-id '<CONTROLLER_GENERATED_NODE_ID>' \
  --ca /var/lib/asterferry/controller-ca.crt \
  --output /var/lib/asterferry/node-bootstrap.json \
  --cache /var/lib/asterferry/snapshot.cache

sudo install -m 0644 deploy/asterferry-node.service \
  /etc/systemd/system/asterferry-node.service
sudo systemctl daemon-reload
sudo systemctl enable --now asterferry-node.service
```

Enrollment creates a generic identity only. Assign the Gateway or Agent role
in Dashboard, configure the Gateway public endpoint and bind each Agent to a
registered Gateway. Role configuration is not part of the installer command.

## Windows and WSL2

Download the release `install-controller.ps1` or `install-node.ps1` asset with
`curl.exe`, then run it from an elevated PowerShell session. Windows services
are named `AsterFerry-Controller` and `AsterFerry-Node` by default. Windows
Node upgrades are coordinated by the Controller and use the same signed
release metadata as Linux Nodes.

WSL2 is compatibility-tested rather than an official support target. With
systemd enabled, the normal services are used. Without systemd, the installers
use an `asterferry`-owned background process, PID and log files, and a WSL
startup hook. Inspect those processes with the installer-provided status
commands and the `controller.log` / `node.log` files. If switching between
systemd and the fallback, run `wsl --shutdown` once after changing the WSL
configuration.

## PostgreSQL and Controller HA

Use PostgreSQL for production-scale state and exactly two active/standby
Controller replicas. Both replicas need the same Controller identity/config
Secret, database access and master key. Put an external readiness-aware load
balancer or Kubernetes Service in front of them; route client traffic only to
the ready leader. SQLite is intentionally single-replica.

The Controller uses a singleton lease and fencing epoch. Losing leadership
drains active Node streams and rejects writes before they can commit. Test
leader loss, standby readiness, Node reconnect and browser-session persistence
before calling an HA deployment ready. The availability objective and restore
procedure are documented in [`compatibility.md`](compatibility.md).

## Health, logs and backups

```bash
sudo systemctl status asterferry-controller.service
sudo systemctl status asterferry-node.service
sudo journalctl -u asterferry-controller.service -f
sudo journalctl -u asterferry-node.service -f
curl --fail --insecure https://<CONTROLLER_IP>:8443/healthz
```

`/readyz` is the boolean readiness probe used by external routing. The
management `/metrics` endpoint is authenticated; the native metrics listener
is separate and loopback-only by default.

Back up the database, CA, TLS identity and master key as one recoverable set:

```bash
sudo -u asterferry /var/lib/asterferry/bin/asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

For PostgreSQL, the backup and restore commands use the external
`pg_dump`/`pg_restore` utilities. Verify every backup in a disposable restore
directory before relying on it. Restore invalidates browser sessions and
resets the Controller lease.

## Upgrades and retirement

Controller and Node binaries from the same release line should be rolled out
as one unit when wire or database contracts change. Native self-update is
limited to stable releases with a Cosign-signed `release-manifest.json`; the
manifest and `SHA256SUMS` must agree before an archive is staged. Readiness
failure triggers rollback or a recoverable manual-required state. Container
and Helm deployments require an image rollout and never replace a binary
inside a running container.

Before an upgrade, complete and verify a backup. Keep the previous binary and
release artifacts until the new Controller is ready and a Node smoke test
passes. A Node installed before the self-update capability needs one manual
run of the current installer.

Use **Retire** for a Node that should no longer authenticate; its certificate
is revoked while services, specifications and audit history remain. Permanent
deletion is allowed only after retirement and dependency checks pass. It does
not cascade-delete business Services.

For runtime API details, observability and advanced controls, see
[`operations.en.md`](operations.en.md). For release integrity and key
rotation, see [`SECURITY.md`](../SECURITY.md) and the
[`release-runbook.md`](release-runbook.md).
