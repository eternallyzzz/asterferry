# Docker deployment

The image contains the same `asterferry gateway`, `asterferry agent`,
`asterferry validate`, `asterferry doctor`, and `asterferry status` commands as
a native installation. The image is built from a static Go 1.26.7 binary and
runs as the non-root `asterferry` user.

## Build the image

From the repository root:

```sh
docker build -t asterferry:local .
```

The default image target is Linux. BuildKit can produce multi-platform images:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t asterferry:local .
```

Tagged releases are published as `ghcr.io/eternallyzzz/asterferry:<version>`
and expose their immutable digest in the release manifest. Verify the
checksum and signature material from the GitHub Release before using a
production image, then prefer the digest form:

```sh
docker pull ghcr.io/eternallyzzz/asterferry:0.1.0
docker inspect ghcr.io/eternallyzzz/asterferry:0.1.0 --format '{{index .RepoDigests 0}}'
```

## Compose layout

`compose.yaml` starts one Gateway and one Agent. It can consume the same bundle
created by `asterferry init`:

```text
asterferry/
  config/
    gateway.yaml
    agent.yaml
  secrets/
    gateway/
      server.crt
      server.key
      agents-ca.crt
      edge-a.token
      management-admin.token
      management-viewer.token
      obfs.key
      obfs.key.previous   # optional during key rotation
    agent/
      gateway-ca.crt
      edge-a.crt
      edge-a.key
      edge-a.token
      management-admin.token
      management-viewer.token
      obfs.key
      obfs.key.previous   # optional during key rotation
```

Generate a Docker-ready development bundle from the repository root. The
Gateway hostname is included in the generated development certificate and the
Agent uses the Compose service name:

```sh
asterferry init ./asterferry --profile dev --gateway-host gateway
# Paths in compose.yaml are resolved from deploy/docker; use an absolute path
# or ../../asterferry when running this command from the repository root.
export ASTERFERRY_BUNDLE_DIR=../../asterferry
# Linux hosts: init creates private files owned by the current user. Match the
# non-root container identity so 0600 secrets stay private and readable.
export ASTERFERRY_CONTAINER_UID="$(id -u)"
export ASTERFERRY_CONTAINER_GID="$(id -g)"
docker compose -f deploy/docker/compose.yaml config
docker compose -f deploy/docker/compose.yaml up -d --build
docker compose -f deploy/docker/compose.yaml ps
```

`ASTERFERRY_BUNDLE_DIR` defaults to `../../asterferry` relative to the Compose
file, matching the repository-root quick start. Set an absolute path when the
bundle lives elsewhere. The Gateway and Agent must share
the same current `obfs.key`; keep the previous key in `obfs.key.previous` on
both sides only while rotating. Development certificates are not production
credentials.

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

The final image is a digest-pinned distroless runtime with no shell, wget, or
package manager. Compose healthchecks invoke the embedded
`asterferry healthcheck` command directly.

The Compose healthchecks call the default loopback HTTP management endpoints.
They do not publish management ports to the host. If a configuration enables
management TLS, change the corresponding healthcheck URL to `https://...` and
add `--insecure-tls` only when the probe is strictly against `127.0.0.1` and
the container does not trust the management CA.

The image also contains the optional embedded Dashboard at `/dashboard/` on
each service's management listener. Set `management.web.enabled: false` to omit
the page while retaining the protected API. The default Compose file does not
publish management listeners and mounts configuration read-only, so the
protected configuration API reports read-only and changes must be made in the
host file. Use `docker compose exec gateway asterferry status --config
/etc/asterferry/config/gateway.yaml` for a container-side check, or publish a narrowly
scoped local port/reverse proxy if a browser view is required. A public bind
requires built-in management TLS and the management Viewer token. Actions and
configuration writes require the Admin token through the CLI or protected API.

## Release upgrades and rollback

Set `ASTERFERRY_IMAGE` to the release digest recorded in
`release-manifest.json`, verify the configuration first, and recreate both
roles together because the v6 protocol is not compatible with v5:

```sh
export ASTERFERRY_IMAGE=ghcr.io/eternallyzzz/asterferry@sha256:<image-digest>
asterferry validate --config ./config/gateway.yaml
asterferry doctor --config ./config/gateway.yaml --skip-ports
asterferry validate --config ./config/agent.yaml
asterferry doctor --config ./config/agent.yaml --skip-ports
docker compose pull
docker compose up -d
docker compose ps
```

Keep the previous digest until the new `/readyz` checks and application logs
are healthy. To roll back, restore the previous `ASTERFERRY_IMAGE` digest and
run `docker compose up -d` again. The Compose stop window remains 35 seconds
for the default 30-second application drain.
