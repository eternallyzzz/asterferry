// Package controlwire contains the protocol glue shared by the Controller and
// nodes. Generated protobuf types live in controlwire/v1; this package adds
// domain conversion, strict size limits, and monotonic-generation checks.
package controlwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"asterferry/internal/jsonutil"
	"asterferry/internal/wireio"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	MaxControlMessageBytes = 16 << 20
	MaxEventBatchBytes     = 4 << 20
	ControlALPN            = "asterferry-control/1"
)

var (
	ErrStaleGeneration    = errors.New("stale generation")
	ErrGenerationConflict = errors.New("generation conflict")
	ErrUnknownSchema      = errors.New("unknown schema")
	ErrChecksumMismatch   = errors.New("checksum mismatch")
)

func SnapshotToProto(snapshot domain.DesiredSnapshot) (*v1.DesiredSnapshot, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	canonical := snapshot.Normalize()
	canonical.Checksum = ""
	checksum, err := canonical.ComputeChecksum()
	if err != nil {
		return nil, err
	}
	if snapshot.Checksum != "" && !strings.EqualFold(snapshot.Checksum, checksum) {
		return nil, &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: ErrChecksumMismatch.Error()}
	}
	canonical.Checksum = checksum
	document, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("marshal desired snapshot: %w", err)
	}
	if len(document) > MaxControlMessageBytes {
		return nil, &domain.ApplyError{Code: "message_too_large", Message: "desired snapshot exceeds control message limit"}
	}
	return &v1.DesiredSnapshot{
		SchemaVersion: uint32(canonical.SchemaVersion),
		NodeId:        canonical.NodeID,
		Generation:    canonical.Generation,
		Checksum:      canonical.Checksum,
		DocumentJson:  document,
	}, nil
}

func SnapshotFromProto(value *v1.DesiredSnapshot) (domain.DesiredSnapshot, error) {
	if value == nil {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "missing_snapshot", Message: "desired snapshot is required"}
	}
	if value.SchemaVersion != domain.SchemaVersion {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "unknown_schema", Path: "schema_version", Message: ErrUnknownSchema.Error()}
	}
	if value.NodeId == "" || value.Generation == 0 || value.Checksum == "" {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "invalid_snapshot_metadata", Message: "node_id, generation and checksum are required"}
	}
	if len(value.DocumentJson) == 0 || len(value.DocumentJson) > MaxControlMessageBytes {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "invalid_snapshot", Path: "document_json", Message: "snapshot document is empty or too large"}
	}
	var snapshot domain.DesiredSnapshot
	if err := jsonutil.DecodeStrict(value.DocumentJson, &snapshot); err != nil {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "invalid_snapshot", Path: "document_json", Message: "snapshot document is not valid JSON"}
	}
	if snapshot.SchemaVersion != uint32(domain.SchemaVersion) || snapshot.SchemaVersion != value.SchemaVersion || snapshot.NodeID != value.NodeId || snapshot.Generation != value.Generation || !strings.EqualFold(snapshot.Checksum, value.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "snapshot_metadata_mismatch", Message: "protobuf metadata does not match snapshot document"}
	}
	if err := snapshot.Validate(); err != nil {
		return domain.DesiredSnapshot{}, err
	}
	checksum, err := snapshot.ComputeChecksum()
	if err != nil || !strings.EqualFold(checksum, value.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: ErrChecksumMismatch.Error()}
	}
	return snapshot, nil
}

func ObservedToProto(state domain.ObservedState) (*v1.ObservedState, error) {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = domain.SchemaVersion
	}
	if state.NodeID == "" {
		return nil, errors.New("observed state node id is required")
	}
	if state.SchemaVersion != domain.SchemaVersion {
		return nil, ErrUnknownSchema
	}
	if state.ObservedAt.IsZero() {
		state.ObservedAt = time.Now().UTC()
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	document, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(document) > MaxControlMessageBytes {
		return nil, errors.New("observed state exceeds control message limit")
	}
	result := &v1.ObservedState{
		SchemaVersion:     state.SchemaVersion,
		NodeId:            state.NodeID,
		AppliedGeneration: state.AppliedGeneration,
		Healthy:           state.Healthy,
		Degraded:          state.Degraded,
		DocumentJson:      document,
		ObservedAt:        timestamppb.New(state.ObservedAt),
	}
	if state.LastError != nil {
		result.LastError = &v1.ApplyError{Code: state.LastError.Code, FieldPath: state.LastError.Path, Message: state.LastError.Message, Retryable: state.LastError.Retryable}
	}
	return result, nil
}

