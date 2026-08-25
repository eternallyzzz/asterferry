# AsterFerry

AsterFerry is a lightweight private-network relay. A public `gateway` and an
internal `agent` connect over TLS 1.3 and QUIC to provide local SOCKS5/HTTP
proxying and TCP/UDP reverse mappings.

The current configuration and wire protocol are v6, with strict role
separation:

- `gateway`: public entry point, Agent mTLS/token authentication, reverse port
  mappings, and policy-controlled egress proxying.
- `agent`: actively connects to the Gateway, exposes local proxy listeners, and
  registers internal services as reverse mappings.

For normal local operation, use the generated Bundle as the primary workflow:

```text
asterferry init ./asterferry --profile dev
asterferry up ./asterferry
asterferry status ./asterferry
```

The role commands remain available for systemd, Docker, Kubernetes, and other
deployments that start one process per container or service:

```text
asterferry gateway --config gateway.yaml
asterferry agent   --config agent.yaml
```

## Quick start

For a local Gateway-Agent test pair, let the CLI generate the configuration,
tokens, obfuscation key, and self-signed certificates:

```powershell
asterferry init ./asterferry --profile dev
asterferry doctor ./asterferry
```

Start both roles from the Bundle:

```powershell
asterferry up ./asterferry
```

Use `asterferry status ./asterferry` to inspect both running roles. The generated
`dev` certificates are for local testing only. For production, use
`asterferry init --profile prod`, submit the generated CSRs to the deployment
PKI, install the CA/certificates, and run `doctor` before starting.

Inspect or validate one role from a Bundle without remembering its generated
file path:

```powershell
asterferry config show ./asterferry --role gateway
asterferry config validate ./asterferry --role agent
asterferry validate ./asterferry --role gateway
```

Older Bundles that used `management.auth_token_file` must be upgraded once
before they can start:

```powershell
asterferry migrate ./asterferry
```

Migration writes configuration backups beside the original YAML files. Use
`--dry-run` to preview the changes.

Every command has focused help. `asterferry completion powershell` (or
`bash`/`zsh`) generates shell completion scripts, and `version` reports the
build and protocol versions without loading a configuration file.

## Dashboard

Running roles serve an embedded operations dashboard from their existing
management listener. The Dashboard is enabled by default but can be disabled
without disabling the protected management API:

```text
Gateway: http://127.0.0.1:9090/dashboard/
Agent:   http://127.0.0.1:9091/dashboard/
```

Enter the generated viewer token in the page. The token is sent only as
a Bearer header, is held in browser memory, and is never put in a URL or
stored on disk. The page shows live status, traffic trends, QUIC diagnostics,
Agent/mapping inventory, and structured runtime events. It is intentionally
read-only: the Dashboard can validate and preview a redacted configuration
draft, while configuration writes and runtime actions require the Admin token
through the CLI or protected management API.

The embedded page is controlled by `management.web.enabled` (default `true` for
legacy/manual configurations; generated production bundles set it to `false`).
The management listener remains available when the page is disabled. It binds
to loopback and uses HTTP by default. A non-loopback `management.listen`
requires `management.tls.cert_file` and `management.tls.key_file`; configure a
`ca_file` when the CLI must verify a private management certificate.

For a remote host, keep the management listener on loopback and use an
administrative port forward, for example:

```sh
ssh -N -L 9090:127.0.0.1:9090 operator@example-host
```

The default deployment does not expose management ports publicly, provide SSO,
or persist event history. Use the existing JSON logs or an external log system
for long-term audit retention. If a public management listener is required,
use built-in TLS and a narrowly scoped firewall policy.

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

The v6 default also applies a versioned outer UDP camouflage layer around QUIC.
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
and is built with Go 1.26.7. Application logs use the standard-library
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

## v6 protocol negotiation

Control messages use a deterministic binary codec maintained under
`internal/transport`; the outer envelope has a fixed 16-byte header, stable
type, request ID, and a length-delimited payload. Proxy payloads use compact
binary relay records with bounded padding and batched writes. TCP and proxy
traffic stay on reliable QUIC streams; the protocol does not use QUIC
datagrams.

During the authenticated handshake, the Agent and Gateway negotiate supported
features and the minimum of their frame, record, write-batch, UDP, and stream
limits. The required `errors.v1` and `limits.v1` capabilities must be present
on both sides. v6 is a breaking protocol generation: v5 frames and
connections are rejected, with no downgrade or compatibility path. The v6
registration payload carries an explicit reverse bind address, and proxy
opens carry bounded DNS candidates for deterministic address-family failover.

