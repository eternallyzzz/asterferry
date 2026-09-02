# AsterFerry

AsterFerry is a private-network relay with a strict control-plane/data-plane
split. The Controller is the only authority for identity, RBAC, desired state,
scheduling, audit and SQLite persistence. Nodes receive typed snapshots, keep
an encrypted last-known-good cache, and carry traffic without an HTTP
management surface; Gateway or Agent is selected by the Node Spec.

```text
  Dashboard / CLI -- HTTPS REST --> Controller -- mTLS gRPC --> Node daemon
                                      |
                                      +-- SQLite, audit, scheduling

  Node (Gateway behavior) <====== AFDP/2 over QUIC ======> Node (Agent behavior)
```

This is a breaking generation. The current deployment accepts only the
Controller JSON bootstrap and AFDP/2; there is no legacy YAML, bundle,
Supervisor, management API, or v6 codec compatibility layer.

## Quick start

For a complete three-server deployment (Controller plus two Nodes), use
the copy-and-run guides:

- [End-to-end quick start in English](docs/quickstart.en.md)
- [中文端到端快速开始](docs/quickstart.zh-CN.md)
- [Operations and runtime visibility (English)](docs/operations.en.md)
- [运行时观测与运维（中文）](docs/operations.zh-CN.md)

Initialize the Controller. `--grpc-advertise` is required and must be the
host:port that Gateway and Agent hosts can reach; it is not the local bind
address and must not be `0.0.0.0`. Set `--release-version` to an existing
release asset version. The generated Admin password is printed once when no
password is supplied:

```powershell
asterferry controller init --dir ./controller `
  --grpc-advertise controller.example.com:9443 `
  --release-version 1.0.0
asterferry controller run --config ./controller/controller.json
```

For an existing installation whose `grpc_advertise` is empty or has changed,
stop the Controller and update it without replacing the database, CA, master
key or Admin account. The command reissues the Controller certificate with the
new address in its SAN; restart the Controller afterwards:

```powershell
asterferry controller configure `
  --config ./controller/controller.json `
  --grpc-advertise controller.example.com:9443
```

If the old configuration also has no published `release_version`, append
`--release-version 1.0.0` (and use `--release-base-url` for a private HTTPS
mirror).

For a Windows Controller with Nodes running in the local WSL instance, the
current WSL gateway is commonly `172.28.80.1:9443`. Use that only for local
host-to-WSL testing; remote Nodes must use Controller C's stable LAN or DNS
address.

Then open the Dashboard at `/dashboard/`. On the Nodes page create a pending
installation task with only the Node ID, name, labels and target platform. The
Controller returns one one-time Linux or Windows installer command. Run it once
on the target Node host as Administrator/root. The command downloads and verifies
the matching release, reaches the Controller to enroll the Node, and starts the generic
`asterferry node run` service. The Node is not created in the enrolled-node list
until that first enrollment succeeds.

After the Node is online, open its details and save a behavior spec: choose
`Gateway` for the public node and configure its reachable AFDP endpoint/port
pools, or choose `Agent` for the private node and configure its selector/routing.
This is the only point where a
Node becomes a Gateway or Agent. After both behavior specs are healthy, create
services in the Dashboard; the Controller schedules them automatically.

The manual enrollment path remains available for offline or custom images. The
Node identity must already exist (the command below binds the token to it); for
a new identity, use the install-first Dashboard flow:

```powershell
asterferry enroll-token create --config ./controller/controller.json --node-id edge-east
asterferry node enroll --controller controller.example:9443 --token <one-time-token> --node-id edge-east --ca ./controller/ca/ca.crt --output node-bootstrap.json
asterferry node run --bootstrap node-bootstrap.json
```

Business resources are managed through `/api/v1` or the optional Dashboard at
`/dashboard/`. The public API has one Node resource tree; Gateway/Agent are
behavior values under `/nodes/{id}/spec`. Mutating API requests use `If-Match` revisions and
may include an `Idempotency-Key`.

Controller backups include the database, master key, CA and TLS identity:

```text
asterferry controller backup --config ./controller/controller.json --output ./backups
asterferry controller restore --config ./controller/controller.json --source ./backups/<timestamp> --destination ./controller-restored
```

## Control plane

`controller init --grpc-advertise <reachable-host:port>` creates a JSON-only local configuration, a Controller CA,
HTTPS/gRPC certificates, a 32-byte owner-readable master key, and the first
Admin account. SQLite runs with WAL, foreign keys and a busy timeout. Secrets
are AES-GCM encrypted with the master key, passwords use Argon2id, and API or
enrollment tokens are stored only as hashes.

