# Security policy

Please do not publish credentials, private keys, certificates, controller
backups or exploit details in a public issue. Report security problems to the
maintainer privately with the affected version, deployment mode, reproduction
steps and impact. Remove all real secrets from reproductions before sending
them.

The v1.0 release is intended for self-hosted personal and small-team networks.
The Controller is single-replica, browser sessions are process-local, and
metrics/OpenAPI exposure must be selected explicitly at the deployment layer.
These are documented product boundaries, not promises of HA or a hosted
security service.
