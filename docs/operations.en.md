# AsterFerry runtime operations

This guide describes the operational view added to the unified Node model.
Every host still runs one `asterferry node run` process; Gateway and Agent are
behavior specs applied to that Node. Runtime operations are separate from the
desired spec and do not require a process restart.

## What is visible by default

After a Node establishes an authenticated Controller stream, it reports
payload-free metadata for:

- the Node-to-Node AFDP session;
- TCP connections and UDP flows carrying a Service;
- Agent-to-Gateway egress streams.

The metadata includes connection ID, type, state, source IP/port when the
process can observe it, peer Node, Gateway/Agent, Assignment, Service, target,
protocol, start/last-activity/end timestamps, byte counters and average byte
rates. It never includes application payload, packet contents, credentials or
full request data.

The Node keeps active entries in a bounded in-memory registry. The Controller
stores the current/recent connection view, lifecycle events and per-minute
traffic rollups. History is pruned after 30 days. A reconnect or process
restart can therefore lose ephemeral active controls, but does not make a
closed connection's already-reported history live again.

Open a Node and select **Observed** to see the read-only table. Gateway rows
show the Agent peer and, for reverse services, the client source IP that
entered the Gateway. Agent rows show the Gateway peer and the local target.

## Advanced operations

Advanced operations are disabled by default. An Admin enables them in
**Dashboard → Admin → Advanced runtime operations**. The read-only metadata
remains visible when the switch is off.

When enabled, an Operator can act on active entries in **Node → Observed**:

- **Disconnect** closes one selected session, TCP connection, UDP flow or
  egress stream.
- **Rate limit** installs a temporary byte-per-second limit with an inbound,
  outbound or bidirectional direction and a TTL (maximum 24 hours).
- **Clear limit** removes the temporary limit.

Controls are held in the Node process rather than desired snapshots. They are
therefore not silently replayed after a replacement or restart. Disabling the
switch blocks new operation requests and asks connected Nodes to clear their
limits; a Node that reconnects while the switch is disabled receives the same
clear instruction.

For direction, `in` and `out` mean traffic entering or leaving the Node. On a
Gateway reverse Service, client-to-Agent traffic is inbound and Agent-to-client
traffic is outbound. On a Gateway egress stream, Agent-to-external traffic is
inbound and external-to-Agent traffic is outbound. Every request is audited;
delivery is best effort and an offline Node does not receive an old operation
automatically.

## REST API

All paths below are under `/api/v1` and require the corresponding Controller
role:

| Path | Role | Purpose |
| --- | --- | --- |
| `GET /runtime/settings` | Viewer | Read the switch state and retention period |
| `PUT /runtime/settings` | Admin | Enable or disable advanced operations |
| `GET /runtime/connections` | Viewer | Query current/recent metadata globally |
| `GET /nodes/{id}/runtime/connections` | Viewer | Query one Node's metadata |
| `GET /runtime/events` | Viewer | Read retained lifecycle/action events |
| `GET /runtime/traffic` | Viewer | Read minute rollups |
| `GET /runtime/stream` | Viewer | Receive SSE change notifications |
| `POST /nodes/{id}/runtime/actions` | Operator | Act by selector |
| `POST /nodes/{id}/runtime/connections/{connection}/actions` | Operator | Act on one connection |

Global and Node-scoped connection queries accept `state`, `type`, `source_ip`,
`peer_node_id`, `gateway_id`, `agent_id`, `assignment_id`, `service_id`,
`protocol` and `limit`. Selector actions accept `connection_id`, `source_ip`,
`peer_node_id`, `assignment_id`, `service_id` and `protocol`. A selector is
required for a node-level action so an accidental empty request cannot affect
all connections.

Example: rate-limit all TCP connections from one observed source for ten
minutes after the Admin switch is enabled:

```sh
curl -X POST https://controller.example/api/v1/nodes/gateway-b/runtime/actions \
  -H 'Authorization: Bearer <operator-token>' \
  -H 'Content-Type: application/json' \
  -d '{
    "action": "rate_limit",
    "selector": {"source_ip": "198.51.100.24", "protocol": "tcp"},
    "direction": "both",
    "bytes_per_second": 1048576,
    "burst_bytes": 2097152,
    "ttl_seconds": 600
  }'
```

The response means accepted/queued or delivered to the current control
stream; it is not a promise that a connection still existed when the Node
applied the action. Watch the runtime event stream and the Node's Observed
view for the resulting state.

## Troubleshooting

- No rows: verify the Node has an active certificate and an authenticated
  control stream, then check **Observed** and `runtime-telemetry-v1` in the
  Node logs.
- Rows are stale/unknown: the Node is offline or its last snapshot has not
  arrived. Current state is intentionally fail-safe and never inferred from
  the Controller's desired spec.
- Operation returns `advanced_operations_disabled`: an Admin must enable the
  switch first. A Viewer cannot operate even when it is enabled.
- Operation is accepted but no change appears: the Node may be offline, the
  connection may have ended between query and action, or the action channel may
  be full. Check the runtime action result event and repeat deliberately after
  verifying the selector.
- A rate limit disappears: it expired, the Node restarted, the generation was
  replaced, or an Admin disabled the feature. Runtime controls are not config
  state.

The Controller never enters the business data path. If a richer payload-level
inspection is needed, use the application protocol's own logging and tracing;
this feature intentionally remains metadata-only.
