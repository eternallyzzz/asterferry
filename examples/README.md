# AsterFerry v6 configuration examples

The configuration uses two fixed roles: `gateway` (public entry point) and
`agent` (internal node). The old `tunnels` section is now named `reverse`, and
the public port field is `gateway_port`. Reverse mappings bind to loopback by
default; set `gateway_bind` explicitly to a public interface when exposure is
intentional.

The examples use protocol/configuration version 6. Gateway and Agent must be
upgraded together; v5 and older configurations and connections are rejected.

For the fastest local test, generate a complete pair instead of following the
manual certificate steps below:

```powershell
asterferry init ./asterferry --profile dev
asterferry doctor ./asterferry
asterferry up ./asterferry
```

The generated development certificates are self-signed and must not be used
for production. Use `--profile prod` to generate private keys and CSRs for an
external PKI; the resulting configuration is intentionally not startable
until the signed certificates and CA files are installed.

The paths in generated configurations are relative to each configuration file,
so the whole bundle can be moved between a Windows checkout and WSL.

## 1. Generate a deployment identifier (manual production setup)

Generate an ALPN identifier used only by the Gateway and Agent in one
deployment:

```powershell
$alpn = -join ((48..57) + (97..122) | Get-Random -Count 16 | ForEach-Object {[char]$_})
$alpn
```

Set the same value in `transport.alpn` on both sides. Do not reuse the value
from the repository example.

## 2. Prepare TLS certificates

The Gateway uses a server certificate and an Agent client CA. Each Agent uses
its own client certificate. Self-signed certificates are suitable for testing;
production deployments should use an internal CA or an established PKI.

Every Agent client certificate must contain a URI SAN exactly matching
`urn:asterferry:agent:<agent-id>`; the Common Name is not used for identity
binding. Regenerate certificates when changing an Agent ID.

The Agent server name must match a SAN on the Gateway certificate. Restrict
certificate and private-key permissions to the service account.

## 3. Create an Agent token

```powershell
openssl rand -hex 32 | Set-Content -NoNewline edge-a.token
openssl rand -hex 32 | Set-Content -NoNewline management-admin.token
openssl rand -hex 32 | Set-Content -NoNewline management-viewer.token
```

Use the same token file contents on the Gateway and the corresponding Agent.
Use a separate token for every Agent. Never commit tokens, private keys, or real
certificates to the repository.

The management Admin and Viewer tokens are independent from Agent tokens. Set
`management.auth.admin_token_file` and `management.auth.viewer_token_file` on
both roles. Health probes remain anonymous; `/metrics` and `/v1/status` require
the Viewer token, while actions and configuration writes require the Admin
token.

Create the same independent transport-obfuscation key on the Gateway and
Agent. It is not a replacement for mTLS or the Agent token:

```powershell
openssl rand -hex 32 | Set-Content -NoNewline obfs.key
```

Mount it as `/etc/asterferry/obfs.key` with read-only permissions. During key
rotation, place the old key at `obfs.key.previous`; outbound packets use the
current key while inbound packets accept both keys for the overlap window.

## 4. Edit the configuration

- Gateway: `gateway.listen`, TLS files, and `gateway.agents[].token_file`
- Gateway: define independent `reverse` and `egress` ACLs for each Agent
- Gateway: special-use/private destinations are denied by default; use a
  narrow `egress.allow_special_cidrs` entry only for an explicitly approved
  development or internal destination
- Agent: `agent.server`, `agent.tls.*`, and `agent.token_file`
- Agent: point `agent.reverse[].local` at internal services
- Agent: configure local proxy listeners under `agent.proxy.inbounds`; the
  default route is Gateway. HTTP Host and TCP TLS SNI observation is enabled by
  default under `agent.proxy.sniff`; it never changes route selection.
- Configure structured logs under `logging`. JSON, bounded INFO/DEBUG sampling,
  and privacy-preserving domain hashes are the production defaults. Use the
  documented `ASTERFERRY_*` environment variables for deployment-specific
  overrides.
- Configure `shutdown.grace_period_seconds` for the bounded drain after
  `SIGTERM`/`SIGINT` (30 seconds by default). The first signal rejects new
  traffic while admitted streams finish; a second signal forces close. Config
  changes take effect after validation and restart, not through SIGHUP.
- The embedded Dashboard is enabled by default for legacy/manual
  configurations; generated production bundles set it to `false`. Set
  `management.web.enabled: false` to retain the protected API without serving
  the page. A non-loopback management listener requires
  `management.tls.cert_file` and `management.tls.key_file`; `ca_file` is
  optional trust material for the CLI `status` command.
- The protected configuration API only accepts non-secret field changes. It
  preserves redacted passwords, validates and previews a diff, writes a `.bak`
  backup, and requests a graceful supervisor restart. To change an inbound
  password, edit the configuration file directly and restart the role; the
  API reports secret fields as read-only. Read-only container and ConfigMap
  mounts remain read-only.
- `cluster.node_id` is optional identity metadata for future Kubernetes
  coordination. It does not enable multiple Gateway replicas or external
  Redis/etcd coordination.
- `obfuscation.transport.mode` defaults to `camouflage`. It masks QUIC UDP
  datagrams and shapes only handshake packets. `max_wire_packet_bytes` bounds
  each shaped fragment; normal QUIC data keeps native packet coalescing.
  `standard` is an explicit raw QUIC mode for migration and trusted networks;
  the two modes do not fall back automatically.

Proxy listeners bind to loopback by default. If a listener binds to another
address, credentials are required.

## 5. Validate, diagnose and start

```powershell
asterferry validate --config examples/gateway.yaml
asterferry validate --config examples/agent.yaml
asterferry doctor --config examples/gateway.yaml
asterferry doctor --config examples/agent.yaml
asterferry gateway --config examples/gateway.yaml
asterferry agent --config examples/agent.yaml
```

The Gateway firewall must allow UDP `4433` and the configured reverse TCP/UDP
ports. Management endpoints bind to loopback by default.