The REST API supports login/logout, Cookie sessions with CSRF protection, API
tokens, fixed Viewer/Operator/Admin roles, unified Node and Node Spec resources,
typed Gateway/Agent behavior documents, services, assignments, enrollment
tokens, runtime actions, observed state, events and audit queries. `/healthz` is anonymous; `/readyz` and `/metrics` are protected
by deployment policy. OpenAPI is served at `/openapi.yaml`.

The Controller scheduler preserves a healthy existing assignment when possible,
otherwise selects a matching Gateway by labels and capacity. Explicit public
port collisions are transactional conflicts; zero ports are allocated from the
Gateway's protocol-specific pool. Every desired snapshot has one node scope,
monotonic generation, schema version and SHA-256 checksum.

### Controller database lifecycle

This pre-1.0 generation uses SQLite schema v9. A v8 database is upgraded once on
startup with the additive runtime-observability migration; unknown or older
generations are still rejected by `OpenStore`. Back up the complete Controller
directory before upgrading and keep the backup for rollback.

## Node behavior

Nodes have only bootstrap identity material, Controller address, cache location
and logging options. Enrollment exchanges a one-time 15-minute token and an
Ed25519 CSR for a 30-day mTLS certificate. Certificates rotate automatically
seven days before expiry; revocation closes online streams and is enforced on
the next connection.

The node reconciler validates a complete snapshot and builds a new data-plane
index before atomically switching generations. Failed validation, checksum,
component application or encrypted-cache publication leaves the previous
generation active. While the Controller is unavailable, the node continues
using its encrypted last-known-good snapshot and reports degraded state after
the configured offline grace period.

### Runtime visibility and operations

Nodes continuously report payload-free runtime metadata after an authenticated
control stream is ready: AFDP sessions, TCP connections, UDP flows and egress
streams include source IP/port, peer, assignment/service, target, protocol,
state, byte counters and rates. The Controller keeps current state, lifecycle
events and per-minute traffic rollups for 30 days. No application payload is
captured or sent to the Controller.

The Dashboard shows this information read-only by default in each Node's
Observed view. Admin can enable **Advanced runtime operations** in the Admin
page; only then can Operators disconnect or apply a temporary byte-rate limit
to a selected connection. Controls live in the Node process, have a bounded
TTL, are audited, and are cleared when the feature is disabled. See the
[operations guide](docs/operations.en.md) for selectors, API paths and failure
semantics.

## AFDP/2 data plane

AFDP/2 uses QUIC with TLS 1.3 and ALPN `asterferry-data/2`. A reliable control
stream begins with `SessionHello`/`SessionAccept` carrying the assignment,
generation and capabilities. The Gateway accepts only Agents present in its
locally applied assignment.

TCP, reverse-TCP and proxy streams carry one bounded protobuf Open message and
then raw bytes. UDP uses QUIC DATAGRAM with a fixed version/flow/sequence/
fragment header, bounded reassembly, duplicate rejection and expiry. Stream,
datagram, message, fragment and buffer limits are negotiated and malformed
input fails closed. Routing, egress, quotas and obfuscation policy come from
the node snapshot; the Controller never enters the business data path.

AFDP/1 and AFDP/2 are not wire-compatible: the ALPN and version byte changed,
and there is no fallback codec. Upgrade all Node binaries together
with the Controller-side rollout before opening new data-plane sessions.

The first release supports multiple Gateways only when each has an independent
reachable public endpoint. Shared VIP takeover, transparent connection
migration and Controller HA are deliberately out of scope.

## Deployment and verification

The Controller has standalone Docker, systemd and single-replica StatefulSet
examples under `deploy/`. Every data-plane host runs the same generic `node`
command and mounts only its bootstrap/cache directories; its Gateway or Agent
behavior is selected later by the Node Spec. The Dashboard is static and can
be disabled in Controller JSON.

Run the core verification suite:

```powershell
go test ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 -checks=all,-SA1019 ./...
npm --prefix web/dashboard test
npm --prefix web/dashboard run build
go test -tags=integration -count=1 -timeout=5m ./internal/integration
```

The Dashboard client bounds each Controller request at 15 seconds and reports
a localized timeout instead of waiting indefinitely when an HTTPS proxy or
Controller is unavailable. Additional protocol fuzzing and deployment smoke
tests should be run before release on Linux, WSL and Windows.

On Windows, the race detector requires CGO and a recent mingw-w64 toolchain.
Use GCC 10.3 or newer (or an LLVM MinGW release that provides
`libsynchronization.a`), then point Go at that compiler before running the
race suite:

```powershell
go env -w CGO_ENABLED=1
go env -w CC=C:\path\to\mingw64\bin\gcc.exe CXX=C:\path\to\mingw64\bin\g++.exe
go test -race ./...
```

An older MinGW compiler can build ordinary tests but fail to start race
instrumented binaries with Windows status `0xc0000139`.
