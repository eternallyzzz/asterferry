# AsterFerry

AsterFerry is a lightweight private-network relay. A public `gateway` and an
internal `agent` connect over TLS 1.3 and QUIC to provide local SOCKS5/HTTP
proxying and TCP/UDP reverse mappings.

The current configuration and wire protocol are v4, with strict role
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

## Runtime architecture

Configuration is validated and resolved once at startup into role-specific
runtime options. `Gateway` and `Agent` are composition roots; session,
mapping, proxy, routing, security, logging, and management concerns remain
separate components.

Local proxy handlers use a protocol-neutral `Outbound` boundary for direct or
gateway paths. QUIC is hidden behind the transport package's
`Listener`/`Session`/`Stream` interfaces, so the role runtimes do not depend on
quic-go types. The wire framing and relay record layers remain independent of
both proxy protocols and deployment configuration.

At runtime, the Agent composes a `ProxyEngine` and a reconnecting session
manager. The Gateway composes a session registry, mapping manager, and egress
proxy service. These components own their own shutdown and concurrency state;
the role objects only assemble them and expose status. Both roles share a
`running -> draining -> stopped` lifecycle gate so admission and active-work
accounting cannot diverge between the two data planes.

## Graceful shutdown

`shutdown.grace_period_seconds` controls the bounded drain after the first
`SIGTERM` or `SIGINT` (default 30 seconds, maximum 3600 seconds). During drain,
`/readyz` returns 503 while `/healthz` remains healthy; new sessions, streams,
proxy accepts, reconnects, and reverse mappings are rejected, while already
admitted traffic is allowed to finish. When the deadline expires, remaining
work is closed forcefully and the process exits as an intentional operational
shutdown. A second termination signal skips the wait and closes immediately.

Configuration changes are intentionally validate-then-restart; SIGHUP does not
reload configuration. Keep the supervisor's termination window longer than the
configured drain period (the bundled Docker, systemd, and Helm defaults use a
35-second window for the 30-second application default).

Proxy payloads are carried inside encrypted QUIC streams using AsterFerry relay
records. The `balanced` profile adds bounded random padding to reduce fixed-size
fingerprints; it does not impersonate HTTP/3, WebSocket, or another application
protocol.

The v4 default also applies a versioned outer UDP camouflage layer around QUIC.
It uses an independent key file, random salts, keyed packet tags, and bounded
handshake shaping. This hides QUIC packet bytes from passive DPI and silently
drops unauthenticated probes. Normal QUIC data keeps native packet coalescing;
the configured wire limit applies to shaped handshake fragments.
`obfuscation.transport.mode: standard` is an explicit raw-QUIC mode for
migration or trusted networks; the modes never fall back automatically.
Camouflage does not make UDP work when UDP is blocked entirely, and it does not
provide HTTP/3 site masquerading. Standard mode keeps quic-go's native UDP
batching; camouflage deliberately uses the portable PacketConn path so its
per-datagram transform is applied before sending. On Linux/WSL, raise the UDP
socket buffer limits before comparing throughput.

The transport uses the production-ready `github.com/quic-go/quic-go` library
and is built with Go 1.26.5. Application logs use the standard-library
`log/slog` package. JSON is the default format; set `logging.format` to `text`
when a human-readable stream is preferred. INFO and DEBUG records use bounded
per-event sampling (5 records/second, burst 20 by default), while warnings,
errors, authentication failures, and security audit records are never sampled.

The Agent can observe HTTP Host and TCP TLS SNI for proxy telemetry without
changing routing. Sniffing is enabled by default, reads at most 16 KiB for 250
ms, and replays every byte to the proxy. Domains are represented by a
process-scoped keyed hash; plaintext domains require both DEBUG logging and the
explicit `ASTERFERRY_LOG_EXPOSE_DOMAIN_DEBUG=true` override. Payloads, cookies,
credentials, and headers are never logged.

## v4 protocol negotiation

Control and data messages use the checked-in protobuf schema under `proto/`.
The outer frame carries a v4 version, stable type, request ID, and an inner
typed protobuf payload. During the authenticated handshake, the Agent and
Gateway negotiate supported features and the minimum of their frame, record,
UDP, and stream limits. The required `errors.v1` and `limits.v1` capabilities
must be present on both sides; there is no v3 fallback.

Protocol failures use stable error codes and a retryable flag. Remote error
details are deliberately short and sanitized, while full diagnostics remain
local log fields.

## Configuration examples

