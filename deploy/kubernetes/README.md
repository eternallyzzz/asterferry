# Kubernetes deployment

AsterFerry is packaged as one role-selectable Helm Chart. Install it twice for
a normal deployment: once as a `gateway` release and once as an `agent`
release. The chart does not create credential Secrets; certificates, private
keys, and tokens must be created out of band.

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
  --from-file=obfs.key=./secrets/gateway/obfs.key
```

Create corresponding Agent ConfigMap and Secret resources with the Agent CA,
client certificate, client key, token, and the same `obfs.key` (plus the
optional `obfs.key.previous` during rotation). Never commit these files or put
their contents into `values.yaml`.

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
remain loopback-only; probes execute `wget` inside the container against
`/healthz` and `/readyz`.

Use `helm lint` and `helm template` before installation. Rotate credentials by
updating the Secret and restarting the release after validating the updated
configuration. The default single-replica Gateway topology is intentional;
multiple replicas require a separate shared-port and session-coordination
design.
