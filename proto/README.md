# AsterFerry protocol schema

The v4 wire schema is defined in `asterferry/v4/asterferry.proto`. The Go
runtime uses the generated file under `internal/transport/wirev4` and keeps
the generated source in the repository so normal builds do not require a
protobuf compiler.

Regenerate it from the repository root with:

```sh
protoc --proto_path=proto --go_out=. --go_opt=module=asterferry \
  proto/asterferry/v4/asterferry.proto
```

Use `google.golang.org/protobuf` and the matching `protoc-gen-go` release
(`v1.36.12`). AsterFerry v4 is intentionally not wire-compatible with v3;
there is no protocol downgrade path.