Copyable templates are available in [examples/README.md](examples/README.md),
[examples/gateway.yaml](examples/gateway.yaml), and
[examples/agent.yaml](examples/agent.yaml). Before production deployment,
replace the certificates, client CA, token, deployment-specific ALPN, and all
sample addresses.

Container deployment guides are available for
[Docker](deploy/docker/README.md) and
[Kubernetes](deploy/kubernetes/README.md). The Kubernetes package is a
role-selectable [Helm Chart](deploy/helm/asterferry), and container images are
published to `ghcr.io/eternallyzzz/asterferry` for version tags.

The Gateway firewall must allow the QUIC UDP port and the configured reverse
TCP/UDP ports. Management endpoints bind to loopback by default: Gateway uses
`127.0.0.1:9090` and Agent uses `127.0.0.1:9091`.

### Cluster readiness

The optional `cluster.node_id` field is identity metadata for future Gateway
coordination. It does not enable clustering, connect to Redis/etcd, or make
multiple Gateway replicas safe. Keep the Gateway at one replica until a
coordinated owner store, L4 connection affinity, and reverse-port routing are
deployed together. The v4 data plane remains local to the Gateway that owns an
Agent session; existing QUIC streams cannot be migrated between nodes.

## Security boundaries

- Production deployments require a Gateway server certificate, Agent client
  certificates, and a per-Agent token.
- Gateway egress proxying applies per-Agent, protocol, port, and destination-IP
  ACLs. Private, loopback, link-local, and metadata addresses are denied by
  default.
- Management endpoints must not be exposed to the public network.
- Logs do not record tokens, private keys, proxy payloads, cookies, credentials,
  or plaintext destinations by default.

### Runtime log overrides

Environment variables are applied after YAML and take precedence. Invalid
values fail startup:

```text
ASTERFERRY_LOG_LEVEL=info|debug|warn|error
ASTERFERRY_LOG_FORMAT=json|text
ASTERFERRY_LOG_SAMPLING_ENABLED=true|false
ASTERFERRY_LOG_SAMPLE_RATE=5
ASTERFERRY_LOG_SAMPLE_BURST=20
ASTERFERRY_LOG_SAMPLE_SUMMARY_INTERVAL=60
ASTERFERRY_LOG_SAMPLE_MAX_KEYS=4096
ASTERFERRY_LOG_EXPOSE_DOMAIN_DEBUG=false
ASTERFERRY_SHUTDOWN_GRACE_PERIOD=30
ASTERFERRY_SNIFF_ENABLED=true
ASTERFERRY_SNIFF_MAX_BYTES=16384
ASTERFERRY_SNIFF_TIMEOUT_MS=250
```

## Verification

```powershell
asterferry validate -c examples/gateway.yaml
asterferry validate -c examples/agent.yaml
go test ./...
go vet ./...
```

The historical `myproxy` and `myfrp` projects were design references only;
their v1 configuration and wire protocol are not compatible with AsterFerry v4.

## Performance validation

The repository includes three benchmark layers: raw QUIC streams,
`AsterFerry` proxy streams, and relay record round trips. Run the same command
on native Windows and in WSL with Go 1.26.5:

```powershell
./scripts/bench-windows.ps1
```

```bash
bash scripts/bench-wsl.sh
```

Results are written under the ignored `tmp/perf/` directory. The benchmark
matrix covers standard/camouflage transport modes, one/eight/32/64 streams,
and both `standard` and `balanced` relay profiles. Raw QUIC benchmarks include
upload, download, and round-trip directions at 16 KiB and 64 KiB payloads;
full proxy benchmarks report round-trip goodput at both payload sizes. The
Windows script writes `metadata.json` and `summary.json`. On a WSL image that
does not have Go installed, `scripts/bench-wsl.ps1` cross-builds static Linux
benchmark binaries with Go 1.26.5 and runs them inside WSL. WSL runs should
use an ext4 checkout when measuring runtime performance; `/mnt/d` is
acceptable for a quick smoke test but can distort build and startup timings.
Record the WSL `net.core.rmem_max` and `net.core.wmem_max` values as well:
restricted UDP socket limits can dominate Linux/WSL results before application
tuning. For a two-process Windows↔WSL reverse-tunnel measurement, use
`scripts/bench-cross-platform.ps1` with separately prepared v4 configs; it
uses `cmd/asterferry-bench` and emits a JSON goodput result. The configs must
map a TCP reverse tunnel to the benchmark echo endpoint; the script does not
generate or copy certificates and private keys.
