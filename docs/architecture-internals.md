# AsterFerry implementation notes

This document is intentionally volatile. It records implementation details
that help maintainers navigate the current code and state-machine contracts;
changes to those details do not automatically imply an architecture change.
When code moves, update this file in the same change or remove the stale
detail. Stable ownership and compatibility rules belong in
[`architecture.md`](architecture.md).

## Current Controller composition

`ControllerRepositories` is the composition root. It wires one
`databaseHandle`, one `ChangeBus`, a `ResourceRepository`, and a
`RuntimeRepository`. The resource repository owns normalized control resources,
snapshots, observed state, auth, enrollment, audit, and the low-frequency
`runtime_settings` control switch. The runtime repository owns
`runtime_connections`, `runtime_events`, and `runtime_traffic_rollups` plus
their retention cleanup. The two repositories deliberately share a pool but do
not call each other's business methods.

The scheduler consumes `SchedulingRepository`, and the current candidate path
uses bounded set-based loads for Gateway specs, assignments, and observed state.
It must not grow a per-candidate SQL lookup loop. Keep the pure `Schedule`
function independent from candidate acquisition so placement tests do not need
a database.

## Normalized aggregate map

The current codec files are split by aggregate:

| Aggregate | Scalar/child families | Codec files |
| --- | --- | --- |
| NodeSpec | `node_specs` dispatch | `resource_node_codec.go` |
| Gateway | `gateway_specs`, endpoints, labels, listeners, port ranges, egress values | `resource_gateway_codec.go`, `resource_gateway_batch_codec.go` |
| Agent | `agent_specs`, selectors, proxies, routes, route values, egress values | `resource_agent_codec.go`, `resource_agent_batch_codec.go` |
| Service | service scalar and selector labels | `resource_service_codec.go` |
| Assignment | assignment scalar, services, bindings | `resource_assignment_codec.go`, `store_normalized_batch_assignments.go` |
| Observed | observed scalar, sessions, listeners | `resource_observed_codec.go`, `store_normalized_batch_observed.go` |

Writers use full replacement of ordered child sets inside the owning
transaction. Ordered loaders require dense zero-based positions; grouped
positions are dense per protocol or value kind. Unknown child kinds and
cross-owner identities are errors. Batch loaders preserve the same checks while
loading bounded ID chunks.

## Runtime metric implementation

`domain.RuntimeMetrics` is the typed control document. Its finite catalog is
`RuntimeMetricCatalog()` and its independent version is
`RuntimeMetricsSchemaVersion`. The current catalog contains active gauges,
runtime counters, and the optional GeoIP boolean. The Controller's
`observed_states` columns and both OpenAPI copies are checked against that
catalog by contract tests.

Do not reintroduce a generic metric map into observed state. For a new metric,
decide first whether it is stable enough for the control contract. If not, add
a bounded Prometheus metric with a documented label policy. If yes, update the
typed field, catalog, SQL read/write contract, OpenAPI, node producer, and
tests as one change.

## Data-plane details currently under test

- A generation owns listeners, sessions, UDP flow indexes, telemetry, and a
  cancellation context. Publication happens only after private construction
  succeeds.
- `duplex.CopyDuplex` treats EOF as a half-close and non-EOF failures as an
  abort of both directions.
- UDP removal is funneled through `removeUDPFlow`; pointer identity prevents
  late cleanup from deleting a replacement flow, and cleanup is idempotent.
- Agent reconnect phases are `disconnected`, `dialing`, `handshaking`,
  `admitting`, and `serving`, with exponential backoff reset on admission.
- The node/data-plane tests should express these as state transitions with an
  oracle. Fuzz tests only cover malformed input and resource bounds.

## Volatile operational facts

Controller HA is out of scope. Login sessions are stored in a process-local
`sync.Map` for 12 hours; restart or routing a request to another replica
invalidates the session. The dedicated metrics listener is intentionally a
separate, deployment-controlled scrape surface, while management metrics and
readiness use authenticated HTTP handlers. The main HTTP server has no global
write deadline because runtime SSE is long-lived; read and idle deadlines still
bound inactive clients. If SSE gets a dedicated server in the future, restore a
finite write timeout for ordinary handlers.

## Change checklist

When changing one of these implementation details:

1. update the relevant contract/state-machine test;
2. update this file if the navigation or behavior description changed;
3. update the stable architecture document only if ownership, dependency
   direction, compatibility, or an invariant changed;
4. run the release gates before tagging.
