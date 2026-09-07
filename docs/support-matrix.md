# v1.0.0 support matrix

The v1.0.0 release is a self-hosted Controller plus generic Node deployment for
personal and small-team private networks. SQLite is single-replica; PostgreSQL
also supports an active/standby pair with external traffic routing.

| Surface | Officially supported | Release evidence |
| --- | --- | --- |
| Native Controller/Node | Linux amd64, Linux arm64, Windows amd64 | Go tests, race tests, native archives |
| WSL2 | Compatibility-tested development/deployment environment; installer supports both systemd and no-systemd WSL fallback; not a formal support target | WSL functional/race script when the local toolchain is available |
| Container image | Linux amd64 and arm64 when built by the operator | Dockerfile and local multi-architecture build validation; the project does not publish an image registry artifact |
| Helm | Kubernetes deployments using the Controller and Node charts | Source chart lint/render validation; set `image.repository` to an operator-built image; the project does not publish OCI charts |
| Controller database | SQLite (default) and PostgreSQL (production-scale) | SQLite test suite and PostgreSQL 16 CI service |
| Controller availability | One SQLite replica, or exactly two PostgreSQL active/standby replicas with a shared identity/config Secret and external load balancer/Service | Lease/fencing integration, readiness gate, Node reconnect and failover smoke |
| GeoIP | Optional external MaxMind-compatible file | Read-only mount/path, freshness check and explicit fallback tests |
| Native self-upgrade | Controller/Node on Windows service, Linux systemd or WSL managed process; amd64 and Linux arm64 Node artifacts | Stable-release discovery, SHA256SUMS verification, readiness rollback and one-Node dispatch tests |
| Container/Helm upgrade | Operator-built image and source-chart rollout by the deployment platform; in-container binary replacement is unsupported | Update API reports rollout-required status; deployment manifests use explicit container mode and image repository |

The exact Go, Node.js and npm release-build pins live in `.toolchain.json` and
are checked against CI, Docker and release scripts. Node.js is a build-only
dependency; production Controller and Node processes do not need Node.js.
The Dashboard is also tested in CI against the pinned Node.js 22 compatibility
lane. Dependency upgrades are frozen during the RC soak and are evaluated in a
separate change.

All Controller and Node binaries in one deployment must use the same v1.x
release line. Mixed releases are not a supported upgrade strategy when the
wire or database contract changes. Nodes installed before the self-upgrade
capability need one manual installer upgrade before Controller-managed updates
are enabled.
