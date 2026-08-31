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
- Adds an explicit stopped-database migration from Controller schema v3 or v4
  to schema v5. Run `asterferry controller migrate --config <controller.json>`
  during a maintenance window; `OpenStore` never rewrites a database
  implicitly, and a pre-v5 rollback backup is retained after publication.
- AFDP/1 to AFDP/2 is an intentional wire-level breaking change: both the QUIC
  ALPN and version byte changed, with no fallback codec. Upgrade Controller,
  Gateway and Agent binaries as a coordinated rollout.
