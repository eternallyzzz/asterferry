package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"asterferry/internal/domain"
)

const DefaultGatewayOfflineAfter = 30 * time.Second

type GatewayCandidate struct {
	Node         domain.Node
	Spec         domain.GatewaySpec
	Healthy      bool
	Assignments  []domain.Assignment
	UsedBindings map[string]struct{}
}

type ScheduleRequest struct {
	Agent      domain.Node
	AgentSpec  domain.AgentSpec
	Services   []domain.Service
	Existing   *domain.Assignment
	Generation uint64
}

// ScheduleAgent selects a Gateway from the current SQLite state and commits
// the resulting assignment with the repository's transactional port checks.
// It is intentionally a convenience around Schedule; callers that already
// have a consistent candidate view can use the pure function directly.
func (s *Store) ScheduleAgent(ctx context.Context, agentID string, options WriteOptions) (domain.Assignment, error) {
	agent, err := s.GetNode(ctx, agentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	if agent.Role != domain.RoleAgent || !agent.Enabled {
		return domain.Assignment{}, errors.New("agent is disabled or has the wrong role")
	}
	agentSpec, err := s.GetAgentSpec(ctx, agentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	services, err := s.ListServices(ctx, agentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	// Disabled services remain in the authoritative collection for audit and
	// future re-enable, but they must not be admitted into a live assignment.
	services = filterEnabledServices(services)
	if len(services) == 0 {
		return domain.Assignment{}, errors.New("agent has no services to schedule")
	}
	assignments, err := s.ListAssignments(ctx, "", agentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	var existing *domain.Assignment
	if len(assignments) > 0 {
		candidate := assignments[0]
		existing = &candidate
	}
	nodes, err := s.ListNodes(ctx, domain.RoleGateway)
	if err != nil {
		return domain.Assignment{}, err
	}
	candidates := make([]GatewayCandidate, 0, len(nodes))
	for _, gateway := range nodes {
		if !gateway.Enabled {
			continue
		}
		spec, specErr := s.GetGatewaySpec(ctx, gateway.ID)
		if specErr != nil {
			if errors.Is(specErr, sql.ErrNoRows) {
				continue
			}
			return domain.Assignment{}, specErr
		}
		gatewayAssignments, listErr := s.ListAssignments(ctx, gateway.ID, "")
		if listErr != nil {
			return domain.Assignment{}, listErr
		}
		used := make(map[string]struct{})
		for _, listener := range spec.Listeners {
			used[bindingKey(listener.Protocol, listener.Bind, listener.Port)] = struct{}{}
		}
		for _, assignment := range gatewayAssignments {
			// Degraded and draining assignments have already relinquished their
			// listeners in the node runtime. They must not keep a public port
			// reserved while failover is selecting a replacement.
			if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
				continue
			}
			for _, binding := range assignment.Bindings {
				used[bindingKey(binding.Protocol, binding.Bind, binding.Port)] = struct{}{}
			}
		}
		// A missing observed row is treated as provisionally healthy. Once a
		// node reports a degraded state it is removed from future placement.
		healthy := true
		if state, observedErr := s.GetObserved(ctx, gateway.ID); observedErr == nil {
			healthy = state.Healthy && !state.Degraded && (state.ObservedAt.IsZero() || time.Since(state.ObservedAt) < DefaultGatewayOfflineAfter)
		} else if !errors.Is(observedErr, sql.ErrNoRows) {
			return domain.Assignment{}, observedErr
		}
		candidates = append(candidates, GatewayCandidate{Node: gateway, Spec: spec, Healthy: healthy, Assignments: gatewayAssignments, UsedBindings: used})
	}
	generation := uint64(1)
	if previous, previousErr := s.LoadSnapshot(ctx, agentID); previousErr == nil {
		if previous.Generation == ^uint64(0) {
			return domain.Assignment{}, errors.New("desired snapshot generation is exhausted")
		}
		generation = previous.Generation + 1
	} else if !errors.Is(previousErr, sql.ErrNoRows) {
		return domain.Assignment{}, previousErr
	}
	if existing != nil && existing.Generation >= generation {
		if existing.Generation == ^uint64(0) {
			return domain.Assignment{}, errors.New("assignment generation is exhausted")
		}
		generation = existing.Generation + 1
	}
	assignment, err := Schedule(ScheduleRequest{Agent: agent, AgentSpec: agentSpec, Services: services, Existing: existing, Generation: generation}, candidates)
	if err != nil {
		return domain.Assignment{}, err
	}
	// Scheduling an already healthy, unchanged assignment is a read-like
	// operation. Preserve its generation and revision instead of creating a new
	// control generation on every retry (which also makes API idempotency keys
	// useful for clients that retry a timeout).
	if existing != nil && existing.State != domain.AssignmentDegraded && existing.State != domain.AssignmentDraining && sameAssignmentPlacement(*existing, assignment) {
		if _, err := s.EnsureDesiredSnapshot(ctx, existing.GatewayID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Assignment{}, err
		}
		if _, err := s.EnsureDesiredSnapshot(ctx, existing.AgentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Assignment{}, err
		}
		return *existing, nil
	}
	writeOptions := options
	if existing != nil {
		// Keep the assignment identity stable across a Gateway failover.  The
		// repository can then update the old row and release its bindings in one
		// transaction instead of deleting the old placement first (which would
		// expose a window where another writer could steal the services/ports).
		assignment.ID = existing.ID
		writeOptions.IfMatch = existing.Revision
	} else {
		writeOptions.IfMatch = 0
	}
	if err := s.PutAssignment(ctx, assignment, writeOptions); err != nil {
		return domain.Assignment{}, err
	}
	if existing != nil && existing.GatewayID != assignment.GatewayID {
		// The assignment row is replaced atomically, but the old Gateway's
		// node-scoped snapshot also needs to release its binding.
		if _, err := s.EnsureDesiredSnapshot(ctx, existing.GatewayID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Assignment{}, err
		}
	}
	if _, err := s.EnsureDesiredSnapshot(ctx, assignment.GatewayID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, err
	}
	if _, err := s.EnsureDesiredSnapshot(ctx, assignment.AgentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, err
	}
	return s.GetAssignment(ctx, assignment.ID)
}

// ReconcileAssignments marks assignments whose Gateway has stopped reporting
// health and attempts a stable-identity failover for their Agents. It is
// deliberately conservative when no observed state exists: a freshly
// enrolled Gateway has not yet proven liveness, but treating that absence as
// failure would cause needless placement churn. A caller may run this method
// periodically; each successful failover updates both node snapshots.
func (s *Store) ReconcileAssignments(ctx context.Context, offlineAfter time.Duration) ([]domain.Assignment, error) {
	if offlineAfter <= 0 {
		offlineAfter = DefaultGatewayOfflineAfter
	}
	assignments, err := s.ListAssignments(ctx, "", "")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	result := make([]domain.Assignment, 0)
	for _, assignment := range assignments {
		if assignment.State == domain.AssignmentDraining {
			continue
		}
		observed, observedErr := s.GetObserved(ctx, assignment.GatewayID)
		if errors.Is(observedErr, sql.ErrNoRows) {
			continue
		}
		if observedErr != nil {
			return nil, observedErr
		}
		stale := observed.Degraded || !observed.Healthy || observed.ObservedAt.IsZero() || now.Sub(observed.ObservedAt) >= offlineAfter
		if !stale {
			continue
		}
		if assignment.State != domain.AssignmentDegraded {
			updated, stateErr := s.UpdateAssignmentState(ctx, assignment.ID, domain.AssignmentDegraded, WriteOptions{IfMatch: assignment.Revision, Actor: "system"})
			if stateErr != nil {
				if IsRevisionConflict(stateErr) {
					continue
				}
				return nil, stateErr
			}
			assignment = updated
			// Publish the degraded state even when no replacement Gateway is
			// currently available; the next reconciliation pass may then retry
			// placement without leaving either node with a stale assignment.
			if _, snapshotErr := s.EnsureDesiredSnapshot(ctx, assignment.GatewayID); snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
				return nil, snapshotErr
			}
			if _, snapshotErr := s.EnsureDesiredSnapshot(ctx, assignment.AgentID); snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
				return nil, snapshotErr
			}
		}
		failedOver, scheduleErr := s.ScheduleAgent(ctx, assignment.AgentID, WriteOptions{Actor: "system"})
		if scheduleErr != nil {
			// No healthy replacement is a normal degraded condition. Keep the
			// assignment state and let the next pass retry after another Gateway
			// becomes healthy.
			continue
		}
		result = append(result, failedOver)
	}
	return result, nil
}

type PortConflictError struct {
	GatewayID string
	Protocol  string
	Bind      string
	Port      uint16
}

func (e *PortConflictError) Error() string {
	return fmt.Sprintf("public %s binding %s:%d is already allocated on gateway %s", e.Protocol, e.Bind, e.Port, e.GatewayID)
}

func Schedule(request ScheduleRequest, candidates []GatewayCandidate) (domain.Assignment, error) {
	if err := domain.ValidateID(request.Agent.ID, "agent.id"); err != nil {
		return domain.Assignment{}, err
	}
	if request.Agent.Role != domain.RoleAgent || request.Generation == 0 {
		return domain.Assignment{}, errors.New("agent and positive generation are required")
	}
	if request.AgentSpec.NodeID == "" {
		request.AgentSpec.NodeID = request.Agent.ID
	}
	if err := request.AgentSpec.Validate(); err != nil {
		return domain.Assignment{}, err
	}
	if request.AgentSpec.NodeID != request.Agent.ID {
		return domain.Assignment{}, errors.New("agent spec does not belong to agent")
	}
	if len(request.Services) == 0 {
		return domain.Assignment{}, errors.New("at least one service is required")
	}
	if request.Existing != nil && (request.Existing.State == domain.AssignmentDegraded || request.Existing.State == domain.AssignmentDraining) {
		// A degraded or draining assignment is not considered sticky. It may
		// still contribute occupancy through the candidate list, but a healthy
		// Gateway should receive a fresh assignment ID and bindings.
		request.Existing = nil
	}
	seenServiceIDs := make(map[string]struct{}, len(request.Services))
	for _, service := range request.Services {
		if _, exists := seenServiceIDs[service.ID]; exists {
			return domain.Assignment{}, fmt.Errorf("service %s is duplicated", service.ID)
		}
		if service.AgentID != request.Agent.ID {
			return domain.Assignment{}, fmt.Errorf("service %s belongs to another agent", service.ID)
		}
		seenServiceIDs[service.ID] = struct{}{}
	}
	ordered := append([]GatewayCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left := ordered[i]
		right := ordered[j]
		if request.Existing != nil {
			leftExisting := left.Node.ID == request.Existing.GatewayID
			rightExisting := right.Node.ID == request.Existing.GatewayID
			if leftExisting != rightExisting {
				return leftExisting
			}
		}
		leftLoad := len(left.Assignments)*1000 + left.Spec.Capacity.UsedServices
		rightLoad := len(right.Assignments)*1000 + right.Spec.Capacity.UsedServices
		if leftLoad != rightLoad {
			return leftLoad < rightLoad
		}
		return left.Node.ID < right.Node.ID
	})
	var explicitConflict error
	var placementErr error
	for _, candidate := range ordered {
		if !candidate.Node.Enabled || candidate.Node.Role != domain.RoleGateway || !candidate.Healthy || !request.AgentSpec.GatewaySelector.Matches(candidate.Node.Labels) {
			continue
		}
		usedAgents := candidate.Spec.Capacity.UsedAgents
		usedServices := candidate.Spec.Capacity.UsedServices
		// Capacity counters may be stale or omitted in a freshly published
		// GatewaySpec. Never under-count the assignments already present in the
		// candidate view; use the larger of the reported and derived values.
		derivedAgents := make(map[string]struct{})
		derivedServices := 0
		for _, current := range candidate.Assignments {
			if current.State == domain.AssignmentDegraded || current.State == domain.AssignmentDraining {
				continue
			}
			derivedAgents[current.AgentID] = struct{}{}
			derivedServices += len(current.ServiceIDs)
		}
		if len(derivedAgents) > usedAgents {
			usedAgents = len(derivedAgents)
		}
		if derivedServices > usedServices {
			usedServices = derivedServices
		}
		// A reschedule that keeps the current Gateway must not count its own
		// assignment against capacity or port occupancy. Otherwise a stable
		// assignment can fail simply because its already allocated port appears
		// occupied by itself.
		var existingBindings map[string]domain.Binding
		if request.Existing != nil && request.Existing.GatewayID == candidate.Node.ID && request.Existing.AgentID == request.Agent.ID {
			if usedAgents > 0 {
				usedAgents--
			}
			if usedServices >= len(request.Existing.ServiceIDs) {
				usedServices -= len(request.Existing.ServiceIDs)
			} else {
				usedServices = 0
			}
			existingBindings = make(map[string]domain.Binding, len(request.Existing.Bindings))
			// Candidate slices and maps are caller-owned. Never mutate the
			// occupancy view while temporarily releasing this assignment's own
			// ports for a stable reschedule.
			candidate.UsedBindings = cloneBindingSet(candidate.UsedBindings)
			for _, binding := range request.Existing.Bindings {
				existingBindings[binding.ServiceID] = binding
				delete(candidate.UsedBindings, bindingKey(binding.Protocol, binding.Bind, binding.Port))
			}
		}
		if candidate.Spec.Capacity.MaxAgents > 0 && usedAgents >= candidate.Spec.Capacity.MaxAgents {
			continue
		}
		if candidate.Spec.Capacity.MaxServices > 0 && usedServices+len(request.Services) > candidate.Spec.Capacity.MaxServices {
			continue
		}
		// MaxConnections is a runtime capacity advertised by the Gateway. It
		// cannot be derived from the assignment list (connections are dynamic),
		// but an observed counter at or above the limit is a hard placement
		// rejection. This keeps a saturated Gateway out of failover selection
		// without treating a missing/zero counter as a reason to churn healthy
		// assignments.
		if candidate.Spec.Capacity.MaxConnections > 0 && candidate.Spec.Capacity.UsedConnections >= candidate.Spec.Capacity.MaxConnections {
			continue
		}
		bindings := cloneBindingSet(candidate.UsedBindings)
		if bindings == nil {
			bindings = make(map[string]struct{})
		}
		for _, existing := range candidate.Assignments {
			if request.Existing != nil && existing.ID == request.Existing.ID {
				continue
			}
			for _, binding := range existing.Bindings {
				bindings[bindingKey(binding.Protocol, binding.Bind, binding.Port)] = struct{}{}
			}
		}
		serviceIDs := make([]string, 0, len(request.Services))
		serviceBindings := make([]domain.Binding, 0, len(request.Services))
		valid := true
		var endpoint string
		for index := range request.Services {
			service := request.Services[index]
			if err := service.Validate(); err != nil {
				return domain.Assignment{}, fmt.Errorf("service %s: %w", service.ID, err)
			}
			if !service.GatewaySelector.Matches(candidate.Node.Labels) {
				valid = false
				if placementErr == nil {
					placementErr = fmt.Errorf("gateway %s does not match service %s selector", candidate.Node.ID, service.ID)
				}
				break
			}
			var port uint16
			var err error
			if existingBinding, ok := existingBindings[service.ID]; ok && existingBinding.Protocol == service.Protocol && normalizeBind(existingBinding.Bind) == normalizeBind(service.PublicBind) && (service.PublicPort == 0 || service.PublicPort == existingBinding.Port) {
				if _, occupied := bindings[bindingKey(existingBinding.Protocol, existingBinding.Bind, existingBinding.Port)]; !occupied {
					port = existingBinding.Port
				}
			}
			if port == 0 {
				port, err = allocatePort(candidate.Spec.PortPool, service.Protocol, service.PublicBind, service.PublicPort, bindings)
			}
			if err != nil {
				var conflict *PortConflictError
				if errors.As(err, &conflict) {
					conflict.GatewayID = candidate.Node.ID
					conflict.Bind = service.PublicBind
					if service.PublicPort != 0 {
						explicitConflict = conflict
					}
					valid = false
					break
				}
				// A candidate's pool may not contain a requested port (or may be
				// exhausted). Try the next independent Gateway before failing the
				// placement as a whole.
				if placementErr == nil {
					placementErr = err
				}
				valid = false
				break
			}
			key := bindingKey(service.Protocol, service.PublicBind, port)
			bindings[key] = struct{}{}
			serviceIDs = append(serviceIDs, service.ID)
			serviceBindings = append(serviceBindings, domain.Binding{ServiceID: service.ID, Protocol: service.Protocol, Bind: service.PublicBind, Port: port})
			if endpoint == "" && len(candidate.Spec.PublicEndpoints) > 0 {
				endpoint = candidate.Spec.PublicEndpoints[0]
			}
		}
		if !valid {
			continue
		}
		assignmentID := fmt.Sprintf("%s-%s", request.Agent.ID, candidate.Node.ID)
		generation := request.Generation
		if request.Existing != nil && request.Existing.GatewayID == candidate.Node.ID && request.Existing.AgentID == request.Agent.ID {
			assignmentID = request.Existing.ID
		}
		return domain.Assignment{ID: assignmentID, GatewayID: candidate.Node.ID, AgentID: request.Agent.ID, ServiceIDs: serviceIDs, Bindings: serviceBindings, Generation: generation, State: domain.AssignmentPending, PublicEndpoint: endpoint, UpdatedAt: time.Now().UTC()}, nil
	}
	if explicitConflict != nil {
		return domain.Assignment{}, explicitConflict
	}
	if placementErr != nil {
		return domain.Assignment{}, placementErr
	}
	return domain.Assignment{}, errors.New("no healthy gateway satisfies the selector and capacity constraints")
}

