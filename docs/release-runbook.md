# Stable release runbook

This is the v1.0 release gate for a single- or active/standby Controller /
multi-Node self-hosted deployment. It is designed for a solo maintainer, so evidence is
automated and every exception is recorded in the release issue.

## Before the RC

1. Protect `main` with required pull-request review and CODEOWNERS checks.
   Assign a second named maintainer before the final v1.0.0 tag; the repository
   cannot solve that organizational dependency by itself.
2. Freeze the pinned release and compatibility toolchains in `.toolchain.json`,
   container base images, Helm dependencies and direct Go/npm versions. Do not
   mix dependency upgrades into the release candidate.
3. Run the tracked-file secret scan and remove all local keys, certificates,
   tokens, databases, binaries and generated Dashboard output from the
   workspace. Confirm `git ls-files` contains no credential material.
4. Run the Linux suite: unit tests, the deterministic behavior-contract and
   state-machine suite, PostgreSQL integration, race, vet,
   staticcheck, govulncheck, Dashboard lint/test/build on the release and
   Node.js 22 compatibility lanes, OpenAPI generation check, Helm lint/render
   and the AFDP/2 end-to-end test. Include the PostgreSQL lease/fencing,
   persisted-session and two-replica failover tests.
5. Run AFDP/control-wire fuzz smoke and the protocol benchmark suite. On a PR,
   the same-runner base/head comparison blocks a default regression above 10%.
6. Build and smoke-test Linux amd64/arm64 and Windows amd64, and lint/render
   both source Helm charts with an explicitly supplied operator image. Dockerfile
   validation is an optional local or manually dispatched check and never
   publishes an image. WSL is compatibility-tested separately; it is not an
   official support promise.
7. Test both SQLite and PostgreSQL from fresh initialization, backup, restore,
   restart and Node reconnect. For HA, run exactly two PostgreSQL-backed
   Controllers behind the external routing layer, stop the leader, verify the
   standby becomes ready within 30 seconds, and verify Nodes reconnect. Confirm
   restore invalidates browser sessions and resets the lease. Record the last
   verified backup timestamp.

## RC and soak

Create the release candidate tag `v1.0.0-rc.2`. The tag workflow marks
it prerelease and publishes immutable artifacts without changing the stable
source version. Each release must publish `install-controller.sh`,
`install-controller.ps1`, `install-node.sh`, `install-node.ps1` and
`node-release.json` together with the native archives and `SHA256SUMS`. The
Controller installers download the Node scripts and metadata into the
Controller data directory; they do not download Node binaries. On Windows,
`install-controller.ps1` also creates or updates the `AsterFerry-Controller`
service. The source-tree Windows installer resolves the newest stable release;
the copy attached to a release embeds that release's immutable download base
URL and version, so it can be executed without asking the operator for either
value. Dashboard-generated commands use the Controller-hosted script by
default, or the matching GitHub Release script when the operator selects that
source. Both paths use the Controller-provided metadata to select and verify
the Node archive. Controller and Node are published and upgraded as one
release unit; do not mix release assets.

The release is native-only: the project does not publish a GHCR container image
or OCI Helm chart. Operators build, scan and approve the image they configure
through Helm's required `image.repository` value.
Operate the candidate for at least seven calendar days with:

- Controller restart, leader loss and graceful shutdown checks;
- Node reconnect, certificate rotation and offline last-known-good checks;
- TCP, UDP, reverse-TCP, proxy and egress smoke traffic;
- metrics scrape from the explicitly exposed metrics listener;
- active/standby readiness routing and the <=30-second failover target;
- backup/restore verification and review of error, readiness and resource
  metrics; and
- no unresolved P0/P1 security, data-loss, protocol, or release-integrity
  issue.

Record benchmark output, supported-platform results, native asset checksums,
SBOM/attestation links, operator image provenance, backup evidence and known
limitations in the release issue. If the candidate changes, restart the
seven-day soak.

## Final publication

After the soak, update `CHANGELOG.md` with the release date, run
`scripts/release-check.ps1 -Version 1.0.0 -SkipDocker`, optionally repeat it
without `-SkipDocker` for local Dockerfile validation, merge the final commit
to `main`, and create `v1.0.0` from that commit. Verify the GitHub release
manifest, SHA-256 checksums, SBOM/attestation and the Linux/Windows install
paths before announcing it.

Do not delete the previous backup or RC artifacts. For a failed release,
withdraw the announcement, keep the immutable artifacts for forensics, restore
the last verified backup, and use the recorded RTO/RPO procedure.
