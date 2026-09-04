# v1.0 support matrix

The v1.0 release is a self-hosted Controller plus generic Node deployment for
personal and small-team private networks. The release is intentionally
single-replica; PostgreSQL does not turn the Controller into an HA service.

| Surface | Officially supported | Release evidence |
| --- | --- | --- |
| Native Controller/Node | Linux amd64, Linux arm64, Windows amd64 | Go tests, race tests, native archives |
| WSL2 | Compatibility-tested development/deployment environment; not a formal support target | WSL functional/race script when the local toolchain is available |
| Container image | Linux amd64 and arm64 | Multi-architecture Buildx image, SBOM, provenance and signature |
| Helm | Kubernetes deployments using the Controller and Node charts | Lint, rendered manifests, packaged OCI charts |
| Controller database | SQLite (default) and PostgreSQL (production-scale) | SQLite test suite and PostgreSQL 16 CI service |
| GeoIP | Optional external MaxMind-compatible file | Read-only mount/path, freshness check and explicit fallback tests |

Go `1.26.7`, Node.js `24.19.0` and npm `12.0.2` are the pinned release-build
toolchains. Node.js is a build-only dependency; production Controller and Node
processes do not need Node.js. Dependency upgrades are frozen during the RC
soak and are evaluated in a separate change.

All Controller and Node binaries in one deployment must use the same v1.x
release line. Mixed releases are not a supported upgrade strategy when the
wire or database contract changes.