func ObservedFromProto(value *v1.ObservedState) (domain.ObservedState, error) {
	if value == nil || len(value.DocumentJson) == 0 || len(value.DocumentJson) > MaxControlMessageBytes {
		return domain.ObservedState{}, errors.New("invalid observed state")
	}
	if value.SchemaVersion != domain.SchemaVersion || value.NodeId == "" {
		return domain.ObservedState{}, ErrUnknownSchema
	}
	var state domain.ObservedState
	if err := jsonutil.DecodeStrict(value.DocumentJson, &state); err != nil {
		return domain.ObservedState{}, fmt.Errorf("decode observed state: %w", err)
	}
	if state.SchemaVersion != value.SchemaVersion || state.NodeID != value.NodeId || state.AppliedGeneration != value.AppliedGeneration || state.Healthy != value.Healthy || state.Degraded != value.Degraded {
		return domain.ObservedState{}, errors.New("observed state metadata mismatch")
	}
	if value.ObservedAt != nil {
		if err := value.ObservedAt.CheckValid(); err != nil {
			return domain.ObservedState{}, errors.New("observed timestamp is invalid")
		}
		wireObservedAt := value.ObservedAt.AsTime().UTC()
		// The timestamp is duplicated in the JSON document for a stable,
		// self-contained representation. Do not let a caller alter the indexed
		// protobuf timestamp while leaving the signed/documented state unchanged.
		if !state.ObservedAt.IsZero() && !state.ObservedAt.Equal(wireObservedAt) {
			return domain.ObservedState{}, errors.New("observed timestamp metadata mismatch")
		}
		state.ObservedAt = wireObservedAt
	}
	if value.LastError == nil && state.LastError != nil {
		return domain.ObservedState{}, errors.New("observed state error metadata mismatch")
	}
	if value.LastError != nil {
		if state.LastError == nil || state.LastError.Code != value.LastError.Code || state.LastError.Path != value.LastError.FieldPath || state.LastError.Message != value.LastError.Message || state.LastError.Retryable != value.LastError.Retryable {
			return domain.ObservedState{}, errors.New("observed state error metadata mismatch")
		}
		state.LastError = &domain.ApplyError{Code: value.LastError.Code, Path: value.LastError.FieldPath, Message: value.LastError.Message, Retryable: value.LastError.Retryable}
	}
	if err := state.Validate(); err != nil {
		return domain.ObservedState{}, err
	}
	return state, nil
}

// GenerationGate rejects every generation that is not strictly newer than
// the last accepted value. It is safe to share between the stream reader and
// an apply worker.
type GenerationGate struct {
	mu   sync.Mutex
	last uint64
}

func (g *GenerationGate) Last() uint64 { g.mu.Lock(); defer g.mu.Unlock(); return g.last }

func (g *GenerationGate) Accept(generation uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if generation == 0 {
		return ErrGenerationConflict
	}
	if generation <= g.last {
		return ErrStaleGeneration
	}
	g.last = generation
	return nil
}

func (g *GenerationGate) AcceptOrSame(generation uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if generation == g.last && generation != 0 {
		return nil
	}
	if generation == 0 {
		return ErrGenerationConflict
	}
	if generation <= g.last {
		return ErrStaleGeneration
	}
	g.last = generation
	return nil
}

// WriteMessage and ReadMessage are useful for non-gRPC control transports and
// fuzz tests. They use a four-byte big-endian length prefix and never allocate
// before checking the configured upper bound.
func WriteMessage(w io.Writer, message proto.Message, max int) error {
	if message == nil {
		return errors.New("control message is nil")
	}
	if max <= 0 || max > MaxControlMessageBytes {
		max = MaxControlMessageBytes
	}
	b, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	if len(b) > max {
		return fmt.Errorf("control message exceeds %d bytes", max)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(b)))
	if err := wireio.WriteFull(w, header[:]); err != nil {
		return err
	}
	return wireio.WriteFull(w, b)
}

func ReadMessage(r io.Reader, message proto.Message, max int) error {
	if message == nil {
		return errors.New("control message is nil")
	}
	if max <= 0 || max > MaxControlMessageBytes {
		max = MaxControlMessageBytes
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return errors.New("control message is empty")
	}
	if size > uint32(max) {
		return fmt.Errorf("control message exceeds %d bytes", max)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	if err := proto.Unmarshal(b, message); err != nil {
		return fmt.Errorf("decode control message: %w", err)
	}
	return nil
}

func ApplyResult(generation uint64, checksum string, status v1.ApplyStatus, applyErr *domain.ApplyError) *v1.ApplyResult {
	result := &v1.ApplyResult{Generation: generation, Checksum: checksum, Status: status}
	if applyErr != nil {
		result.Error = &v1.ApplyError{Code: applyErr.Code, FieldPath: applyErr.Path, Message: applyErr.Message, Retryable: applyErr.Retryable}
	}
	return result
}

func Heartbeat(generation uint64, healthy bool) *v1.Heartbeat {
	return &v1.Heartbeat{SentAt: timestamppb.New(time.Now().UTC()), AppliedGeneration: generation, Healthy: healthy}
}
