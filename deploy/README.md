# AsterFerry deployment

AsterFerry is deployed as one Controller and independently managed data-plane
Nodes. The Controller owns SQLite state, PKI, scheduling, RBAC and audit. A
Node is a registered identity; its Gateway or Agent behavior is a separate
specification selected in the Dashboard. Nodes receive typed snapshots and
never expose a management HTTP API or read business YAML.

For a copy-and-run three-server walkthrough in both languages, see the
[English end-to-end quick start](../docs/quickstart.en.md) and the
[中文端到端快速开始](../docs/quickstart.zh-CN.md).

## Controller

```sh
asterferry controller init --dir /var/lib/asterferry \
  --grpc-advertise controller.example.com:9443 \
  --release-version 1.0.0
asterferry controller run --config /var/lib/asterferry/controller.json
```

The generated CA, Controller certificate, master key, database and first Admin
account are owner-readable. Back up the complete installation before changes:

```sh
asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

This pre-1.0 generation uses SQLite schema v9. A v8 database receives the
additive runtime-observability migration on startup; older or unknown
generations remain incompatible. Export any needed resources, retain a
complete backup, and upgrade during a maintenance window.

`deploy/asterferry-controller.service` is a single-replica systemd unit; the
Controller is not advertised as highly available. Nodes retain their encrypted
last-known-good snapshot while it is unavailable.

## Node enrollment

The recommended path is Dashboard → **Nodes** → **Generate install command**.
Choose only the target platform; this creates a pending generic installation
intent, not an enrolled node. The Controller returns one short-lived,
node-bound installer command; run it once on B or C as root/Administrator. The
installer downloads and verifies the matching release, reaches the Controller
to enroll the Node, creates the system service and starts it. The identity is
not shown in the enrolled-node list until that enrollment succeeds. After the
Node is online, open its details, select Gateway or Agent behavior, and save
the spec. Configure the Gateway endpoint and port pools there. Once the
Gateway and Agent are configured, create services in the Dashboard; resource
changes trigger scheduling automatically.

For offline images or custom service accounts, create the generic Node identity
first, then use a node-bound token in the manual path below:

```sh
asterferry enroll-token create --config /var/lib/asterferry/controller.json --node-id gw-east
asterferry node enroll --controller controller.example:9443 \
  --token <one-time-token> --node-id gw-east \
  --ca /var/lib/asterferry/ca/ca.crt \
  --output /var/lib/asterferry/node-bootstrap.json
asterferry node run --bootstrap /var/lib/asterferry/node-bootstrap.json
```

A bootstrap file holds only Controller address, node identity, certificate/key,
CA and cache/logging settings. Protect it as a secret. Certificate renewal is
automatic seven days before expiry and is persisted atomically by the node
runtime.

## Linux service units

Install the binary and create a dedicated `asterferry` account, then place the
Node bootstrap file and writable state directory under its service account:

```sh
install -m 0644 deploy/asterferry-node.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now asterferry-node
```

Use `deploy/asterferry-node.service` for the generic Node. Nodes need outbound
Controller connectivity; a Node configured as Gateway additionally needs its AFDP/2 QUIC
endpoint reachable by Agents. All business changes go through the Controller
API.

## Docker Compose and Helm

`deploy/docker/compose.yaml` runs the Controller and two optional Node daemon
slots. Both slots use the same generic Node binary; Gateway or Agent behavior is
selected later by the Controller Node Spec. The
Controller directory is writable; node bootstrap/state mounts are isolated from
it. Bootstrap mounts are writable because certificate rotation atomically
replaces the file. `deploy/helm/asterferry-controller` creates a single-replica
StatefulSet with a PVC. `deploy/helm/asterferry-node` copies the Secret-provided
enrollment seed into its state PVC with an init container so rotation never
mutates the Kubernetes Secret.

Each Gateway must have its own reachable public endpoint. Shared VIP takeover,
transparent connection migration and Controller HA are outside the first
release.

AFDP/1 to AFDP/2 is a deliberate hard wire break: QUIC ALPN and the protocol
version byte both changed, and there is no fallback. Roll out all Node binaries
in coordination with the Controller-side release.

## Operations and verification

The REST API is rooted at `/api/v1`; `/healthz` is anonymous while readiness,
metrics and resource operations require a Viewer, Operator or Admin role.
Mutating requests use `If-Match` revisions and may include an `Idempotency-Key`.
Use the Dashboard only as a Controller client.

Runtime connection metadata is read-only by default and never includes payloads.
Admin can enable advanced runtime operations in Dashboard → Admin. Operators can
then disconnect or temporarily rate-limit active Node connections; controls are
ephemeral, audited and bounded by TTL. See the bilingual
[runtime operations guide](../docs/operations.en.md) and
[中文运维指南](../docs/operations.zh-CN.md).

```sh
go test ./...
go vet ./...
npm --prefix web/dashboard test -- --run
npm --prefix web/dashboard run build
helm lint deploy/helm/asterferry-controller deploy/helm/asterferry-node
docker compose -f deploy/docker/compose.yaml config
```
