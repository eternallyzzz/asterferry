# AsterFerry

AsterFerry is a lightweight private-network relay. A public `gateway` and an
internal `agent` connect over TLS 1.3 and QUIC to provide local SOCKS5/HTTP
proxying and TCP/UDP reverse mappings.

The current configuration and wire protocol are v2, with strict role
separation:

- `gateway`: public entry point, Agent mTLS/token authentication, reverse port
  mappings, and policy-controlled egress proxying.
- `agent`: actively connects to the Gateway, exposes local proxy listeners, and
  registers internal services as reverse mappings.

```text
asterferry gateway -c gateway.yaml
asterferry agent   -c agent.yaml
asterferry validate -c agent.yaml
```

## Traffic model

```text
local application -> agent.proxy -> direct or gateway egress -> destination
public user       -> gateway_port -> agent.reverse[].local
```

Proxy payloads are carried inside encrypted QUIC streams using AsterFerry relay
records. The `balanced` profile adds bounded random padding to reduce fixed-size
fingerprints; it does not impersonate HTTP/3, WebSocket, or another application
protocol.

## Configuration examples

Copyable templates are available in [examples/README.md](examples/README.md),
[examples/gateway.yaml](examples/gateway.yaml), and
[examples/agent.yaml](examples/agent.yaml). Before production deployment,
replace the certificates, client CA, token, deployment-specific ALPN, and all
sample addresses.

The Gateway firewall must allow the QUIC UDP port and the configured reverse
TCP/UDP ports. Management endpoints bind to loopback by default: Gateway uses
`127.0.0.1:9090` and Agent uses `127.0.0.1:9091`.

## Security boundaries

- Production deployments require a Gateway server certificate, Agent client
  certificates, and a per-Agent token.
- Gateway egress proxying applies per-Agent, protocol, port, and destination-IP
  ACLs. Private, loopback, link-local, and metadata addresses are denied by
  default.
- Management endpoints must not be exposed to the public network.
- Logs do not record tokens, private keys, or proxy payloads.

## Verification

```powershell
asterferry validate -c examples/gateway.yaml
asterferry validate -c examples/agent.yaml
go test ./...
go vet ./...
```

The historical `myproxy` and `myfrp` projects were design references only;
their v1 configuration and wire protocol are not compatible with AsterFerry v2.
