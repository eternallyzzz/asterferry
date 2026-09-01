# Docker Compose deployment

This Compose file runs the Controller, Gateway and Agent as independent
processes. Only the Controller mounts the SQLite/CA/TLS directory. Nodes mount
an enrolled bootstrap JSON and an encrypted state directory; they do not mount
YAML business configuration or expose a management API.

## Prepare

Build the image and initialize the Controller once on the host:

```powershell
asterferry controller init --dir ./controller `
  --grpc-advertise controller.example.com:9443 `
  --release-version 1.0.0
docker compose -f deploy/docker/compose.yaml build
```

Create a Gateway and Agent Node in the Controller, mint role-bound enrollment
tokens, and enroll their bootstrap files. By default Compose expects:

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

The Controller health check is anonymous `/healthz`; readiness and metrics are
authenticated API endpoints. Gateway and Agent reconnect to the Controller and
continue their last encrypted snapshot while it is unavailable.

Stop the stack with `docker compose ... down`. Back up the Controller before
upgrades using `asterferry controller backup`; restore into a fresh data
directory with `asterferry controller restore`.
