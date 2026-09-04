# Docker Compose deployment

This Compose file runs the Controller and two generic Node deployment slots as
independent processes. Only the Controller mounts its config/PKI directory
(and the SQLite file when SQLite is selected). Nodes mount
an enrolled bootstrap JSON and an encrypted state directory; they do not mount
YAML business configuration or expose a management API.

## Prepare

Build the image and initialize the Controller once on the host:

```powershell
asterferry controller init --dir ./controller `
  --grpc-advertise controller.example.com:9443 `
  --release-version 2.0.0
docker compose -f deploy/docker/compose.yaml build
```

The default Compose example uses SQLite. PostgreSQL is recommended when the
Controller has many Nodes: initialize the config with
`--database-driver postgres --database-url 'postgres://...'`, then mount that
config into the Controller container. The PostgreSQL server is intentionally
external to this development Compose stack so credentials stay out of the
checked-in file and a managed PostgreSQL service can be used.

Create two generic Node installation tasks in the Controller and run the
generated commands on the target hosts, or enroll bootstrap files manually.
After each Node is online, select Gateway or Agent behavior in its Dashboard
detail page. By default Compose expects:

```text
controller/controller.json
nodes/gateway-bootstrap.json
nodes/agent-bootstrap.json
nodes/gateway-state/
nodes/agent-state/
```

Override those paths with `ASTERFERRY_CONTROLLER_DIR`,
`ASTERFERRY_GATEWAY_BOOTSTRAP`, `ASTERFERRY_AGENT_BOOTSTRAP`,
`ASTERFERRY_GATEWAY_STATE`, and `ASTERFERRY_AGENT_STATE`. Controller HTTPS and
gRPC ports are configurable with `ASTERFERRY_CONTROLLER_HTTPS_PORT` and
`ASTERFERRY_CONTROLLER_GRPC_PORT`; the Gateway data endpoint uses
`ASTERFERRY_GATEWAY_DATA_PORT`.

## Run

```powershell
docker compose -f deploy/docker/compose.yaml config
docker compose -f deploy/docker/compose.yaml up -d
docker compose -f deploy/docker/compose.yaml ps
```

The Controller health check is anonymous `/healthz`; `/readyz` is a public
boolean readiness probe, and the management HTTPS metrics endpoint is an
authenticated API endpoint. Native
configs additionally default to a loopback-only plain-HTTP metrics listener at
`127.0.0.1:9090`; publish it to a trusted Prometheus network only after adding
the corresponding compose/network rule and changing `metrics_listen` to the
container bind address (for example `:9090`). `/openapi.yaml` and
`/api/v1/openapi.yaml` are anonymous for client discovery; restrict them at the
reverse proxy/network layer if required. Browser Cookie sessions are durable
hashed records with a 12-hour lifetime. PostgreSQL active/standby deployments
share them; the example remains single-replica SQLite.

Nodes reconnect to the Controller and continue their last encrypted snapshot
while it is unavailable.

Stop the stack with `docker compose ... down`. Back up the Controller before
upgrades using `asterferry controller backup`; restore into a fresh data
directory with `asterferry controller restore`. For PostgreSQL, run the backup
CLI on a host/container that has `pg_dump` and `pg_restore` installed.
