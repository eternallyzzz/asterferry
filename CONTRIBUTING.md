# Contributing

Changes to the Controller, Node data plane, protocol, storage schema,
authentication or release workflow need tests and a short compatibility note.
The project currently has one primary maintainer; automated checks are the
minimum safety net, not a substitute for a future second maintainer.

Before opening a change, run the focused package tests and then the checks in
[`docs/release-runbook.md`](docs/release-runbook.md) that cover the affected
surface. Do not commit local credentials, certificates, tokens, databases,
generated Dashboard assets or GeoIP binaries. Use temporary directories for
fixtures and run `pwsh -NoProfile -File scripts/secret-scan.ps1` before
staging.

Use the stable test taxonomy in [`docs/architecture.md`](docs/architecture.md);
volatile navigation details are in [`docs/architecture-internals.md`](docs/architecture-internals.md):
`*_contract_test.go` files specify observable behavior, `*_state_machine_test.go`
files compare deterministic lifecycle models with the implementation,
`*_fuzz_test.go` files cover decoder robustness, and `*_bench_test.go` files
cover performance. New behavior starts with a contract test; add a regression
case only when it records a distinct historical failure that the contract does
not already express. The focused contract gate is:

```text
go test -count=1 ./internal/afdp ./internal/controller ./internal/dataplane ./internal/duplex ./internal/node -run 'Contract|StateMachine'
```

To move generated local state and any test credentials out of the workspace,
run `pwsh -NoProfile -File scripts/clean-local-state.ps1`. The command moves
`tmp/`, `dist/`, `controller/`, generated Dashboard assets and root test
binaries into a recoverable quarantine directory outside the repository.

Keep handwritten production Go files in `cmd/` and `internal/` below 600 lines;
split by responsibility when a file approaches the limit. Generated protobuf
and embedded-asset sources are excluded by the layout check, but new generated
exceptions must be documented in `scripts/check-source-layout.py`.

API, wire and database changes must update the canonical OpenAPI or protocol
documentation and state whether the v2.x compatibility contract remains
intact. The canonical OpenAPI document is
`internal/controller/openapi.yaml`; run `python scripts/sync-openapi.py` after
editing it. `api/openapi.yaml` is generated and must not be edited directly.
