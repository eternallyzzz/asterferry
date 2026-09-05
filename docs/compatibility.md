# Compatibility contract

## v2.x freeze

The v2.0 release establishes the public compatibility line:

- AFDP/2 and control/2 are the supported wire protocols.
- The REST/OpenAPI contract, snapshot schema and backup format are stable
  inputs for v2.x.
- A v2.x release may add optional fields and endpoints, but must not silently
  change the meaning of existing fields, authentication requirements, wire
  framing, or persisted data.
- A breaking wire, API, or database change belongs in v2. It requires a
  documented migration path or an explicitly announced fresh-generation cut.

## Fresh-generation boundary

Pre-v2 binaries, configurations, databases and backup manifests are rejected;
there is no in-place schema migration or SQLite-to-PostgreSQL conversion in
v2.0. Database schema v12 is the first normalized v2 generation. This is a
deliberate boundary, not an upgrade mechanism to repeat for every v2.x patch.

Before any upgrade, export a Controller backup and verify that it can be
listed/restored into a disposable directory using the same release line. Keep
the previous installation intact until the new process passes readiness and a
Node data-plane smoke test.

## Recovery objectives

For a two-replica PostgreSQL Controller with durable PostgreSQL failover, the
availability target is RTO <=30 seconds and RPO 0 for committed control data.
This does not migrate existing gRPC or AFDP streams; Nodes reconnect and the
external routing layer removes the old Pod through `/readyz`.

Disaster restore is a separate objective: RTO is determined by the operator's
backup and PostgreSQL restore procedure, and RPO is the timestamp of the last
successful verified backup. Restore invalidates all browser sessions and
resets the Controller lease. Operators must retain the config, master key, CA
and TLS identity together with the database.

## Independent version identifiers

`/api/v1` is the REST route contract. The control-wire and snapshot payloads
use protocol version `2`, while the physical Controller database uses schema
version `12` with the `relational` layout marker. Protocol v2 carries the exact
Agent-to-Gateway binding. These identifiers are independent: a database layout
change does not silently change the REST route or payload protocol.
