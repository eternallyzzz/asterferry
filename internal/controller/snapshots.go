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

	"asterferry/internal/domain"
)

// BuildDesiredSnapshot materializes the complete node-scoped desired state
// from the authoritative resources in SQLite.  The result is pure data: it
// can be checksummed, sent over the control stream, or encrypted in the node
// cache without giving a node access to the Controller repository.
func (s *Store) BuildDesiredSnapshot(ctx context.Context, nodeID string) (domain.DesiredSnapshot, error) {
	node, err := s.GetNode(ctx, nodeID)
	if err != nil {
		return domain.DesiredSnapshot{}, err
	}
	snapshot := domain.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeID: node.ID}
	serviceByID := make(map[string]domain.Service)
	switch node.Role {
	case domain.RoleGateway:
		spec, specErr := s.GetGatewaySpec(ctx, node.ID)
		if specErr != nil {
			return domain.DesiredSnapshot{}, specErr
		}
		snapshot.Gateway = &spec
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
	case domain.RoleAgent:
		spec, specErr := s.GetAgentSpec(ctx, node.ID)
		if specErr != nil {
			return domain.DesiredSnapshot{}, specErr
		}
		snapshot.Agent = &spec
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
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "invalid_role", Path: "node.role", Message: "node role must be gateway or agent"}
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
		if previous.Generation == math.MaxUint64 {
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
func (s *Store) EnsureDesiredSnapshot(ctx context.Context, nodeID string) (SnapshotRecord, error) {
	snapshot, err := s.BuildDesiredSnapshot(ctx, nodeID)
	if err != nil {
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

// RebuildDesiredSnapshots refreshes every node that has a complete spec.  A
// node without a spec is skipped so an operator can create the Node first and
// publish its role-specific spec in a later transaction.
func (s *Store) RebuildDesiredSnapshots(ctx context.Context) error {
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
