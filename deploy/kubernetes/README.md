# Kubernetes deployment

AsterFerry is deployed as separate Controller and data-plane Node releases.
The Controller owns the database, PKI and API; Nodes receive typed snapshots
and expose no management HTTP service. Gateway or Agent is selected as the
Node's behavior spec after enrollment.

## Controller

Create a namespace and install the single-replica Controller chart. The chart
creates a PVC for the default SQLite database, CA, TLS identity and master key:

```sh
kubectl create namespace asterferry
helm upgrade --install asterferry-controller ./deploy/helm/asterferry-controller \
  --namespace asterferry --create-namespace \
  --set image.repository=ghcr.io/eternallyzzz/asterferry
```

After the `v2.0.0` release is published, the same installation can use the
immutable OCI chart instead of the checked-out path:

```sh
helm upgrade --install asterferry-controller \
  oci://ghcr.io/eternallyzzz/charts/asterferry-controller \
  --version 2.0.0 --namespace asterferry --create-namespace
```

Run `controller init` once against the mounted data directory (for example in
a maintenance Job) before starting the StatefulSet. Expose HTTPS `8443` and
mTLS gRPC `9443` through an internal or external Service according to your
deployment policy. The normal Service selects only the ready leader.

For active/standby HA, use PostgreSQL and prepare one existing Secret shared by
both Pods. It must contain `controller.json`, `master.key`, `ca.key`,
`ca.crt`, `controller.key` and `controller.crt`; the config must
set `database_driver=postgres`, the shared `database_url` and
`high_availability=true`, with paths under `/etc/asterferry`:

```sh
kubectl -n asterferry create secret generic asterferry-controller-identity \
  --from-file=controller.json=./controller-ha/controller.json \
  --from-file=master.key=./controller-ha/master.key \
  --from-file=ca.key=./controller-ha/ca.key \
  --from-file=ca.crt=./controller-ha/ca.crt \
  --from-file=controller.key=./controller-ha/controller.key \
  --from-file=controller.crt=./controller-ha/controller.crt
helm upgrade --install asterferry-controller ./deploy/helm/asterferry-controller \
  --namespace asterferry \
  --set controller.replicas=2 \
  --set controller.highAvailability.enabled=true \
  --set controller.highAvailability.existingSecret=asterferry-controller-identity
```

The HA chart does not create a PVC. Its headless Service is for stable Pod
identity; the normal Service and an external load balancer route HTTPS/gRPC to
the ready leader. Existing gRPC streams break during takeover and Nodes
reconnect automatically. SQLite rejects HA mode and remains single-replica.

For a multi-Node or production deployment, use PostgreSQL instead of putting
the Controller database on the PVC. Run `controller init` with
`--database-driver postgres --database-url 'postgres://...'` against the
managed database, and place the resulting `controller.json` plus the CA/TLS
and master-key files in the Controller volume/Secret according to your secret
management policy. PostgreSQL is required for the two-replica HA mode.

The HTTPS endpoint policy is deliberate: `/healthz` and `/readyz` are
anonymous; `/readyz` returns only a boolean readiness result; management
`/metrics` requires an authenticated Viewer, Operator or Admin; and
`/openapi.yaml` plus `/api/v1/openapi.yaml` are anonymous for client
discovery. The Controller chart keeps a separate plain-HTTP metrics
listener disabled by default. Set `metrics.enabled=true`, choose the listen
address, and apply a namespace/network policy before exposing its Service;
this is the standard unauthenticated Prometheus scrape surface. Browser Cookie
sessions are durable hashed records with a 12-hour lifetime and are shared by
the PostgreSQL active/standby pair. Logout, password changes, expiry and
backup restore invalidate them; use API tokens for automated clients.

## Node releases

For ordinary hosts, the Dashboard one-click Node flow is recommended: it
returns a short-lived node-bound installer command for the selected platform.
Kubernetes deployments can keep using the chart-native path below. Create a
generic enrollment token, enroll the Node, put the resulting bootstrap JSON in
a Secret whose key is `bootstrap.json`, and install one release per Node:

```sh
helm upgrade --install gw-east ./deploy/helm/asterferry-node \
  --namespace asterferry \
  --set bootstrapSecret=gw-east-bootstrap

helm upgrade --install agent-east ./deploy/helm/asterferry-node \
  --namespace asterferry \
  --set bootstrapSecret=agent-east-bootstrap
```

For the published chart, use
`oci://ghcr.io/eternallyzzz/charts/asterferry-node --version 2.0.0` in place
of the local chart path. Set `image.digest` from the release manifest when a
digest-pinned image deployment is required; it takes precedence over the
chart's tag.

There is no role switch in the chart. Both releases run the same generic Node
command; select Gateway or Agent behavior in the Dashboard after the Node
connects.

GeoIP routing is disabled by default. To enable it, supply a reviewed,
versioned MaxMind-compatible database in an operator-owned ConfigMap and set
`geoip.enabled=true` and `geoip.existingConfigMap=<name>`. The chart mounts it
read-only; the database is never downloaded by the Node.

The node chart copies the Secret into its private state PVC so certificate
rotation can atomically update the bootstrap file. Configured Gateway data
traffic uses AFDP/2 over QUIC; Nodes configured as Agents normally need no
Service because they initiate both Controller and Gateway connections. Each
Gateway must have an independent reachable public endpoint.

## Verification

```sh
helm lint deploy/helm/asterferry-controller deploy/helm/asterferry-node
helm template controller deploy/helm/asterferry-controller --namespace asterferry
helm template node deploy/helm/asterferry-node --namespace asterferry
kubectl -n asterferry rollout status statefulset/asterferry-controller
```

Keep bootstrap Secrets and the Controller PVC (or PostgreSQL backup) backed up
using the Controller's `backup` command. PostgreSQL backup/restore requires
`pg_dump`/`pg_restore` on the machine running the CLI. Stop a node or Controller independently when testing the
offline behavior: an applied node snapshot continues serving traffic while
the Controller is unavailable and reconciles automatically after reconnect.
