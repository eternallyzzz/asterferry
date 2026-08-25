# Changelog

All notable AsterFerry releases are documented here.

## [0.1.0] - Unreleased

The first product release of the v6 transport stack.

- Ships the v6 TLS 1.3 and QUIC relay protocol with explicit capability and
  limit negotiation.
- Provides Gateway and Agent roles, SOCKS5/HTTP proxying, TCP/UDP reverse
  mappings, policy-controlled egress, and graceful draining.
- Includes an authenticated operations dashboard, runtime metrics, structured
  events, and scoped Admin/Viewer management credentials. The dashboard is
  read-only; configuration writes and runtime actions use the Admin CLI/API.
- Adds portable two-role bundles with `init`, `doctor`, `status`, `up`, `down`,
  `config`, and legacy-bundle migration workflows. The local supervisor
  restarts only the role that requests configuration reload with exit code 75.
- Makes Bundle workflows the primary CLI path, adds role-aware configuration
  commands, and removes the legacy `management.auth_token_file` field. Older
  Bundles must run `asterferry migrate` before startup; Admin and Viewer token
  paths are now explicit.
- Keeps Docker Compose aligned with the same Bundle layout and provides
  minimal generated configurations with production Dashboard serving disabled
  by default.
- Publishes static non-root distroless container images for Linux amd64 and
  arm64, plus native Linux amd64/arm64 and Windows amd64 CLI packages.
- Uses immutable release metadata, SHA256 checksums, SBOMs, and build
  attestations for published artifacts.

The v6 protocol is a breaking generation. v5 configuration frames and
connections are not compatible with this release; upgrade the Gateway and
Agent together. Reverse mappings now default to loopback and require an
explicit `gateway_bind` for public exposure; proxy opens carry bounded DNS
candidates for address-family failover.