func filterEnabledServices(values []domain.Service) []domain.Service {
	result := make([]domain.Service, 0, len(values))
	for _, service := range values {
		if service.Enabled {
			result = append(result, service)
		}
	}
	return result
}

func sameAssignmentPlacement(left, right domain.Assignment) bool {
	if left.ID != right.ID || left.GatewayID != right.GatewayID || left.AgentID != right.AgentID || left.PublicEndpoint != right.PublicEndpoint || len(left.ServiceIDs) != len(right.ServiceIDs) || len(left.Bindings) != len(right.Bindings) {
		return false
	}
	leftServices := append([]string(nil), left.ServiceIDs...)
	rightServices := append([]string(nil), right.ServiceIDs...)
	sort.Strings(leftServices)
	sort.Strings(rightServices)
	for i := range leftServices {
		if leftServices[i] != rightServices[i] {
			return false
		}
	}
	leftBindings := append([]domain.Binding(nil), left.Bindings...)
	rightBindings := append([]domain.Binding(nil), right.Bindings...)
	sort.Slice(leftBindings, func(i, j int) bool {
		return bindingKey(leftBindings[i].Protocol, leftBindings[i].Bind, leftBindings[i].Port) < bindingKey(leftBindings[j].Protocol, leftBindings[j].Bind, leftBindings[j].Port)
	})
	sort.Slice(rightBindings, func(i, j int) bool {
		return bindingKey(rightBindings[i].Protocol, rightBindings[i].Bind, rightBindings[i].Port) < bindingKey(rightBindings[j].Protocol, rightBindings[j].Bind, rightBindings[j].Port)
	})
	for i := range leftBindings {
		if leftBindings[i].ServiceID != rightBindings[i].ServiceID || leftBindings[i].Protocol != rightBindings[i].Protocol || normalizeBind(leftBindings[i].Bind) != normalizeBind(rightBindings[i].Bind) || leftBindings[i].Port != rightBindings[i].Port {
			return false
		}
	}
	return true
}

