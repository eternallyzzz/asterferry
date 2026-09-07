## Change summary

<!-- State the behavior or maintenance outcome. Link the issue or release item. -->

## Compatibility and risk

- [ ] No REST, wire, database, configuration, or release-contract change
- [ ] If a contract changes, compatibility and rollback are documented
- [ ] Security-sensitive changes include a threat/regression test

## Verification

- [ ] Focused tests
- [ ] `go test ./...`
- [ ] `go test -race ./...` (when applicable)
- [ ] `go vet ./...` and `staticcheck -checks=all,-SA1019 ./...`
- [ ] `govulncheck ./...`
- [ ] Source layout and toolchain checks
- [ ] `python scripts/secret-scan.py --staged`
- [ ] Generated files and OpenAPI copy are synchronized
