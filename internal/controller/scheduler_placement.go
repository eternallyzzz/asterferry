package controller

import (
	"asterferry/internal/domain"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (s *ResourceRepository) LoadGatewayCandidates(ctx context.Context) ([]GatewayCandidate, error) {
	nodes, err := s.ListNodes(ctx, string(domain.NodeSpecGateway))
	if err != nil {
		return nil, err
	}
	eligible := make([]domain.Node, 0, len(nodes))
	for _, gateway := range nodes {
		if gateway.Enabled && gateway.CertificateState != domain.CertificateRevoked && gateway.CertificateState != domain.CertificateExpired {
			eligible = append(eligible, gateway)
		}
	}
	if len(eligible) == 0 {
		return nil, nil
	}

	// Candidate loading is deliberately set based. The number of reads is
	// bounded by the backend parameter limit, not by the number of Gateways.
	specs, err := s.loadGatewaySpecsBatch(ctx, eligible)
	if err != nil {
		return nil, err
	}
	assignments, err := s.loadGatewayAssignmentsBatch(ctx, eligible)
	if err != nil {
		return nil, err
	}
	observed, err := s.loadGatewayObservedBatch(ctx, eligible)
	if err != nil {
		return nil, err
	}

	candidates := make([]GatewayCandidate, 0, len(eligible))
	for _, gateway := range eligible {
		nodeSpec, ok := specs[gateway.ID]
		if !ok || nodeSpec.Kind != domain.NodeSpecGateway || nodeSpec.Gateway == nil {
			continue
		}
		spec := *nodeSpec.Gateway
		spec.Revision = nodeSpec.Revision
		gatewayAssignments := assignments[gateway.ID]
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
		// node reports a degraded or stale state it is removed from placement.
		healthy := true
		if state, ok := observed[gateway.ID]; ok {
			healthy = state.Healthy && !state.Degraded && (state.ObservedAt.IsZero() || time.Since(state.ObservedAt) < DefaultGatewayOfflineAfter)
		}
		candidates = append(candidates, GatewayCandidate{Node: gateway, Spec: spec, Healthy: healthy, Assignments: gatewayAssignments, UsedBindings: used})
	}
	return candidates, nil
}

const gatewayCandidateBatchSize = 500

func (s *ResourceRepository) loadGatewaySpecsBatch(ctx context.Context, gateways []domain.Node) (map[string]domain.NodeSpec, error) {
	return loadGatewaySpecsBatchNormalized(ctx, s.db, gateways)
}

func (s *ResourceRepository) loadGatewayAssignmentsBatch(ctx context.Context, gateways []domain.Node) (map[string][]domain.Assignment, error) {
	return loadAssignmentsBatchNormalized(ctx, s.db, gateways)
}

func (s *ResourceRepository) loadGatewayObservedBatch(ctx context.Context, gateways []domain.Node) (map[string]domain.ObservedState, error) {
	return loadObservedBatchNormalized(ctx, s.db, gateways)
}

func questionMarks(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (s *ResourceRepository) AllocateAssignmentID(ctx context.Context, assignment domain.Assignment) (string, error) {
	base := assignment.ID
	if err := domain.ValidateID(base, "assignment.id"); err != nil {
		return "", err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assignments WHERE id=?`, base).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return base, nil
	} else if err != nil {
		return "", err
	}
	serviceIDs := append([]string(nil), assignment.ServiceIDs...)
	sort.Strings(serviceIDs)
	digest := sha256.Sum256([]byte(strings.Join(serviceIDs, "\x00")))
	for suffix := 6; suffix <= len(digest); suffix += 2 {
		hexSuffix := hex.EncodeToString(digest[:suffix])
		candidateBase := base
		if maxBaseLength := 128 - 1 - len(hexSuffix); len(candidateBase) > maxBaseLength {
			candidateBase = candidateBase[:maxBaseLength]
		}
		candidate := candidateBase + "-" + hexSuffix
		if err := domain.ValidateID(candidate, "assignment.id"); err != nil {
			return "", err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assignments WHERE id=?`, candidate).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique assignment id")
}

func scopedScheduleWriteOptions(options WriteOptions, assignmentID string) WriteOptions {
	key := strings.TrimSpace(options.IdempotencyKey)
	if key == "" {
		return options
	}
	digest := sha256.Sum256([]byte(key + "\x00" + assignmentID))
	options.IdempotencyKey = "schedule-" + hex.EncodeToString(digest[:])
	return options
}

func Schedule(request ScheduleRequest, candidates []GatewayCandidate) (domain.Assignment, error) {
	if err := domain.ValidateID(request.Agent.ID, "agent.id"); err != nil {
		return domain.Assignment{}, err
	}
	if request.Generation == 0 {
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
		leftServices := activeAssignmentServiceCount(left.Assignments)
		rightServices := activeAssignmentServiceCount(right.Assignments)
		leftLoad := len(activeAssignmentAgents(left.Assignments))*1000 + leftServices
		rightLoad := len(activeAssignmentAgents(right.Assignments))*1000 + rightServices
		if leftLoad != rightLoad {
			return leftLoad < rightLoad
		}
		return left.Node.ID < right.Node.ID
	})
	var explicitConflict error
	var placementErr error
	for _, candidate := range ordered {
		if !candidate.Node.Enabled || candidate.Node.CertificateState == domain.CertificateRevoked || candidate.Node.CertificateState == domain.CertificateExpired || candidate.Node.SpecKind != domain.NodeSpecGateway || !candidate.Healthy || !request.AgentSpec.GatewaySelector.Matches(candidate.Node.Labels) {
			continue
		}
		usedAgents := len(activeAssignmentAgents(candidate.Assignments))
		usedServices := activeAssignmentServiceCount(candidate.Assignments)
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
		bindings := cloneBindingSet(candidate.UsedBindings)
		if bindings == nil {
			bindings = make(map[string]struct{})
		}
		for _, existing := range candidate.Assignments {
			if existing.State == domain.AssignmentDegraded || existing.State == domain.AssignmentDraining {
				// These placements have relinquished their listeners. Keep their
				// documents for diagnostics, but never let them block failover.
				continue
			}
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
		return domain.Assignment{ID: assignmentID, GatewayID: candidate.Node.ID, AgentID: request.Agent.ID, ServiceIDs: serviceIDs, Bindings: serviceBindings, Generation: generation, State: domain.AssignmentPending, PublicEndpoint: endpoint, Obfuscation: candidate.Spec.Obfuscation, UpdatedAt: time.Now().UTC()}, nil
	}
	if explicitConflict != nil {
		return domain.Assignment{}, explicitConflict
	}
	if placementErr != nil {
		return domain.Assignment{}, placementErr
	}
	return domain.Assignment{}, ErrNoHealthyGateway
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

func activeAssignmentAgents(values []domain.Assignment) map[string]struct{} {
	result := make(map[string]struct{})
	for _, assignment := range values {
		if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
			continue
		}
		result[assignment.AgentID] = struct{}{}
	}
	return result
}

func activeAssignmentServiceCount(values []domain.Assignment) int {
	count := 0
	for _, assignment := range values {
		if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
			continue
		}
		count += len(assignment.ServiceIDs)
	}
	return count
}

func sameAssignmentPlacement(left, right domain.Assignment) bool {
	if left.ID != right.ID || left.GatewayID != right.GatewayID || left.AgentID != right.AgentID || left.PublicEndpoint != right.PublicEndpoint || !sameObfuscationPolicy(left.Obfuscation, right.Obfuscation) || len(left.ServiceIDs) != len(right.ServiceIDs) || len(left.Bindings) != len(right.Bindings) {
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
