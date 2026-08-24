# Docker deployment

The image contains the same `asterferry gateway`, `asterferry agent`, and
`asterferry validate` commands as a native installation. The image is built
from a static Go 1.26.5 binary and runs as the non-root `asterferry` user.

## Build the image

From the repository root:

```sh
docker build -t asterferry:local .
```

The default image target is Linux. BuildKit can produce multi-platform images:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t asterferry:local .
```

## Compose layout

`compose.yaml` starts one Gateway and one Agent. It expects this local layout:

```text
deploy/docker/
  compose.yaml
  config/
    gateway.yaml
    agent.yaml
  secrets/
    gateway/
      server.crt
      server.key
      agents-ca.crt
      edge-a.token
      obfs.key
      obfs.key.previous   # optional during key rotation
    agent/
      gateway-ca.crt
      edge-a.crt
      edge-a.key
      edge-a.token
      obfs.key
      obfs.key.previous   # optional during key rotation
```

Create the directories and copy the example configurations before starting.
Change certificate, token, ALPN, and `obfuscation.transport.key_file` paths in
the copied configuration to use `/etc/asterferry/secrets/...`. The Gateway and
Agent must share the same current `obfs.key`; keep the previous key in
`obfs.key.previous` on both sides only while rotating. The Agent server address should be
`gateway:4433` inside the Compose network.

```sh
cd deploy/docker
docker compose config
docker compose up -d --build
docker compose ps
```

Both services use a 35-second Compose stop window for the default 30-second
application drain. Set `shutdown.grace_period_seconds` in each configuration
and increase the external stop window when using a longer period. The first
termination signal marks the service unready, rejects new traffic, and allows
admitted streams to finish; a second signal or an expired window closes the
remaining sessions.

The Gateway publishes QUIC UDP `4433`, reverse TCP `28080`, and reverse UDP
`21003` by default. Keep these mappings aligned with the configured
`gateway_port` values. Override host-side ports with
`ASTERFERRY_QUIC_PORT`, `ASTERFERRY_REVERSE_WEB_PORT`, and
`ASTERFERRY_REVERSE_DNS_PORT`.

The Agent proxy listeners are not published by default. If a proxy must be
reachable outside the container, bind its inbound to `0.0.0.0`, keep
credentials enabled, and explicitly add a Compose port mapping.

Structured logging is configured in YAML under `logging`. Deployment-specific
overrides can be passed as container environment variables such as
`ASTERFERRY_LOG_LEVEL`, `ASTERFERRY_LOG_FORMAT`, and
`ASTERFERRY_LOG_SAMPLING_ENABLED`; environment values take precedence over
YAML and invalid values stop startup.

## Security properties

Both services use a read-only root filesystem, drop all Linux capabilities, and
disable privilege escalation. Certificates, private keys, and tokens are
mounted read-only and are never copied into the image.

The Compose healthchecks call the loopback-only management endpoints. They do
not publish management ports to the host.
