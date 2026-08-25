# Changelog

All notable AsterFerry releases are documented here.

## [0.1.0] - Unreleased

The first product release of the v5 transport stack.

- Ships the v5 TLS 1.3 and QUIC relay protocol with explicit capability and
  limit negotiation.
- Provides Gateway and Agent roles, SOCKS5/HTTP proxying, TCP/UDP reverse
  mappings, policy-controlled egress, and graceful draining.
- Includes the embedded authenticated operations dashboard, runtime metrics,
  structured events, and management actions.
- Publishes static non-root distroless container images for Linux amd64 and
  arm64, plus native Linux amd64/arm64 and Windows amd64 CLI packages.
- Uses immutable release metadata, SHA256 checksums, SBOMs, and build
  attestations for published artifacts.

The v5 protocol is a breaking generation. v4 configuration frames and
connections are not compatible with this release; upgrade the Gateway and
Agent together.
