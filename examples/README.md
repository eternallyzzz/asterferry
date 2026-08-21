# AsterFerry v3 configuration examples

The configuration uses two fixed roles: `gateway` (public entry point) and
`agent` (internal node). The old `tunnels` section is now named `reverse`, and
the public port field is `gateway_port`.

## 1. Generate a deployment identifier

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

The Agent server name must match a SAN on the Gateway certificate. Restrict
certificate and private-key permissions to the service account.

## 3. Create an Agent token

```powershell
openssl rand -hex 32 | Set-Content -NoNewline edge-a.token
```

Use the same token file contents on the Gateway and the corresponding Agent.
Use a separate token for every Agent. Never commit tokens, private keys, or real
certificates to the repository.

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
- Agent: `agent.server`, `agent.tls.*`, and `agent.token_file`
- Agent: point `agent.reverse[].local` at internal services
- Agent: configure local proxy listeners under `agent.proxy.inbounds`; the
  default route is Gateway. HTTP Host and TCP TLS SNI observation is enabled by
  default under `agent.proxy.sniff`; it never changes route selection.
- Configure structured logs under `logging`. JSON, bounded INFO/DEBUG sampling,
  and privacy-preserving domain hashes are the production defaults. Use the
  documented `ASTERFERRY_*` environment variables for deployment-specific
  overrides.
- `obfuscation.transport.mode` defaults to `camouflage`. It masks QUIC UDP
  datagrams and shapes only handshake packets. `max_wire_packet_bytes` bounds
  each shaped fragment; normal QUIC data keeps native packet coalescing.
  `standard` is an explicit raw QUIC mode for migration and trusted networks;
  the two modes do not fall back automatically.

Proxy listeners bind to loopback by default. If a listener binds to another
address, credentials are required.

## 5. Validate and start

```powershell
asterferry validate -c examples/gateway.yaml
asterferry validate -c examples/agent.yaml
asterferry gateway -c examples/gateway.yaml
asterferry agent -c examples/agent.yaml
```

The Gateway firewall must allow UDP `4433` and the configured reverse TCP/UDP
ports. Management endpoints bind to loopback by default.
