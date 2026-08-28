# Changelog

## [1.0.0] - Unreleased

This release is a deliberate breaking cutover to the Controller/data-plane
architecture.

- Adds the SQLite-backed Controller with RBAC, audit, enrollment, scheduling,
  revision checks and encrypted node snapshots.
- Adds AFDP/1 over QUIC with typed session negotiation, bounded TCP/UDP framing,
  routing, egress and packet obfuscation.
- Makes Gateway and Agent independent node processes with offline last-known-
  good state, certificate rotation and observed-state reporting.
- Ships REST/OpenAPI and a Dashboard with Controller resource CRUD, assignment
  visibility, runtime actions and audit history.
- Replaces role-specific YAML, bundles, local Supervisor state and the v6 data
  protocol. Existing stores and node configurations must be initialized again;
  no compatibility or automatic migration is provided.