Protocol failures use stable error codes and a retryable flag. Remote error
details are deliberately short and sanitized, while full diagnostics remain
local log fields.

## Configuration examples

Copyable templates are available in [examples/README.md](examples/README.md),
[examples/gateway.yaml](examples/gateway.yaml), and
[examples/agent.yaml](examples/agent.yaml). Before production deployment,
replace the certificates (the Agent client certificate URI SAN must be
`urn:asterferry:agent:<agent-id>`), client CA, Agent token, management
Admin/Viewer tokens,
deployment-specific ALPN, and all sample addresses.

Container deployment guides are available for
[Docker](deploy/docker/README.md) and
[Kubernetes](deploy/kubernetes/README.md). The Kubernetes package is a
role-selectable [Helm Chart](deploy/helm/asterferry), and container images are
published to `ghcr.io/eternallyzzz/asterferry` for version tags. Release
versions also publish native CLI archives, an OCI Helm Chart, checksums, an
SBOM, and signed build provenance.

The Gateway firewall must allow the QUIC UDP port and the configured reverse
TCP/UDP ports. Management endpoints bind to loopback by default: Gateway uses
`127.0.0.1:9090` and Agent uses `127.0.0.1:9091`.

Configuration file references to certificates, tokens, and keys are resolved
relative to the configuration file directory. Absolute paths are unchanged;
this makes generated bundles portable between working directories and WSL.

### Cluster readiness

The optional `cluster.node_id` field is identity metadata for future Gateway
coordination. It does not enable clustering, connect to Redis/etcd, or make
multiple Gateway replicas safe. Keep the Gateway at one replica until a
coordinated owner store, L4 connection affinity, and reverse-port routing are
deployed together. The v6 data plane remains local to the Gateway that owns an
Agent session; existing QUIC streams cannot be migrated between nodes.

## Security boundaries

- Production deployments require a Gateway server certificate, Agent client
  certificates with URI SAN identity binding, a per-Agent token, and separate
  management Admin/Viewer Bearer tokens. Health probes are anonymous; metrics
  and status require the Viewer token.
- Gateway egress proxying applies per-Agent, protocol, port, and destination-IP
  ACLs. Special-use, private, loopback, link-local, metadata, and reserved
  addresses are denied by default; use narrow `allow_special_cidrs` entries
  only for explicitly approved exceptions.
- Non-loopback management endpoints require built-in TLS and the management
  Bearer token. Loopback plus SSH/Kubernetes port forwarding remains the
  preferred deployment.
- Logs do not record tokens, private keys, proxy payloads, cookies, credentials,
  or plaintext destinations by default.

Container probes use `asterferry healthcheck --url ...`; HTTPS probes with a
private certificate may add `--insecure-tls` when the request is strictly
loopback-local. The production image does not include a shell or
general-purpose network tools.

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

## Releases, verification, and upgrades

The first product release is `v0.1.0`. Product versions use SemVer without
changing the independent v6 wire-protocol identifier. A release tag on `main`
publishes the following immutable release material:

- Linux amd64/arm64 and Windows amd64 CLI archives.
- A multi-platform GHCR image at `ghcr.io/eternallyzzz/asterferry`.
- An OCI Chart at `oci://ghcr.io/eternallyzzz/charts/asterferry`.
- `SHA256SUMS`, an SPDX JSON SBOM, `release-manifest.json`, and GitHub build
  attestations.

Run the local release preflight before creating a tag:

```powershell
.\scripts\release-check.ps1 -Version 0.1.0
```

For a container deployment, prefer the image digest recorded in
`release-manifest.json` over a mutable tag:

```text
ghcr.io/eternallyzzz/asterferry@sha256:<image-digest>
```

Verify downloaded release files and attestations before deployment:

```sh
sha256sum -c SHA256SUMS
gh attestation verify asterferry_0.1.0_linux_amd64.tar.gz \
  --repo eternallyzzz/asterferry
cosign verify \
  --certificate-identity-regexp 'https://github.com/eternallyzzz/asterferry/.github/workflows/container.yml@refs/tags/v0.1.0' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/eternallyzzz/asterferry@sha256:<image-digest>
```

For Kubernetes, install the signed Chart by version and set `image.digest` to
the matching image digest. Run `validate` and `doctor` against the exact
configuration and secrets before upgrading. Use an atomic Helm upgrade and
keep the previous release available for rollback. For Compose, pull the pinned
image, run `docker compose config`, then recreate both roles together.

