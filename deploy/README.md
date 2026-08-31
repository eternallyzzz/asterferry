# AsterFerry deployment

AsterFerry is deployed as one Controller and independently managed Gateway and
Agent data-plane nodes. The Controller owns SQLite state, PKI, scheduling, RBAC
and audit. Nodes receive typed snapshots and never expose a management HTTP API
or read business YAML.

## Controller

```sh
asterferry controller init --dir /var/lib/asterferry
asterferry controller run --config /var/lib/asterferry/controller.json
```

The generated CA, Controller certificate, master key, database and first Admin
account are owner-readable. Back up the complete installation before changes:

```sh
asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

For an existing schema v3 or v4 database, stop the Controller and run the
explicit v5 migration during a maintenance window. Validate first; the publish
step retains the original file as a rollback backup:

```sh
asterferry controller migrate --config /var/lib/asterferry/controller.json --dry-run
asterferry controller migrate --config /var/lib/asterferry/controller.json
```

`OpenStore` does not perform implicit schema rewrites. Do not start the
Controller until the migration command has completed successfully.

`deploy/asterferry-controller.service` is a single-replica systemd unit; the
Controller is not advertised as highly available. Nodes retain their encrypted
last-known-good snapshot while it is unavailable.

## Node enrollment

Register a Node (role and labels) through the Controller API or Dashboard,
create a role-bound enrollment token, and use it once to create bootstrap
material:

```sh
asterferry enroll-token create --config /var/lib/asterferry/controller.json --role gateway
asterferry gateway enroll --controller controller.example:9443 \
  --token <one-time-token> --node-id gw-east \
  --ca /var/lib/asterferry/ca/ca.crt \
  --output /var/lib/asterferry/gateway-bootstrap.json
asterferry gateway run --bootstrap /var/lib/asterferry/gateway-bootstrap.json
```

Repeat with `agent enroll` and `agent run` for Agents. A bootstrap file holds
only Controller address, node identity, certificate/key, CA and cache/logging
settings. Protect it as a secret. Certificate renewal is automatic seven days
before expiry and is persisted atomically by the node runtime.

## Linux service units

Install the binary and create a dedicated `asterferry` account, then place each
node's bootstrap file and writable state directory under its service account:

```sh
install -m 0644 deploy/asterferry-gateway.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now asterferry-gateway
```

Use `asterferry-agent.service` on an Agent host and
`asterferry-controller.service` for the Controller. Nodes need only their
AFDP/2 QUIC endpoint (Gateway) and outbound Controller/data connectivity; all
business changes go through the Controller API.

## Docker Compose and Helm

`deploy/docker/compose.yaml` runs the Controller, Gateway and Agent as separate
services. The Controller directory is writable; node bootstrap/state mounts are
isolated from it. Bootstrap mounts are writable because certificate rotation
atomically replaces the file. `deploy/helm/asterferry-controller` creates a
single-replica StatefulSet with a PVC. `deploy/helm/asterferry-node` copies the
Secret-provided enrollment seed into its state PVC with an init container so
rotation never mutates the Kubernetes Secret.

Each Gateway must have its own reachable public endpoint. Shared VIP takeover,
transparent connection migration and Controller HA are outside the first
release.

AFDP/1 to AFDP/2 is a deliberate hard wire break: QUIC ALPN and the protocol
version byte both changed, and there is no fallback. Roll out Gateway and
Agent binaries in coordination with the Controller-side release.

## Operations and verification

The REST API is rooted at `/api/v1`; `/healthz` is anonymous while readiness,
metrics and resource operations require a Viewer, Operator or Admin role.
Mutating requests use `If-Match` revisions and may include an `Idempotency-Key`.
Use the Dashboard only as a Controller client.

```sh
go test ./...
go vet ./...
npm --prefix web/dashboard test -- --run
npm --prefix web/dashboard run build
helm lint deploy/helm/asterferry-controller deploy/helm/asterferry-node
docker compose -f deploy/docker/compose.yaml config
```