func allocatePort(pool domain.PortPool, protocol, bind string, requested uint16, used map[string]struct{}) (uint16, error) {
	if requested != 0 {
		key := bindingKey(protocol, bind, requested)
		if _, exists := used[key]; exists {
			return 0, &PortConflictError{Protocol: protocol, Bind: bind, Port: requested}
		}
		if !portInPool(pool, protocol, requested) {
			return 0, fmt.Errorf("requested port %d is not in gateway %s port pool", requested, protocol)
		}
		return requested, nil
	}
	var ranges []domain.PortRange
	if protocol == domain.ProtocolTCP {
		ranges = pool.TCP
	} else {
		ranges = pool.UDP
	}
	for _, r := range ranges {
		for port := uint32(r.Min); port <= uint32(r.Max); port++ {
			candidate := uint16(port)
			if _, exists := used[bindingKey(protocol, bind, candidate)]; !exists {
				return candidate, nil
			}
		}
	}
	return 0, errors.New("gateway port pool is exhausted")
}

func portInPool(pool domain.PortPool, protocol string, port uint16) bool {
	var ranges []domain.PortRange
	if protocol == domain.ProtocolTCP {
		ranges = pool.TCP
	} else {
		ranges = pool.UDP
	}
	if len(ranges) == 0 {
		return true
	}
	for _, r := range ranges {
		if port >= r.Min && port <= r.Max {
			return true
		}
	}
	return false
}

func bindingKey(protocol, bind string, port uint16) string {
	bind = strings.TrimSpace(bind)
	if address, err := netip.ParseAddr(bind); err == nil {
		bind = address.Unmap().String()
	}
	return fmt.Sprintf("%s|%s|%d", protocol, bind, port)
}

func cloneBindingSet(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return nil
	}
	result := make(map[string]struct{}, len(source))
	for key := range source {
		result[key] = struct{}{}
	}
	return result
}
