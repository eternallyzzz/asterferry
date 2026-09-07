# Security policy

Please do not publish credentials, private keys, certificates, controller
backups or exploit details in a public issue. Report security problems to the
maintainer privately with the affected version, deployment mode, reproduction
steps and impact. Remove all real secrets from reproductions before sending
them.

The v1.0.0 release is intended for self-hosted personal and small-team
networks. SQLite deployments are single-replica; PostgreSQL deployments may
run exactly two active/standby Controller replicas behind an external,
readiness-aware routing layer. Browser sessions are durable database records
and are shared by PostgreSQL replicas, while ChangeBus notifications, runtime
registries and active streams remain process-local. Metrics/OpenAPI exposure
must be selected explicitly at the deployment layer. These are documented
product boundaries, not promises of a hosted security service.
