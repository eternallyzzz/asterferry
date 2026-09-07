package dataplane

import (
	"asterferry/internal/afdp"
	"asterferry/internal/domain"
	"context"
	"errors"
	"fmt"
)

// ApplySnapshot validates and builds all new indexes before taking the lock;
// a malformed or partially failing snapshot therefore cannot replace the
// last-applied generation.
func (e *Engine) ApplySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if e.closed.Load() {
		return errors.New("data-plane engine is closed")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.Checksum == "" {
		return &domain.ApplyError{Code: "missing_checksum", Path: "checksum", Message: "data-plane snapshots must include a checksum"}
	}
	if snapshot.NodeID != e.nodeID {
		return errors.New("snapshot node id does not match engine")
	}
	if e.kind == domain.NodeSpecGateway && snapshot.Gateway == nil {
		return errors.New("gateway snapshot is required")
	}
	if e.kind == domain.NodeSpecAgent && snapshot.Agent == nil {
		return errors.New("agent snapshot is required")
	}
	currentGeneration := e.Generation()
	rollback := previous != nil && snapshot.Generation < previous.Generation && currentGeneration == previous.Generation
	// A same-generation snapshot is normally an exact replay, but the
	// Controller also uses it to repair a node whose cache has diverged.  The
	// caller must provide the previous snapshot so a random direct call cannot
	// bypass the monotonic-generation gate; checksum equality is deliberately
	// not required here because the new snapshot is authoritative.
	sameGeneration := previous != nil && snapshot.Generation == currentGeneration && previous.Generation == snapshot.Generation
	if !rollback && !sameGeneration && snapshot.Generation <= currentGeneration {
		return errors.New("snapshot generation is stale")
	}
	assignments := make(map[string]afdp.AssignmentView)
	services := make(map[string]domain.Service, len(snapshot.Services))
	var gatewaySpec *domain.GatewaySpec
	var agentSpec *domain.AgentSpec
	if snapshot.Gateway != nil {
		value := cloneGatewaySpec(*snapshot.Gateway)
		gatewaySpec = &value
	}
	if snapshot.Agent != nil {
		value := cloneAgentSpec(*snapshot.Agent)
		agentSpec = &value
	}
	for _, service := range snapshot.Services {
		if err := service.Validate(); err != nil {
			return err
		}
		if service.AgentID != e.nodeID && e.kind == domain.NodeSpecAgent {
			continue
		}
		service.GatewaySelector.MatchLabels = cloneStringMap(service.GatewaySelector.MatchLabels)
		services[service.ID] = service
	}
	for _, assignment := range snapshot.Assignments {
		if assignment.Generation > snapshot.Generation {
			return fmt.Errorf("assignment %s generation is newer than snapshot", assignment.ID)
		}
		if e.kind == domain.NodeSpecGateway && assignment.GatewayID != e.nodeID {
			continue
		}
		if e.kind == domain.NodeSpecAgent && assignment.AgentID != e.nodeID {
			continue
		}
		for _, serviceID := range assignment.ServiceIDs {
			service, ok := services[serviceID]
			if !ok {
				return fmt.Errorf("assignment %s references service %s not present in snapshot", assignment.ID, serviceID)
			}
			if service.AgentID != assignment.AgentID {
				return fmt.Errorf("assignment %s references service %s owned by another agent", assignment.ID, serviceID)
			}
		}
		// Derive the assignment ceiling from the immutable node default, not
		// the currently active generation. The latter may be lower because a
		// previous snapshot supplied a tighter Agent/Gateway limit; carrying it
		// into a replacement generation would make limits sticky after they are
		// relaxed by the Controller.
		assignmentLimit := e.baseMaxStreams
		if snapshot.Agent != nil && snapshot.Agent.Limits.MaxStreams > 0 && snapshot.Agent.Limits.MaxStreams < assignmentLimit {
			assignmentLimit = snapshot.Agent.Limits.MaxStreams
		}
		if snapshot.Gateway != nil && snapshot.Gateway.Transport.MaxStreams > 0 && snapshot.Gateway.Transport.MaxStreams < assignmentLimit {
			assignmentLimit = snapshot.Gateway.Transport.MaxStreams
		}
		assignments[assignment.ID] = afdp.AssignmentFromDomain(assignment, assignmentLimit)
	}
	e.mu.Lock()
	if e.closed.Load() {
		e.mu.Unlock()
		return errors.New("data-plane engine is closed")
	}
	// The initial generation check above only covers the time spent building
	// indexes. A direct caller may race another ApplySnapshot during that
	// work; rollback and same-generation repairs are valid only while the
	// expected previous generation is still active.
	if rollback && (previous == nil || e.generation != previous.Generation) {
		e.mu.Unlock()
		return errors.New("snapshot generation changed during rollback")
	}
	if sameGeneration && e.generation != snapshot.Generation {
		e.mu.Unlock()
		return errors.New("snapshot generation changed during resync")
	}
	if !rollback && !sameGeneration && snapshot.Generation <= e.generation {
		e.mu.Unlock()
		return errors.New("snapshot generation is stale")
	}
	e.assignments, e.services, e.generation = assignments, services, snapshot.Generation
	e.epoch++
	// Active counts are generation-scoped. Existing streams retain the
	// aggregate reservation until their lease closes, but a replacement
	// assignment must not inherit the old generation's per-assignment count;
	// stale leases are intentionally prevented from decrementing this map.
	e.active = make(map[string]int, len(assignments))
	e.gatewaySpec, e.agentSpec = gatewaySpec, agentSpec
	e.maxStreams = e.baseMaxStreams
	if agentSpec != nil {
		e.egress = cloneEgressPolicy(agentSpec.Egress)
		e.connectionLimit = agentSpec.Limits.MaxConnections
		e.bufferLimit = agentSpec.Limits.MaxBufferBytes
		if agentSpec.Limits.MaxStreams > 0 && agentSpec.Limits.MaxStreams < e.maxStreams {
			e.maxStreams = agentSpec.Limits.MaxStreams
		}
	} else if gatewaySpec != nil {
		e.egress = cloneEgressPolicy(gatewaySpec.Egress)
		e.connectionLimit = gatewaySpec.Capacity.MaxConnections
		e.bufferLimit = 0
		if gatewaySpec.Transport.MaxStreams > 0 && gatewaySpec.Transport.MaxStreams < e.maxStreams {
			e.maxStreams = gatewaySpec.Transport.MaxStreams
		}
	} else {
		e.egress = domain.EgressPolicy{}
		e.connectionLimit = 0
		e.bufferLimit = 0
	}
	e.mu.Unlock()
	return nil
}

