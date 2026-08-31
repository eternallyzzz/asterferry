# Kubernetes deployment

AsterFerry is deployed as separate Controller and data-plane node releases.
The Controller owns the database, PKI and API; Gateway and Agent pods receive
typed snapshots and expose no management HTTP service.

## Controller

Create a namespace and install the single-replica Controller chart. The chart
creates a PVC for the SQLite database, CA, TLS identity and master key:

```sh
kubectl create namespace asterferry
helm upgrade --install asterferry-controller ./deploy/helm/asterferry-controller \
  --namespace asterferry --create-namespace \
  --set image.repository=ghcr.io/eternallyzzz/asterferry
```

Run `controller init` once against the mounted data directory (for example in
a maintenance Job) before starting the StatefulSet. Expose HTTPS `8443` and
mTLS gRPC `9443` through an internal or external Service according to your
deployment policy. Controller HA is outside this release.

## Gateway and Agent nodes

Register nodes and create role-bound enrollment tokens in the Controller. Put
the resulting bootstrap JSON in a Secret whose key is `bootstrap.json`, then
install one node release per node:

```sh
helm upgrade --install gw-east ./deploy/helm/asterferry-node \
  --namespace asterferry \
  --set role=gateway \
  --set bootstrapSecret=gw-east-bootstrap

helm upgrade --install agent-east ./deploy/helm/asterferry-node \
  --namespace asterferry \
  --set role=agent \
  --set bootstrapSecret=agent-east-bootstrap
```

The node chart copies the Secret into its private state PVC so certificate
rotation can atomically update the bootstrap file. Gateway data traffic uses
AFDP/2 over QUIC; Agents normally need no Service because they initiate both
Controller and Gateway connections. Each Gateway must have an independent
reachable public endpoint.

## Verification

```sh
helm lint deploy/helm/asterferry-controller deploy/helm/asterferry-node
helm template controller deploy/helm/asterferry-controller --namespace asterferry
helm template gateway deploy/helm/asterferry-node --namespace asterferry --set role=gateway
kubectl -n asterferry rollout status statefulset/asterferry-controller
```

Keep bootstrap Secrets and the Controller PVC backed up using the Controller's
`backup` command. Stop a node or Controller independently when testing the
offline behavior: an applied node snapshot continues serving traffic while
the Controller is unavailable and reconciles automatically after reconnect.
