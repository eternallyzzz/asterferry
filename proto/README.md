# AsterFerry wire schemas

The current control plane is defined by [`controlwire/v1/control.proto`](controlwire/v1/control.proto).
It is a bidirectional gRPC service over TLS 1.3. Node enrollment uses a
single-use, role-bound token and a CSR; all subsequent node traffic requires
Controller-issued mTLS credentials.

The data plane is AFDP/2 (`asterferry-data/2`) over QUIC. Its protobuf open
metadata is defined by [`afdp/v1/data.proto`](afdp/v1/data.proto). TCP streams
switch to raw bytes after the bounded Open exchange. UDP payloads use QUIC
DATAGRAM with the fixed AFDP/2 version/flow/sequence/fragment header.

There is no fallback or compatibility codec. Unknown schema versions,
stale generations, bad checksums, malformed frames, and resource-limit
violations are rejected before allocation or state mutation.
