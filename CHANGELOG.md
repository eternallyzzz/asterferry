# Changelog

## [1.0.0] - Unreleased

This release is a deliberate breaking cutover to the Controller/data-plane
architecture.

- Adds the SQLite-backed Controller with RBAC, audit, enrollment, scheduling,
  revision checks and encrypted node snapshots.
- Adds AFDP/2 over QUIC with typed session negotiation, bounded TCP/UDP framing,
  routing, egress and packet obfuscation.
- Makes Gateway and Agent independent node processes with offline last-known-
  good state, certificate rotation and observed-state reporting.
- Ships REST/OpenAPI and a Dashboard with Controller resource CRUD, assignment
  visibility, runtime actions and audit history.
- Replaces role-specific YAML, bundles, local Supervisor state and the v6 data
  protocol. Existing node configurations must be initialized again; existing
  Controller stores require the explicit migration described below.
- Adds an explicit stopped-database migration from Controller schema v3, v4 or
  v5 to schema v6. The new `node_bootstraps` table stores pending installation
  intents without pre-creating node identities. Run
  `asterferry controller migrate --config <controller.json>` during a
  maintenance window; `OpenStore` never rewrites a database implicitly, and a
  pre-v6 rollback backup is retained after publication.
- Adds Dashboard install-first provisioning: the generated command is the only
  action required on a Gateway or Agent host, and the node/spec are created
  atomically when the host completes its first enrollment.
- AFDP/1 to AFDP/2 is an intentional wire-level breaking change: both the QUIC
  ALPN and version byte changed, with no fallback codec. Upgrade Controller,
  Gateway and Agent binaries as a coordinated rollout.
