# Security policy

Please do not publish credentials, private keys, certificates, controller
backups or exploit details in a public issue. Report security problems to the
maintainer privately with the affected version, deployment mode, reproduction
steps and impact. Remove all real secrets from reproductions before sending
them.

The current stable release is intended for self-hosted personal and small-team
networks. SQLite deployments are single-replica; PostgreSQL deployments may
run exactly two active/standby Controller replicas behind an external,
readiness-aware routing layer. Browser sessions are durable database records
and are shared by PostgreSQL replicas, while ChangeBus notifications, runtime
registries and active streams remain process-local. Metrics/OpenAPI exposure
must be selected explicitly at the deployment layer. These are documented
product boundaries, not promises of a hosted security service.

## Release integrity and self-update

Native self-update is allowed only from a published stable GitHub Release that
contains `release-manifest.json` and its detached
`release-manifest.json.sig`. Controller and Node embed the repository's Cosign
public key and verify the manifest before using any archive digest. The
manifest digest must also agree with `SHA256SUMS`; the checksum file alone is
not a trust root. The release workflow signs the manifest with the private key
held in `ASTERFERRY_RELEASE_SIGNING_KEY`, verifies it against
`internal/update/release-public-key.pem`, and requires the protected
`ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256` SPKI fingerprint secret to match that
embedded key.

Before the first stable release, the embedded development public key and the
CI private-key secret must be replaced together with the project's real Cosign
ECDSA P-256 key pair. The release gate rejects the development key fingerprint.
A key rotation is a release-integrity change: publish the new binary
generation only after updating the embedded key, protected fingerprint and
documenting the transition. Manual installer downloads are outside this
runtime signature-verification scope and remain a separate installer contract.
The cross-platform gate is implemented by
`scripts/check-release-public-key.py`; the Bash and PowerShell entry points
share that implementation and do not require OpenSSL. Local release preflight
requires the same `ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256` environment value as
the protected CI gate.

The replacement helper writes a schema-versioned local journal before waiting
for the parent, before the executable rename, and after the rename. It records
the SHA-256 identities of the original and staged executables. A process that
starts with `prepared`, `replacing`, `waiting`, or `recovering` state first
reconciles those identities with the live, staged and backup files. Only a
verified target layout followed by readiness can become successful; a verified
old layout becomes `failed`, while partial or ambiguous layouts report
`manual_required` and retain the replacement files for operator review. Pending
journals without identity metadata are never auto-promoted.

The updater's `InsecureSkipVerify` is limited to the local HTTPS readiness
probe. The endpoint must be HTTPS on a loopback IP (or `localhost`), cannot
carry credentials, query strings, or fragments, and redirects are disabled.
Downloaded release bytes are independently checked against the signed
manifest and SHA-256 digest. This TLS exception must not be reused for release
downloads or remote health endpoints.