// ResetSnapshot removes a generation that was installed speculatively but
// could not be activated by a sibling data-plane component.  The node
// reconciler uses this only when applying the very first snapshot: there is
// no previous desired document to pass through ApplySnapshot's normal
// rollback path, so leaving the engine index installed would allow a failed
// listener build to admit streams for a generation that is not actually
// serving traffic.
//
// expectedGeneration makes the operation compare-and-swap-like.  A caller
// racing a later successful apply cannot accidentally clear that newer
// generation.  Existing counters are intentionally retained; any in-flight
// reservation belongs to the failed generation and will release through its
// normal close path, while the empty index rejects new admissions.
func (e *Engine) ResetSnapshot(expectedGeneration uint64) error {
	if e == nil {
		return errors.New("data-plane engine is nil")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if expectedGeneration != 0 && e.generation != expectedGeneration {
		return errors.New("data-plane snapshot generation changed during reset")
	}
	e.generation = 0
	e.gatewaySpec = nil
	e.agentSpec = nil
	e.assignments = make(map[string]afdp.AssignmentView)
	e.services = make(map[string]domain.Service)
	e.active = make(map[string]int)
	e.connectionLimit = 0
	e.bufferLimit = 0
	e.maxStreams = e.baseMaxStreams
	e.egress = domain.EgressPolicy{}
	e.epoch++
	// No valid snapshot is active after a failed first apply. Keep admission
	// drained until the next authenticated control stream/snapshot explicitly
	// reopens it.
	e.draining.Store(true)
	return nil
}
