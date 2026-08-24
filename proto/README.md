# AsterFerry protocol notes

AsterFerry v5 uses a small deterministic binary codec instead of a generated
protobuf runtime. The v5 envelope is a fixed 16-byte header followed by a
typed payload:

```text
version:uint8 type:uint8 flags:uint16 length:uint32 request_id:uint64 payload
```

All integer fields are big-endian in the envelope. Typed payloads use
uvarint lengths and values, fixed field order, and reject trailing bytes. The
relay data stream uses the separate 12-byte record format implemented in
`internal/relay`.

The checked-in Go codec is the protocol source of truth. The former v4
protobuf wire files are intentionally removed: v5 has no v4 fallback or
downgrade path.
