# AsterFerry deployment

AsterFerry is deployed as one logical Controller (single process or an
active/standby pair) and independently managed data-plane Nodes. The
Controller owns SQLite (default) or PostgreSQL state, PKI, scheduling, RBAC and audit. A
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

SQLite database schema v12 remains the default. For a production-sized or
active/standby Controller, use
an external PostgreSQL database:

```sh
asterferry controller init --dir /var/lib/asterferry \
  --grpc-advertise controller.example.com:9443 \
  --database-driver postgres \
  --database-url 'postgres://asterferry:<password>@postgres.example.com/asterferry?sslmode=require'
```

The development schema is a clean break: there is no in-place migration and no
`controller migrate` command. To change backend, initialize a new Controller
and recreate resources in the Dashboard. Older or unknown database generations
and pre-v12 backup manifests remain incompatible. PostgreSQL backup/restore
requires `pg_dump`/`pg_restore` installed on the machine running the CLI;
SQLite backup remains local-file based.

`deploy/asterferry-controller.service` is a single-replica systemd unit.
For HA, run two PostgreSQL-backed Controller processes under an external
supervisor and load balancer; both must use the same config, CA, TLS identity
and master key. Nodes retain their encrypted last-known-good snapshot while
the Controller is unavailable and reconnect after takeover.

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
replaces the file. `deploy/helm/asterferry-controller` creates a single-replica SQLite
StatefulSet by default, or a two-replica PostgreSQL active/standby StatefulSet
when `controller.highAvailability.enabled=true`. HA mounts one existing Secret
containing `controller.json`, `master.key`, `ca.key`, `ca.crt`,
`controller.key` and `controller.crt`; it does not create a PVC.
`deploy/helm/asterferry-node` copies the Secret-provided
enrollment seed into its state PVC with an init container so rotation never
mutates the Kubernetes Secret.

Each Gateway must have its own reachable public endpoint. Shared VIP takeover
and transparent connection migration remain outside the first release. The
Controller chart's normal Service selects only ready Pods, so an external
load balancer or Kubernetes Service provides leader routing.

AFDP/1 to AFDP/2 is a deliberate hard wire break: QUIC ALPN and the protocol
version byte both changed, and there is no fallback. Roll out all Node binaries
in coordination with the Controller-side release.

## Operations and verification

The REST API is rooted at `/api/v1`. The endpoint policy is deliberate:
`/healthz` and `/readyz` are anonymous; `/readyz` returns only a boolean
readiness result, while `/metrics` requires an authenticated Viewer,
Operator or Admin (use a read-only Viewer API token for Prometheus);
and `/openapi.yaml` plus `/api/v1/openapi.yaml` are anonymous for client
discovery. If the OpenAPI document is considered deployment-sensitive, limit
those paths with the HTTPS ingress or network policy. Browser Cookie sessions
are durable hashed records with a 12-hour lifetime and are shared by
PostgreSQL active/standby replicas. Logout, password changes, expiry and backup
restore invalidate them; use API tokens for automated clients. SQLite remains
single-replica.
Mutating requests use `If-Match` revisions and may include an `Idempotency-Key`.
Use the Dashboard only as a Controller client.

Runtime connection metadata is read-only by default and never includes payloads.
Admin can enable advanced runtime operations in Dashboard → Admin. Operators can
then disconnect or temporarily rate-limit active Node connections; controls are
ephemeral, audited and bounded by TTL. See the bilingual
[runtime operations guide](../docs/operations.en.md) and
[中文运维指南](../docs/operations.zh-CN.md).

Generated Dashboard assets are intentionally not committed. A source-built
single binary must generate `internal/dashboard/dist/` first and use the
`dashboard_assets` build tag; Docker and GoReleaser perform these steps in their
release pipelines.

```sh
go test ./...
go vet ./...
npm --prefix web/dashboard ci --audit=false --registry=https://registry.npmjs.org --replace-registry-host=always
npm --prefix web/dashboard test -- --run
npm --prefix web/dashboard run build
go build -tags=dashboard_assets ./cmd/asterferry
helm lint deploy/helm/asterferry-controller deploy/helm/asterferry-node
docker compose -f deploy/docker/compose.yaml config
```

## Release artifacts

The normalized architecture uses `v1.0.0` as its first public release version.
Controller and Node installations must use the same version and must not mix
these binaries with the pre-release generation. A tagged release
publishes the native archives and `install-node.sh`/`install-node.ps1` to the
GitHub Release, together with `SHA256SUMS` and a source SBOM. It also publishes
the multi-architecture image at `ghcr.io/eternallyzzz/asterferry` and the
`asterferry-controller` and `asterferry-node` charts as separate OCI artifacts
under `ghcr.io/eternallyzzz/charts`.

Verify the downloaded archive against `SHA256SUMS` before installation. For a
fresh deployment, initialize the Controller with `--release-version 1.0.0` so
generated Node installation commands refer to the matching release assets. The
release manifest records immutable image and Chart digests; set Helm's
`image.digest` to pin an installation, which takes precedence over
`image.tag`.
