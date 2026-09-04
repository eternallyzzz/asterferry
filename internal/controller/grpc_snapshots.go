package controller

import (
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

func validateApplyResult(result *v1.ApplyResult, snapshot SnapshotRecord) error {
	if result == nil || result.GetGeneration() == 0 || result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_UNSPECIFIED {
		return errors.New("apply result generation and status are required")
	}
	switch result.GetStatus() {
	case v1.ApplyStatus_APPLY_STATUS_ACCEPTED, v1.ApplyStatus_APPLY_STATUS_APPLIED, v1.ApplyStatus_APPLY_STATUS_REJECTED:
	default:
		return errors.New("apply result status is invalid")
	}
	if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_REJECTED {
		if result.GetError() == nil || strings.TrimSpace(result.GetError().GetCode()) == "" || strings.TrimSpace(result.GetError().GetMessage()) == "" {
			return errors.New("rejected apply result must include an error")
		}
		if len(result.GetError().GetCode()) > 128 || len(result.GetError().GetFieldPath()) > 256 || len(result.GetError().GetMessage()) > 2048 {
			return errors.New("apply result error is too large")
		}
	} else if result.GetError() != nil {
		return errors.New("accepted or applied result must not include an error")
	}
	if snapshot.NodeID == "" {
		return errors.New("apply result has no desired snapshot")
	}
	if result.GetGeneration() != snapshot.Generation {
		return errors.New("apply result generation does not match desired snapshot")
	}
	if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_APPLIED && result.GetChecksum() == "" {
		return errors.New("applied result must include a checksum")
	}
	if result.GetChecksum() != "" && !strings.EqualFold(result.GetChecksum(), snapshot.Checksum) {
		return errors.New("apply result checksum does not match desired snapshot")
	}
	return nil
}

const (
	snapshotWireRetryLimit = 5
	snapshotWireRetryStart = 250 * time.Millisecond
	snapshotWireRetryMax   = 5 * time.Second
)

func (s *ControlServer) pushSnapshots(ctx context.Context, cancel context.CancelFunc, nodeID string, send func(*v1.ControllerMessage) error, lastSent *atomic.Uint64, changes <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
			snapshot, err := s.store.EnsureDesiredSnapshot(ctx, nodeID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				slog.Default().Warn("failed to refresh node desired snapshot", "node_id", nodeID, "error", err)
				continue
			}
			previous := lastSent.Load()
			if snapshot.Generation <= previous {
				continue
			}
			if err := s.sendSnapshotForWire(ctx, cancel, snapshot, send, lastSent); err != nil {
				return
			}
		}
	}
}

// sendSnapshotForWire is shared by the initial handshake and change-driven
// publication path. A transient inability to decrypt an at-rest obfuscation
// key must not strand an otherwise healthy control stream at its old
// generation. After a bounded retry window the stream is cancelled so the
// Node reconnects and re-enters the complete bootstrap/authentication path.
func (s *ControlServer) sendSnapshotForWire(ctx context.Context, cancel context.CancelFunc, snapshot SnapshotRecord, send func(*v1.ControllerMessage) error, lastSent *atomic.Uint64) error {
	retryDelay := snapshotWireRetryStart
	for attempt := 1; ; attempt++ {
		wireDocument, err := s.store.SnapshotDocumentForWire(snapshot.Document)
		if err == nil {
			message := &v1.ControllerMessage{Body: &v1.ControllerMessage_DesiredSnapshot{DesiredSnapshot: &v1.DesiredSnapshot{SchemaVersion: domain.CurrentControlProtocolVersion, NodeId: snapshot.NodeID, Generation: snapshot.Generation, Checksum: snapshot.Checksum, DocumentJson: wireDocument}}}
			if err := send(message); err != nil {
				return err
			}
			lastSent.Store(snapshot.Generation)
			return nil
		}
		if attempt >= snapshotWireRetryLimit {
			slog.Default().Error("desired snapshot remained unavailable for wire", "node_id", snapshot.NodeID, "attempts", attempt, "error", err)
			if cancel != nil {
				cancel()
			}
			return err
		}
		slog.Default().Warn("failed to prepare desired snapshot for wire; retrying", "node_id", snapshot.NodeID, "attempt", attempt, "retry_after", retryDelay, "error", err)
		if !waitForSnapshotRetry(ctx, retryDelay) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("snapshot wire retry was interrupted")
		}
		retryDelay *= 2
		if retryDelay > snapshotWireRetryMax {
			retryDelay = snapshotWireRetryMax
		}
	}
}

func waitForSnapshotRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
