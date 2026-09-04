# Changelog

## [2.0.0] - Unreleased

This release is a deliberate breaking cutover to the normalized Controller/
data-plane architecture.

- Adds the SQLite-backed Controller with RBAC, audit, enrollment, scheduling,
  revision checks and encrypted node snapshots; PostgreSQL is now supported as
  the production-scale backend with a bounded pool.
- Adds AFDP/2 over QUIC with typed session negotiation, bounded TCP/UDP framing,
  routing, egress and packet obfuscation.
- Makes Gateway and Agent behavior run through one generic Node identity and
  daemon, with offline last-known-good state, certificate rotation and
  observed-state reporting. Behavior is selected from the Node Spec after the
  host completes enrollment.
- Ships REST/OpenAPI and a Dashboard with Controller resource CRUD, assignment
  visibility, runtime actions and audit history.
- Adds payload-free runtime connection metadata, lifecycle events, per-minute
  traffic rollups, node-scoped selectors and an Admin-gated advanced operations
  switch for disconnect/rate-limit controls. Runtime history is retained for
  30 days.
- Replaces role-specific YAML, bundles, local Supervisor state and the v6 data
  protocol. Existing node configurations and Controller databases must be
  initialized again. The Controller store is now a fresh database schema v11
  with a relational aggregate layout and a single canonical database marker;
  old databases and backup manifests are
  rejected without migration.
- The `node_bootstraps` table stores pending installation intents and
  `node_specs` stores the behavior envelope without pre-creating node
  identities. Before installing this initial public release, preserve any old
  Controller backup separately; it is not a migration input.
- Adds Dashboard install-first provisioning: the generated command is the only
  action required on a Node host, and the identity plus any optional initial
  spec are created atomically when the host completes its first enrollment.
- AFDP/1 to AFDP/2 and control/1 to control/2 are intentional wire-level
  breaking changes with no fallback or negotiation. Upgrade Controller and all
  Node binaries as a coordinated rollout.
- PostgreSQL backup/restore uses the external `pg_dump`/`pg_restore` client
  utilities. The Controller remains single-replica; PostgreSQL does not add
  Controller HA.
- Hardens storage error classification, PostgreSQL connection lifetime,
  idempotency retention, schema probing, joined resource reads and UDP flow
  cleanup, with behavior contracts for the affected paths.
- Adds deterministic QUIC half-close, AFDP reassembly, UDP lifecycle and
  reconnect state-machine tests, real loopback transport contracts, and the
  maintainer-facing architecture document.
- Adds an opt-in, separately bound Prometheus listener with a loopback default;
  management HTTPS metrics remain authenticated. GeoIP routing is now an
  optional external, freshness-checked resource instead of a repository or
  image binary.
- Adds release-candidate tagging, seven-day soak guidance, tracked-secret
  scanning, protocol fuzz smoke and same-runner benchmark regression gates.
