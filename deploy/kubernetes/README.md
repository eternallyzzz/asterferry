# Kubernetes deployment

AsterFerry is packaged as one role-selectable Helm Chart. Install it twice for
a normal deployment: once as a `gateway` release and once as an `agent`
release. The chart does not create credential Secrets; certificates, private
keys, Agent tokens, and management tokens must be created out of band.

## Prepare configuration and Secrets

The chart expects a ConfigMap key named `config.yaml` and an existing Secret.
The Secret is mounted at `/etc/asterferry/secrets`. Use that path in the
configuration file, for example:

```yaml
gateway:
  tls:
    cert_file: /etc/asterferry/secrets/server.crt
    key_file: /etc/asterferry/secrets/server.key
    client_ca_file: /etc/asterferry/secrets/agents-ca.crt
management:
  # Production defaults to no embedded page; set true only for an internal
  # administrative port-forward.
  web:
    enabled: false
  auth:
    admin_token_file: /etc/asterferry/secrets/management-admin.token
    viewer_token_file: /etc/asterferry/secrets/management-viewer.token
  # Non-loopback management.listen requires both TLS files.
  # tls:
  #   cert_file: /etc/asterferry/secrets/management.crt
  #   key_file: /etc/asterferry/secrets/management.key
  #   ca_file: /etc/asterferry/secrets/management-ca.crt
obfuscation:
  transport:
    mode: camouflage
    key_file: /etc/asterferry/secrets/obfs.key
    # Uncomment during rotation after mounting obfs.key.previous.
    # previous_key_file: /etc/asterferry/secrets/obfs.key.previous
```

Create resources in the target namespace:

```sh
kubectl create namespace asterferry

kubectl -n asterferry create configmap asterferry-gateway-config \
  --from-file=config.yaml=./config/gateway.yaml

kubectl -n asterferry create secret generic asterferry-gateway-secrets \
  --from-file=server.crt=./secrets/gateway/server.crt \
  --from-file=server.key=./secrets/gateway/server.key \
  --from-file=agents-ca.crt=./secrets/gateway/agents-ca.crt \
  --from-file=edge-a.token=./secrets/gateway/edge-a.token \
  --from-file=management-admin.token=./secrets/gateway/management-admin.token \
  --from-file=management-viewer.token=./secrets/gateway/management-viewer.token \
  --from-file=obfs.key=./secrets/gateway/obfs.key
```

Create corresponding Agent ConfigMap and Secret resources with the Agent CA,
client certificate (URI SAN `urn:asterferry:agent:<agent-id>`), client key,
Agent token, management Admin/Viewer tokens, and the same `obfs.key` (plus the
optional `obfs.key.previous` during rotation). Never commit these files or put
their contents into `values.yaml`.

Before applying a ConfigMap and Secret, run the local preflight checks against
the same configuration and secret paths:

```sh
asterferry validate --config ./config/gateway.yaml
asterferry doctor --config ./config/gateway.yaml --skip-ports
asterferry validate --config ./config/agent.yaml
asterferry doctor --config ./config/agent.yaml --skip-ports
```

After rollout, use the Kubernetes readiness probe for availability and query
`asterferry status --config ...` from an administrative context when the
loopback management endpoint is reachable. Use the Viewer token for status and
the Admin token for actions or configuration writes.

Each role also serves the embedded Dashboard from its management listener.
When the pod runtime permits an administrative port-forward, use
`kubectl port-forward` to the role's management port and open `/dashboard/`;
otherwise keep using the CLI or an internal authenticated proxy. The chart
does not publish management ports through a Service, and the Dashboard token
is entered in memory rather than placed in a URL. The ConfigMap volume is
read-only, so the protected configuration API is read-only; update the ConfigMap
and roll out the Deployment when changing configuration.

## Install a tagged release from OCI

Release Charts are available from GHCR. Authenticate with a token that can
read packages, then install the same chart version for both roles. Pin the
image digest from the release `release-manifest.json` instead of relying on a
mutable tag:

```sh
helm registry login ghcr.io

helm upgrade --install asterferry-gateway \
  oci://ghcr.io/eternallyzzz/charts/asterferry \
  --version 0.1.0 \
  --namespace asterferry \
  --set role=gateway \
  --set config.existingConfigMap=asterferry-gateway-config \
  --set secret.existingSecret=asterferry-gateway-secrets \
  --set image.digest=sha256:<image-digest> \
  --atomic --wait --timeout 5m
```

Install the Agent with the same `--version` and image digest, changing only
the role-specific ConfigMap and Secret names. Verify the published checksum,
SBOM, and GitHub build attestation before promoting the release to a cluster.

## Install Gateway

The default Gateway Service is a `LoadBalancer` and publishes QUIC UDP `4433`
plus the example reverse TCP/UDP ports. Update
`gateway.service.ports` whenever the configuration uses different
`gateway_port` values.

```sh
helm upgrade --install asterferry-gateway ./deploy/helm/asterferry \
  --namespace asterferry \
  --set role=gateway \
  --set config.existingConfigMap=asterferry-gateway-config \
  --set secret.existingSecret=asterferry-gateway-secrets
```

