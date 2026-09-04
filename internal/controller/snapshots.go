package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"asterferry/internal/domain"
)

type snapshotSubscription struct {
	id uint64
	ch chan struct{}
}

// SubscribeSnapshotChanges returns a coalescing notification stream for
// desired-state writes. The notification carries no payload: the subscriber
// must materialize the current node-scoped snapshot after receiving it.
func (b *ChangeBus) SubscribeSnapshotChanges(nodeID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	sub := &snapshotSubscription{id: nextActionSubscription.Add(1), ch: ch}
	b.actionMu.Lock()
	if b.closed.Load() {
		close(ch)
		b.actionMu.Unlock()
		return ch, func() {}
	}
	if b.snapshotSubs == nil {
		b.snapshotSubs = make(map[string]map[uint64]*snapshotSubscription)
	}
	if b.snapshotSubs[nodeID] == nil {
		b.snapshotSubs[nodeID] = make(map[uint64]*snapshotSubscription)
	}
	b.snapshotSubs[nodeID][sub.id] = sub
	b.actionMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.actionMu.Lock()
			if subscribers := b.snapshotSubs[nodeID]; subscribers != nil {
				if current := subscribers[sub.id]; current == sub {
					delete(subscribers, sub.id)
					close(current.ch)
				}
				if len(subscribers) == 0 {
					delete(b.snapshotSubs, nodeID)
				}
			}
			b.actionMu.Unlock()
		})
	}
}

func (b *ChangeBus) notifySnapshotChanges(nodeIDs ...string) {
	if len(nodeIDs) == 0 {
		return
	}
	targets := make(map[string]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if strings.TrimSpace(nodeID) != "" {
			targets[nodeID] = struct{}{}
		}
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	if b.closed.Load() {
		return
	}
	for nodeID := range targets {
		subscribers := b.snapshotSubs[nodeID]
		for _, subscriber := range subscribers {
			select {
			case subscriber.ch <- struct{}{}:
			default:
			}
		}
	}
}

func (s *ResourceRepository) commitAndNotify(tx *sql.Tx, nodeIDs ...string) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ChangeBus().notifySnapshotChanges(nodeIDs...)
	return nil
}

func (s *ResourceRepository) commitAndNotifyResources(tx *sql.Tx, nodeIDs ...string) error {
	return s.commitAndNotifyResourcesWithOptions(tx, false, nodeIDs...)
}

func (s *ResourceRepository) commitAndNotifyPendingServices(tx *sql.Tx, nodeIDs ...string) error {
	return s.commitAndNotifyResourcesWithOptions(tx, true, nodeIDs...)
}

func (s *ResourceRepository) commitAndNotifyResourcesWithOptions(tx *sql.Tx, pendingServices bool, nodeIDs ...string) error {
	if err := s.commitAndNotify(tx, nodeIDs...); err != nil {
		return err
	}
	s.ChangeBus().notifyResourceChangesWithOptions(pendingServices, nodeIDs...)
	return nil
}

func (s *ResourceRepository) commitAndNotifyResourceOnly(tx *sql.Tx, nodeIDs ...string) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ChangeBus().notifyResourceChanges(nodeIDs...)
	return nil
}

