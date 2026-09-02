# Changelog

## [1.0.0] - Unreleased

This release is a deliberate breaking cutover to the Controller/data-plane
architecture.

- Adds the SQLite-backed Controller with RBAC, audit, enrollment, scheduling,
  revision checks and encrypted node snapshots.
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
  30 days; schema v8 receives an additive v9 migration.
- Replaces role-specific YAML, bundles, local Supervisor state and the v6 data
  protocol. Existing node configurations must be initialized again. The
  Controller store is now schema v9; `OpenStore` upgrades the immediately
  previous v8 generation for the additive runtime tables and rejects older
  generations instead of rewriting their business data.
- The `node_bootstraps` table stores pending installation intents and
  `node_specs` stores the behavior envelope without pre-creating node
  identities. Back up/export the old Controller before creating the new
  generation during the pre-release upgrade window.
- Adds Dashboard install-first provisioning: the generated command is the only
  action required on a Node host, and the identity plus any optional initial
  spec are created atomically when the host completes its first enrollment.
- AFDP/1 to AFDP/2 and control/1 to control/2 are intentional wire-level
  breaking changes with no fallback or negotiation. Upgrade Controller and all
  Node binaries as a coordinated rollout.