For a cloud LoadBalancer, add provider-specific annotations with
`gateway.service.annotations`. Use `gateway.service.type=NodePort` when the
cluster does not provide LoadBalancer integration.

## Install Agent

An Agent normally needs no Service because it initiates the Gateway connection.
Its local reverse endpoints can target Kubernetes Services by DNS name, for
example `web.default.svc.cluster.local:8080`.

```sh
helm upgrade --install asterferry-agent ./deploy/helm/asterferry \
  --namespace asterferry \
  --set role=agent \
  --set config.existingConfigMap=asterferry-agent-config \
  --set secret.existingSecret=asterferry-agent-secrets
```

The Agent Service is disabled by default. If a proxy listener must be exposed,
bind that inbound to `0.0.0.0`, keep credentials enabled, and set
`agent.service.enabled=true` with only the intended ports.

Use the chart's `env` value to provide runtime log overrides without changing
the ConfigMap, for example:

```yaml
env:
  - name: ASTERFERRY_LOG_LEVEL
    value: info
  - name: ASTERFERRY_LOG_FORMAT
    value: json
```

The same `ASTERFERRY_*` variables can control sampling and Agent sniffing.

## Minikube smoke test

Minikube can exercise the chart with the local image and an in-cluster
`NodePort` Gateway. Load the image into the Minikube node and override the
image settings for the test:

```sh
minikube image load asterferry:local

helm upgrade --install asterferry-gateway ./deploy/helm/asterferry \
  --namespace asterferry --create-namespace \
  --set role=gateway \
  --set gateway.service.type=NodePort \
  --set image.repository=asterferry \
  --set image.tag=local \
  --set image.pullPolicy=Never \
  --set secret.existingSecret=asterferry-gateway-secrets \
  --set-file config.content=./config/gateway.yaml
```

Install the Agent release with the same image overrides and its own ConfigMap
or `config.content`. Point `agent.server` at the Gateway Service DNS name, for
example `asterferry-gateway:4433`, then verify both workloads:

```sh
kubectl -n asterferry rollout status deployment/asterferry-gateway
kubectl -n asterferry rollout status deployment/asterferry-agent
kubectl -n asterferry get pods,svc
```

Minikube does not allocate a cloud LoadBalancer address by itself. Use the
`NodePort` override above for an in-cluster smoke test, or run `minikube
tunnel` in another terminal when testing the default `LoadBalancer` Service.

## Inline configuration

For controlled environments, `config.content` can generate the ConfigMap from
Helm values. Existing ConfigMap references are preferred for large files and
for separating configuration ownership from the release:

```sh
helm upgrade --install asterferry-gateway ./deploy/helm/asterferry \
  --namespace asterferry \
  --set role=gateway \
  --set secret.existingSecret=asterferry-gateway-secrets \
  --set-file config.content=./config/gateway.yaml
```

## Security and lifecycle

The chart runs as UID/GID 10001, drops all capabilities, disables privilege
escalation, uses a read-only root filesystem, disables ServiceAccount token
automounting, and uses the RuntimeDefault seccomp profile. Management endpoints
are loopback-only by default. A non-loopback listener requires built-in TLS;
mount the management certificate and key through the existing Secret. Probes
execute the embedded `asterferry healthcheck` command inside the tool-free
container against `/healthz` and `/readyz`. Set `probes.scheme=https` when
management TLS is enabled; set `probes.insecureTLS=true` only for a
loopback-local probe using a certificate the container cannot verify. On
the first termination signal, AsterFerry marks
`/readyz` unavailable while keeping `/healthz` healthy, stops new admissions,
and drains existing streams for `shutdown.grace_period_seconds` (30 seconds by
default). The chart's 35-second `terminationGracePeriodSeconds` leaves a small
supervisor buffer; increase it when configuring a longer application drain.
The second signal or a deadline forces connection closure.

Use `helm lint` and `helm template` before installation. Rotate credentials by
updating the Secret and restarting the release after validating the updated
configuration. The default single-replica Gateway topology is intentional;
multiple replicas require a separate shared-port and session-coordination
design.

For a production upgrade, run `validate` and `doctor` against the exact
configuration and secrets, then use the OCI command with the new version and
`--atomic --wait`. Upgrade Gateway and Agent together because v5 is a breaking
protocol generation. Check `kubectl rollout status` and the readiness probe
before removing the previous release. If the new release is unhealthy, inspect
`helm history` and roll back both roles with:

```sh
helm rollback asterferry-gateway <revision> --namespace asterferry --wait
helm rollback asterferry-agent <revision> --namespace asterferry --wait
```

Only roll back to a release with a compatible configuration and the same v5
protocol generation; v4 is not a valid rollback target.

## Cluster readiness boundary

Both roles accept an optional `cluster.node_id` identity field. The runtime
reports it through the management status endpoint and includes it in session
logs, but this field does not enable active-active behavior. Do not increase
the Gateway `replicaCount` for production yet: QUIC sessions, reverse mapping
listeners, and their owner state remain local to one process. A future
Kubernetes cluster mode must add Lease-based ownership, L4 affinity, and
reverse-port owner routing as one coordinated feature; it must not use the
Kubernetes etcd datastore directly from the application.