// BuildDesiredSnapshot materializes the complete node-scoped desired state
// from the authoritative resources in the configured database.  The result is pure data: it
// can be checksummed, sent over the control stream, or encrypted in the node
// cache without giving a node access to the Controller repository.
func (s *ResourceRepository) BuildDesiredSnapshot(ctx context.Context, nodeID string) (domain.DesiredSnapshot, error) {
	node, err := s.GetNode(ctx, nodeID)
	if err != nil {
		return domain.DesiredSnapshot{}, err
	}
	snapshot := domain.DesiredSnapshot{SchemaVersion: domain.CurrentControlProtocolVersion, NodeID: node.ID}
	serviceByID := make(map[string]domain.Service)
	nodeSpec, specErr := s.GetNodeSpec(ctx, node.ID)
	if errors.Is(specErr, sql.ErrNoRows) {
		// An enrolled but unconfigured generic node is healthy enough to keep a
		// control stream; it simply has no desired data-plane snapshot yet.
		return domain.DesiredSnapshot{}, sql.ErrNoRows
	}
	if specErr != nil {
		return domain.DesiredSnapshot{}, specErr
	}
	switch nodeSpec.Kind {
	case domain.NodeSpecGateway:
		snapshot.Gateway = nodeSpec.Gateway
		assignments, listErr := s.ListAssignments(ctx, node.ID, "")
		if listErr != nil {
			return domain.DesiredSnapshot{}, listErr
		}
		snapshot.Assignments = assignments
		for _, assignment := range assignments {
			for _, serviceID := range assignment.ServiceIDs {
				if _, seen := serviceByID[serviceID]; seen {
					continue
				}
				service, serviceErr := s.GetService(ctx, serviceID)
				if serviceErr != nil {
					return domain.DesiredSnapshot{}, fmt.Errorf("assignment %q service %q: %w", assignment.ID, serviceID, serviceErr)
				}
				serviceByID[service.ID] = service
			}
		}
	case domain.NodeSpecAgent:
		snapshot.Agent = nodeSpec.Agent
		services, listErr := s.ListServices(ctx, node.ID)
		if listErr != nil {
			return domain.DesiredSnapshot{}, listErr
		}
		for _, service := range services {
			serviceByID[service.ID] = service
		}
		assignments, listErr := s.ListAssignments(ctx, "", node.ID)
		if listErr != nil {
			return domain.DesiredSnapshot{}, listErr
		}
		snapshot.Assignments = assignments
	default:
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "invalid_spec_kind", Path: "node_spec.kind", Message: "node spec kind must be gateway or agent"}
	}
	serviceIDs := make([]string, 0, len(serviceByID))
	for serviceID := range serviceByID {
		serviceIDs = append(serviceIDs, serviceID)
	}
	sort.Strings(serviceIDs)
	for _, serviceID := range serviceIDs {
		snapshot.Services = append(snapshot.Services, serviceByID[serviceID])
	}
	// Assignment generations are shared by both ends of a data-plane session,
	// while snapshot generations are node-local. A newly selected Gateway may
	// therefore receive an assignment whose generation is ahead of that
	// Gateway's previous snapshot; start its local generation at the highest
	// contained assignment generation so the cross-node envelope remains
	// self-consistent.
	for _, assignment := range snapshot.Assignments {
		if assignment.Generation > snapshot.Generation {
			snapshot.Generation = assignment.Generation
		}
	}
	// Legacy v3 assignments may contain Controller-encrypted key material
	// without the plaintext-derived KeyID. Normalize the storage form before
	// calculating a checksum so the persisted document and the decrypted wire
	// document share one canonical identity.
	if snapshot.Gateway != nil {
		if err := s.protectObfuscationPolicy(&snapshot.Gateway.Obfuscation); err != nil {
			return domain.DesiredSnapshot{}, err
		}
	}
	for index := range snapshot.Assignments {
		if err := s.protectObfuscationPolicy(&snapshot.Assignments[index].Obfuscation); err != nil {
			return domain.DesiredSnapshot{}, fmt.Errorf("assignment %q obfuscation: %w", snapshot.Assignments[index].ID, err)
		}
	}

	// Existing generations are retained when the materialized content is
	// unchanged.  This makes reconnects idempotent and lets nodes reject stale
	// snapshots using one monotonic number per node.
	previous, previousErr := s.LoadSnapshot(ctx, node.ID)
	if previousErr == nil {
		if previous.Generation > snapshot.Generation {
			snapshot.Generation = previous.Generation
		}
	} else if !errors.Is(previousErr, sql.ErrNoRows) {
		return domain.DesiredSnapshot{}, previousErr
	} else if snapshot.Generation == 0 {
		snapshot.Generation = 1
	}
	candidate, err := snapshot.WithChecksum()
	if err != nil {
		return domain.DesiredSnapshot{}, err
	}
	if previousErr == nil && strings.EqualFold(previous.Checksum, candidate.Checksum) {
		return candidate, nil
	}
	if previousErr == nil {
		if previous.Generation >= math.MaxInt64 {
			return domain.DesiredSnapshot{}, errors.New("desired snapshot generation is exhausted")
		}
		nextGeneration := previous.Generation + 1
		if snapshot.Generation < nextGeneration {
			snapshot.Generation = nextGeneration
		}
		candidate, err = snapshot.WithChecksum()
		if err != nil {
			return domain.DesiredSnapshot{}, err
		}
	}
	return candidate, nil
}

