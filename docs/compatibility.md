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
v2.0. Database schema v11 is the first normalized v2 generation. This is a
deliberate boundary, not an upgrade mechanism to repeat for every v2.x patch.

Before any upgrade, export a Controller backup and verify that it can be
listed/restored into a disposable directory using the same release line. Keep
the previous installation intact until the new process passes readiness and a
Node data-plane smoke test.

## Recovery objectives

The operational target is RTO 30 minutes and RPO equal to the timestamp of the
last successful verified backup. Operators must schedule backups often enough
for that RPO to be meaningful and must retain the config, master key, CA and
TLS identity together with the database.

## Independent version identifiers

`/api/v1` is the REST route contract. The control-wire and snapshot payloads
use protocol version `1`, while the physical Controller database uses schema
version `11` with the `relational` layout marker. These identifiers are
independent: a database layout change does not silently change the REST route
or payload protocol.
