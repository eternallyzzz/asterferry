# AsterFerry

AsterFerry is a private-network relay with a strict control-plane/data-plane
split. The Controller is the only authority for identity, RBAC, desired state,
scheduling, audit and SQLite persistence. Gateway and Agent nodes receive typed
snapshots, keep an encrypted last-known-good cache, and carry traffic without
an HTTP management surface.

```text
Dashboard / CLI -- HTTPS REST --> Controller -- mTLS gRPC --> Gateway / Agent
                                      |
                                      +-- SQLite, audit, scheduling

Gateway <============= AFDP/1 over QUIC =============> Agent
```

This is a breaking generation. The current deployment accepts only the
Controller JSON bootstrap and AFDP/1; there is no legacy YAML, bundle,
Supervisor, management API, or v6 codec compatibility layer.

## Quick start

Initialize the Controller. The generated Admin password is printed once when
no password is supplied:

```powershell
asterferry controller init --dir ./controller
asterferry controller run --config ./controller/controller.json
```

Register a node in the Controller, create a short-lived role-bound token, and
enroll its bootstrap identity:

```powershell
asterferry enroll-token create --config ./controller/controller.json --role gateway
asterferry gateway enroll --controller controller.example:9443 --token <one-time-token> --node-id gw-east --ca ./controller/ca/ca.crt --output gw-bootstrap.json
asterferry gateway run --bootstrap gw-bootstrap.json
```

Use the corresponding `agent enroll` and `agent run --bootstrap` commands for
an Agent. Business resources are managed through `/api/v1` or the optional
Dashboard at `/dashboard/`. Mutating API requests use `If-Match` revisions and
may include an `Idempotency-Key`.

Controller backups include the database, master key, CA and TLS identity:

```text
asterferry controller backup --config ./controller/controller.json --output ./backups
asterferry controller restore --config ./controller/controller.json --source ./backups/<timestamp> --destination ./controller-restored
```

## Control plane

`controller init` creates a JSON-only local configuration, a Controller CA,
HTTPS/gRPC certificates, a 32-byte owner-readable master key, and the first
Admin account. SQLite runs with WAL, foreign keys and a busy timeout. Secrets
are AES-GCM encrypted with the master key, passwords use Argon2id, and API or
enrollment tokens are stored only as hashes.

The REST API supports login/logout, Cookie sessions with CSRF protection, API
tokens, fixed Viewer/Operator/Admin roles, Node/Gateway/Agent/Service CRUD,
assignments, enrollment tokens, runtime actions, observed state, events and
audit queries. `/healthz` is anonymous; `/readyz` and `/metrics` are protected
by deployment policy. OpenAPI is served at `/openapi.yaml`.

The Controller scheduler preserves a healthy existing assignment when possible,
otherwise selects a matching Gateway by labels and capacity. Explicit public
port collisions are transactional conflicts; zero ports are allocated from the
Gateway's protocol-specific pool. Every desired snapshot has one node scope,
monotonic generation, schema version and SHA-256 checksum.

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

## AFDP/1 data plane

AFDP/1 uses QUIC with TLS 1.3 and ALPN `asterferry-data/1`. A reliable control
stream begins with `SessionHello`/`SessionAccept` carrying the assignment,
generation and capabilities. The Gateway accepts only Agents present in its
locally applied assignment.

TCP, reverse-TCP and proxy streams carry one bounded protobuf Open message and
then raw bytes. UDP uses QUIC DATAGRAM with a fixed version/flow/sequence/
fragment header, bounded reassembly, duplicate rejection and expiry. Stream,
datagram, message, fragment and buffer limits are negotiated and malformed
input fails closed. Routing, egress, quotas and obfuscation policy come from
the node snapshot; the Controller never enters the business data path.

The first release supports multiple Gateways only when each has an independent
reachable public endpoint. Shared VIP takeover, transparent connection
migration and Controller HA are deliberately out of scope.

## Deployment and verification

The Controller has standalone Docker, systemd and single-replica StatefulSet
examples under `deploy/`. Gateway and Agent are independently deployable and
mount only their bootstrap/cache directories. The Dashboard is static and can
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