// EnsureDesiredSnapshot materializes and durably records the current state if
// a resource write has changed it.  It is safe to call before every control
// stream reconnect; unchanged state does not create another generation.
func (s *ResourceRepository) EnsureDesiredSnapshot(ctx context.Context, nodeID string) (SnapshotRecord, error) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	snapshot, err := s.BuildDesiredSnapshot(ctx, nodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A deleted NodeSpec leaves an explicit empty snapshot behind so the
			// connected Node can retire its previous behavior. A stale non-empty row
			// from an older binary is fail-closed here as well.
			if existing, loadErr := s.LoadSnapshot(ctx, nodeID); loadErr == nil {
				var current domain.DesiredSnapshot
				if unmarshalErr := json.Unmarshal(existing.Document, &current); unmarshalErr != nil {
					return SnapshotRecord{}, unmarshalErr
				}
				if current.Gateway == nil && current.Agent == nil {
					return existing, nil
				}
				if existing.Generation == math.MaxInt64 {
					return SnapshotRecord{}, errors.New("desired snapshot generation is exhausted")
				}
				cleared, clearErr := (domain.DesiredSnapshot{
					SchemaVersion: domain.CurrentControlProtocolVersion,
					NodeID:        nodeID,
					Generation:    existing.Generation + 1,
				}).WithChecksum()
				if clearErr != nil {
					return SnapshotRecord{}, clearErr
				}
				data, marshalErr := json.Marshal(cleared)
				if marshalErr != nil {
					return SnapshotRecord{}, marshalErr
				}
				if saveErr := s.SaveSnapshot(ctx, SnapshotRecord{NodeID: nodeID, Generation: cleared.Generation, Checksum: cleared.Checksum, Document: data}); saveErr != nil {
					return SnapshotRecord{}, saveErr
				}
				return s.LoadSnapshot(ctx, nodeID)
			} else if !errors.Is(loadErr, sql.ErrNoRows) {
				return SnapshotRecord{}, loadErr
			}
		}
		return SnapshotRecord{}, err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return SnapshotRecord{}, err
	}
	if err := s.SaveSnapshot(ctx, SnapshotRecord{NodeID: snapshot.NodeID, Generation: snapshot.Generation, Checksum: snapshot.Checksum, Document: data}); err != nil {
		return SnapshotRecord{}, err
	}
	return s.LoadSnapshot(ctx, nodeID)
}

// clearDesiredSnapshotTx writes the fail-closed empty snapshot used when a
// node behavior is deleted. The caller owns the surrounding resource
// transaction and snapshotMu, so spec deletion and data-plane retirement are
// published as one generation transition.
func clearDesiredSnapshotTx(ctx context.Context, tx *sql.Tx, nodeID string) error {
	var current uint64
	err := tx.QueryRowContext(ctx, `SELECT generation FROM desired_snapshots WHERE node_id=?`, nodeID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	generation := uint64(1)
	if err == nil {
		if current >= math.MaxInt64 {
			return errors.New("desired snapshot generation is exhausted")
		}
		generation = current + 1
	}
	snapshot, err := (domain.DesiredSnapshot{SchemaVersion: domain.CurrentControlProtocolVersion, NodeID: nodeID, Generation: generation}).WithChecksum()
	if err != nil {
		return err
	}
	document, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,payload_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,checksum=excluded.checksum,payload_json=excluded.payload_json,created_at=excluded.created_at WHERE excluded.generation > desired_snapshots.generation`, nodeID, snapshot.Generation, snapshot.Checksum, document, now.Format(time.RFC3339Nano))
	return err
}

// RebuildDesiredSnapshots refreshes every node that has a complete spec.  A
// node without a spec is skipped so an operator can enroll the identity first
// and publish its behavior document in a later transaction.
func (s *ResourceRepository) RebuildDesiredSnapshots(ctx context.Context) error {
	nodes, err := s.ListNodes(ctx, "")
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if _, err := s.EnsureDesiredSnapshot(ctx, node.ID); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return err
		}
	}
	return nil
}
