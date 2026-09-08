# AsterFerry

[中文说明](docs/README.zh-CN.md)

AsterFerry is a self-hosted private-network forwarding system. A Controller
manages identity, access control, configuration and scheduling. Each data-plane
host runs one generic Node; the Controller assigns Gateway or Agent behavior
after enrollment.

```text
Dashboard / CLI -- HTTPS --> Controller -- mTLS gRPC --> Node
                                      |
                                      +-- SQLite or PostgreSQL

Gateway <========== AFDP/1 over QUIC ==========> Agent
```

Wire protocols are AFDP/1 and control/1. The database schema is v1.

## 3-minute Linux quickstart

Requirements: a Linux Controller host, a reachable Controller address, and one
or more Linux or Windows Node hosts. The installer creates the service,
certificates, database and first Admin account.

Set `<CONTROLLER_IP>` to an address reachable by every Node. Run the installer
as root. For a non-root shell, replace the final `bash` with `sudo bash`.

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | bash -s -- --grpc-advertise <CONTROLLER_IP>:9443
```

1. Open `https://<CONTROLLER_IP>:8443/dashboard/` and save the one-time Admin
   password printed by the installer.
2. In **Nodes**, create an installation task and run the generated command on
   the target host. Run Linux commands as root or with `sudo`; run Windows
   commands in an elevated PowerShell. Do not edit the generated Node ID or
   enrollment token.
3. In the Node details, save a **Gateway** specification on one Node and an
   **Agent** specification on another. Bind the Agent to that Gateway.
4. Create a **Service**, configure its public endpoint and local target, then
   verify traffic through the Gateway endpoint.

For reproducible deployments, use the matching installer asset from a tagged
GitHub Release and pin its version.

## Network and deployment

Open only the ports required by the topology and restrict them with the host
firewall or network policy.

| Port | Direction | Purpose |
| --- | --- | --- |
| TCP 8443 | clients → Controller | HTTPS API and Dashboard |
| TCP 9443 | Nodes → Controller | mTLS control connection |
| UDP 4433 | clients ↔ Gateway | Default AFDP/1 data port |
| TCP 9090 | Prometheus → Controller | Optional, loopback-only metrics listener |

The Controller may listen on `0.0.0.0`, but `--grpc-advertise` must be a real
address reachable by Nodes; never advertise `0.0.0.0`.

### Windows and WSL

Download `install-controller.ps1` from the selected release, then run it in an
elevated PowerShell:

```powershell
.\install-controller.ps1 -GrpcAdvertise <CONTROLLER_IP>:9443
```

The Dashboard-generated Node command supports the published Linux and Windows
Node assets. WSL2 supports systemd and a no-systemd fallback. It is
compatibility-tested, not a formal support target.

### Containers and Helm

Container and Helm deployments use operator-built images. Initialize the
Controller data directory and set the image in
[`deploy/docker/compose.yaml`](deploy/docker/compose.yaml) or the
`image.repository` value in the
[`Controller chart`](deploy/helm/asterferry-controller) and
[`Node chart`](deploy/helm/asterferry-node). Container and Helm deployments
replace the image during an upgrade; they do not replace a binary inside a
running container.

SQLite is for one Controller replica. PostgreSQL supports an active/standby
pair with an external readiness-aware load balancer or Kubernetes Service.
Both replicas must share the Controller identity, master key and database.

## Routine operations

### Health and logs

For a native Linux service:

```bash
sudo systemctl status asterferry-controller.service
sudo journalctl -u asterferry-controller.service -f
curl --fail --insecure https://127.0.0.1:8443/healthz
curl --fail --insecure https://127.0.0.1:8443/readyz
```

`/healthz` checks the process. `/readyz` is the routing readiness signal.
Management `/metrics` requires authentication. The separate metrics listener
is loopback-only by default and should only be exposed to a trusted Prometheus
network.

### Backup and restore

Back up the database, Controller configuration, CA, TLS identity and master
key together. For a native installation:

```bash
sudo -u asterferry /var/lib/asterferry/bin/asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

Verify the backup in a disposable restore directory. PostgreSQL backups use
`pg_dump` and `pg_restore`. Restoring a backup invalidates browser sessions and
resets the Controller lease.

### Upgrades and retirement

- Back up and verify the Controller before upgrading.
- Keep Controller and Nodes on the same release line when the wire or database
  contract changes.
- Native self-update uses a signed release manifest, checks the archive digest,
  and rolls back after a failed readiness check.
- Container and Helm installations require a new approved image rollout.
- Use **Retire** for a Node that should no longer authenticate. Services and
  audit history remain; permanent deletion requires the retired Node to have
  no remaining dependencies.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Dashboard does not open | Check the Controller service, TCP 8443 and the HTTPS listen address. |
| Node remains pending | Check that `--grpc-advertise` resolves and TCP 9443 is reachable from the Node; enrollment tokens expire and are single-use. |
| Node enrolled but carries no traffic | Save a Gateway/Agent specification, bind the Agent to a Gateway, create a Service and check the Gateway firewall/public endpoint. |
| Traffic is rejected | Check the Service target, protocol/port, assignment and the Node **Observed** view. |
| Root install says `sudo` is missing | Run the final installer command directly as root; `sudo` is only for a non-root shell. |

## Security essentials

- Treat the initial Admin password, enrollment token and generated bootstrap
  file as secrets. Do not paste them into issues, chat or shell history.
- Never copy or expose the Controller CA private key. Protect the Controller
  data directory, database, TLS identity and master key with the same care.
- Use HTTPS release URLs and do not modify Dashboard-generated enrollment
  commands. Keep the required Controller and Node ports behind a firewall.
- The native updater uses certificate verification bypass only for a localhost
  readiness probe; release manifests are signature-verified and downloaded
  assets are checked against their signed SHA-256 digests.
- Do not expose the optional metrics listener publicly. Use authenticated
  management metrics or a trusted internal scrape network.

## Support and interfaces

| Surface | Status |
| --- | --- |
| Native Controller/Node | Linux amd64/arm64 and Windows amd64 |
| WSL2 | Compatibility-tested; not a formal support target |
| Database | SQLite for one replica; PostgreSQL for production-scale state and two-replica active/standby |
| Container/Helm | Supported with an operator-built image and source charts |
| GeoIP routing | Optional external, reviewed MaxMind-compatible database; not bundled |

The Controller serves OpenAPI at `/openapi.yaml` and `/api/v1/openapi.yaml`.
The source is [`internal/controller/openapi.yaml`](internal/controller/openapi.yaml);
[`api/openapi.yaml`](api/openapi.yaml) is its generated copy. Wire schemas are
[`control.proto`](proto/controlwire/v1/control.proto) and
[`data.proto`](proto/afdp/v1/data.proto).