The v6 protocol is a breaking generation, so Gateway and Agent upgrades must
be coordinated. A rollback is supported only between releases that share the
same compatible configuration and protocol generation; do not use a v5
binary or configuration as a v6 rollback target.

## Verification

The local full-verification entry point checks the native Windows toolchain,
the selected WSL distribution, Linux/Windows cross-builds, a local multi-
platform Docker image build, and both Helm release roles. It never pushes an
image or creates a release:

```powershell
.\scripts\test-all.ps1
.\scripts\test-all.ps1 -WslDistro Debian -FullBench
```

The default run performs `gofmt` validation, `go vet`, uncached tests, race
tests, and builds for Windows amd64 plus Linux amd64/arm64. WSL must have Go
1.26.7, `gcc`, and a working race-test toolchain for strict full verification.
If WSL has no Go installation, pass `-SkipRace`; the runner cross-compiles
each package's functional test binary on Windows and executes those binaries
inside WSL, while clearly reporting that race testing was skipped. Docker
Buildx and Helm are required for the container and chart checks. Use
`-FullBench` to run the complete Windows and WSL performance matrices; results
and verification logs are written under ignored `tmp/test/` and `tmp/perf/`
directories.

For a quick configuration-only check, the built binary can still validate the
examples directly:

```powershell
asterferry validate --config examples/gateway.yaml
asterferry validate --config examples/agent.yaml
```

`validate` checks YAML and semantic configuration rules without requiring
mounted secrets. `doctor` performs the local deployment checks, including
secret permissions, TLS identities and certificate expiry, and temporary port
availability checks. Use `doctor --skip-ports` when the role is already
running.

### Coverage

Generate runtime coverage reports for Windows and the selected native WSL
distribution with:

```powershell
.\scripts\coverage.ps1
.\scripts\coverage.ps1 -WslDistro Debian
```

The command runs the complete Go test suite with `-coverpkg`, so the existing
integration tests contribute coverage to the Agent, Gateway, and Transport
runtime packages. It writes separate `coverage.out`, `functions.txt`,
`coverage.html`, logs, and metadata under `tmp/coverage/windows/` and
`tmp/coverage/wsl/`. The benchmark-only command `cmd/asterferry-bench` is
excluded from the runtime denominator. The WSL
distribution must have the expected Go toolchain installed; coverage does not
fall back to cross-compiled test binaries.

The historical `myproxy` and `myfrp` projects were design references only;
their v1 configuration and wire protocol are not compatible with AsterFerry v6.

## Performance validation

The repository includes three benchmark layers: raw QUIC streams,
`AsterFerry` proxy streams, and relay record round trips. Run the same command
on native Windows and in WSL with Go 1.26.7:

```powershell
./scripts/bench-windows.ps1
```

```bash
bash scripts/bench-wsl.sh
```

The default command is a short representative smoke suite: standard and
camouflage end-to-end goodput at 64 KiB/8 streams, relay round trips, and the
1 KiB/1-stream latency benchmark. Use `-FullMatrix` on Windows (and
`powershell.exe -File scripts/bench-wsl.ps1 -FullMatrix` for WSL) to run every
transport/proxy combination. Results are written under the ignored
`tmp/perf/` directory. The full benchmark matrix covers
standard/camouflage transport modes, one/eight/32/64 streams,
and both `standard` and `balanced` relay profiles. Raw QUIC benchmarks include
upload, download, and round-trip directions at 16 KiB and 64 KiB payloads;
full proxy benchmarks report round-trip goodput at both payload sizes. The
Windows script writes `metadata.json` and `summary.json`. On a WSL image that
does not have Go installed, `scripts/bench-wsl.ps1` cross-builds static Linux
benchmark binaries with Go 1.26.7 and runs them inside WSL. WSL runs should
use an ext4 checkout when measuring runtime performance; `/mnt/d` is
acceptable for a quick smoke test but can distort build and startup timings.
Record the WSL `net.core.rmem_max` and `net.core.wmem_max` values as well:
restricted UDP socket limits can dominate Linux/WSL results before application
tuning. For a two-process Windows↔WSL reverse-tunnel measurement, use
`scripts/bench-cross-platform.ps1` with separately prepared v6 configs; it
uses `cmd/asterferry-bench` and emits a JSON goodput result. The configs must
map a TCP reverse tunnel to the benchmark echo endpoint; the script does not
generate or copy certificates and private keys.
