package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

const DefaultGatewayOfflineAfter = 30 * time.Second

var ErrNoHealthyGateway = errors.New("no healthy gateway satisfies the fixed binding, selector, and capacity constraints")

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

type scheduleTarget struct {
	existing *domain.Assignment
	services []domain.Service
}

// ScheduleAgent selects a Gateway from the current Controller state and commits
// the resulting assignment with the repository's transactional port checks.
// It is intentionally a convenience around Schedule; callers that already
// have a consistent candidate view can use the pure function directly.
func (s *Scheduler) ScheduleAgent(ctx context.Context, agentID string, options WriteOptions) (assignments []domain.Assignment, returnErr error) {
	finishMetrics := s.startMetrics()
	defer func() { finishMetrics(returnErr) }()
	if err := validateIdempotencyKey(strings.TrimSpace(options.IdempotencyKey)); err != nil {
		return nil, err
	}
	agent, err := s.repository.GetNode(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if !agent.Enabled {
		return nil, errors.New("agent is disabled")
	}
	nodeSpec, err := s.repository.GetNodeSpec(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if nodeSpec.Kind != domain.NodeSpecAgent || nodeSpec.Agent == nil {
		return nil, errors.New("node is not configured as an agent")
	}
	agentSpec := *nodeSpec.Agent
	agentSpec.Revision = nodeSpec.Revision
	services, err := s.repository.ListServices(ctx, agentID)
	if err != nil {
		return nil, err
	}
	// Disabled services remain in the authoritative collection for audit and
	// future re-enable, but they must not be admitted into a live assignment.
	services = filterEnabledServices(services)
	if len(services) == 0 {
		return nil, errors.New("agent has no services to schedule")
	}
	currentAssignments, err := s.repository.ListAssignments(ctx, "", agentID)
	if err != nil {
		return nil, err
	}
	serviceByID := make(map[string]domain.Service, len(services))
	for _, service := range services {
		serviceByID[service.ID] = service
	}
	assignedServices := make(map[string]struct{}, len(services))
	targets := make([]scheduleTarget, 0, len(currentAssignments)+1)
	for index := range currentAssignments {
		current := currentAssignments[index]
		candidateServices := servicesForAssignment(current, serviceByID)
		if len(candidateServices) == 0 {
			continue
		}
		for _, service := range candidateServices {
			assignedServices[service.ID] = struct{}{}
		}
		existing := current
		targets = append(targets, scheduleTarget{existing: &existing, services: candidateServices})
	}
	unassigned := make([]domain.Service, 0, len(services))
	for _, service := range services {
		if _, assigned := assignedServices[service.ID]; !assigned {
			unassigned = append(unassigned, service)
		}
	}
	if len(unassigned) > 0 {
		targets = append(targets, scheduleTarget{services: unassigned})
	}
	for _, target := range targets {
		if _, err := s.scheduleAgentAssignment(ctx, agent, agentSpec, target.services, target.existing, options); err != nil {
			return nil, err
		}
	}
	return s.repository.ListAssignments(ctx, "", agentID)
}

// ReconcileAssignmentsForAgents schedules newly-created services as well as
// repairing an Agent's existing placements. Resource notifications identify
// the affected Agent, so normal service creation no longer requires a second
// manual schedule action from the operator.
func (s *Scheduler) ReconcileAssignmentsForAgents(ctx context.Context, agentIDs ...string) (result []domain.Assignment, returnErr error) {
	seen := make(map[string]struct{}, len(agentIDs))
	result = make([]domain.Assignment, 0)
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			continue
		}
		if _, exists := seen[agentID]; exists {
			continue
		}
		seen[agentID] = struct{}{}
		node, err := s.repository.GetNode(ctx, agentID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		nodeSpec, specErr := s.repository.GetNodeSpec(ctx, agentID)
		if specErr != nil && !errors.Is(specErr, sql.ErrNoRows) {
			return nil, specErr
		}
		if specErr != nil || nodeSpec.Kind != domain.NodeSpecAgent || !node.Enabled {
			continue
		}
		services, err := s.repository.ListServices(ctx, agentID)
		if err != nil {
			return nil, err
		}
		if len(filterEnabledServices(services)) == 0 {
			continue
		}
		// This is an internal repair pass, not an API retry boundary. A stable
		// idempotency key would make a later service change collide with the
		// old reconciliation request hash, so deliberately leave it empty.
		assignments, err := s.ScheduleAgent(ctx, agentID, WriteOptions{Actor: "system"})
		if errors.Is(err, ErrNoHealthyGateway) {
			// Pending services will be retried when a Gateway resource changes
			// and by the periodic repair sweep.
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, assignments...)
	}
	return result, nil
}

// ReconcilePendingServices retries enabled services that do not yet have an
// assignment. It is used when a Gateway first becomes available, because the
// service may have been created while no Gateway matched its selector.
func (s *Scheduler) ReconcilePendingServices(ctx context.Context) (result []domain.Assignment, returnErr error) {
	finishMetrics := s.startMetrics()
	defer func() { finishMetrics(returnErr) }()
	nodes, err := s.repository.ListNodes(ctx, string(domain.NodeSpecAgent))
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		// Reuse the regular reconciliation path so pending detection and
		// scheduling apply the same validation rules as an operator request.
		assignments, scheduleErr := s.ReconcileAssignmentsForAgents(ctx, node.ID)
		if scheduleErr != nil {
			return result, scheduleErr
		}
		result = append(result, assignments...)
	}
	return result, nil
}

func servicesForAssignment(assignment domain.Assignment, serviceByID map[string]domain.Service) []domain.Service {
	services := make([]domain.Service, 0, len(assignment.ServiceIDs))
	for _, serviceID := range assignment.ServiceIDs {
		if service, ok := serviceByID[serviceID]; ok {
			services = append(services, service)
		}
	}
	sort.SliceStable(services, func(i, j int) bool { return services[i].ID < services[j].ID })
	return services
}

const maxScheduleAttempts = 2

func (s *Scheduler) scheduleAgentAssignment(ctx context.Context, agent domain.Node, agentSpec domain.AgentSpec, services []domain.Service, existing *domain.Assignment, options WriteOptions) (domain.Assignment, error) {
	for attempt := 0; attempt < maxScheduleAttempts; attempt++ {
		attemptExisting := existing
		if attempt > 0 && existing != nil {
			// A port conflict means the candidate view was stale. Refresh the
			// assignment too so a concurrent replacement cannot turn the retry
			// into an avoidable revision conflict.
			refreshed, err := s.repository.GetAssignment(ctx, existing.ID)
			switch {
			case err == nil:
				attemptExisting = &refreshed
			case errors.Is(err, sql.ErrNoRows):
				attemptExisting = nil
			default:
				return domain.Assignment{}, err
			}
		}
		assignment, err := s.scheduleAgentAssignmentAttempt(ctx, agent, agentSpec, services, attemptExisting, options)
		if err == nil {
			return assignment, nil
		}
		var portConflict *PortConflictError
		if !errors.As(err, &portConflict) || attempt+1 >= maxScheduleAttempts {
			return domain.Assignment{}, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return domain.Assignment{}, ctxErr
		}
	}
	return domain.Assignment{}, errors.New("scheduling attempts exhausted")
}

func (s *Scheduler) scheduleAgentAssignmentAttempt(ctx context.Context, agent domain.Node, agentSpec domain.AgentSpec, services []domain.Service, existing *domain.Assignment, options WriteOptions) (domain.Assignment, error) {
	candidates, err := s.gatewayCandidates(ctx)
	if err != nil {
		return domain.Assignment{}, err
	}
	generation := uint64(1)
	if previous, previousErr := s.repository.LoadSnapshot(ctx, agent.ID); previousErr == nil {
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
	// Scheduling an already healthy, unchanged placement is a read-like
	// operation. Compare the key identity as well as the network placement so
	// a manual schedule can repair a missed Gateway key-rotation propagation.
	if existing != nil && existing.State != domain.AssignmentDegraded && existing.State != domain.AssignmentDraining && sameAssignmentPlacement(*existing, assignment) {
		if _, err := s.repository.EnsureDesiredSnapshot(ctx, existing.GatewayID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Assignment{}, err
		}
		if _, err := s.repository.EnsureDesiredSnapshot(ctx, existing.AgentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Assignment{}, err
		}
		return *existing, nil
	}
	writeOptions := options
	if existing != nil {
		// Keep the assignment identity stable across a Gateway failover. The
		// repository can then update the old row and release its bindings in one
		// transaction instead of deleting the old placement first.
		assignment.ID = existing.ID
		writeOptions.IfMatch = existing.Revision
	} else {
		writeOptions.IfMatch = 0
		assignment.ID, err = s.repository.AllocateAssignmentID(ctx, assignment)
		if err != nil {
			return domain.Assignment{}, err
		}
	}
	writeOptions = scopedScheduleWriteOptions(writeOptions, assignment.ID)
	if err := s.repository.PutAssignment(ctx, assignment, writeOptions); err != nil {
		return domain.Assignment{}, err
	}
	if existing != nil && existing.GatewayID != assignment.GatewayID {
		// The assignment row is replaced atomically, but the old Gateway's
		// node-scoped snapshot also needs to release its binding.
		if _, err := s.repository.EnsureDesiredSnapshot(ctx, existing.GatewayID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Assignment{}, err
		}
	}
	if _, err := s.repository.EnsureDesiredSnapshot(ctx, assignment.GatewayID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, err
	}
	if _, err := s.repository.EnsureDesiredSnapshot(ctx, assignment.AgentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, err
	}
	return s.repository.GetAssignment(ctx, assignment.ID)
}

func (s *Scheduler) scheduleExistingAssignment(ctx context.Context, existing domain.Assignment, options WriteOptions) (domain.Assignment, error) {
	agent, err := s.repository.GetNode(ctx, existing.AgentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	nodeSpec, err := s.repository.GetNodeSpec(ctx, existing.AgentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	if nodeSpec.Kind != domain.NodeSpecAgent || nodeSpec.Agent == nil {
		return domain.Assignment{}, errors.New("node is not configured as an agent")
	}
	agentSpec := *nodeSpec.Agent
	agentSpec.Revision = nodeSpec.Revision
	services, err := s.repository.ListServices(ctx, existing.AgentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	serviceByID := make(map[string]domain.Service, len(services))
	for _, service := range filterEnabledServices(services) {
		serviceByID[service.ID] = service
	}
	selected := servicesForAssignment(existing, serviceByID)
	if len(selected) == 0 {
		return domain.Assignment{}, errors.New("assignment has no enabled services to schedule")
	}
	return s.scheduleAgentAssignment(ctx, agent, agentSpec, selected, &existing, options)
}

// ReconcileAssignments marks assignments whose Gateway has stopped reporting
// health and attempts a stable-identity failover for their Agents. It is
// deliberately conservative when no observed state exists: a freshly
// enrolled Gateway has not yet proven liveness, but treating that absence as
// failure would cause needless placement churn. A caller may run this method
// periodically; each successful failover updates both node snapshots.
func (s *Scheduler) ReconcileAssignments(ctx context.Context, offlineAfter time.Duration) (result []domain.Assignment, returnErr error) {
	finishMetrics := s.startMetrics()
	defer func() { finishMetrics(returnErr) }()
	if offlineAfter <= 0 {
		offlineAfter = DefaultGatewayOfflineAfter
	}
	assignments, err := s.repository.ListAssignments(ctx, "", "")
	if err != nil {
		return nil, err
	}
	return s.reconcileAssignmentSet(ctx, assignments, offlineAfter)
}

// ReconcileAssignmentsForGateways is the event-driven counterpart to the
// safety-sweep method above. Heartbeats and resource writes identify the
// Gateway whose placements may need work, so normal operation avoids reading
// every assignment in the database.
func (s *Scheduler) ReconcileAssignmentsForGateways(ctx context.Context, offlineAfter time.Duration, gatewayIDs ...string) (result []domain.Assignment, returnErr error) {
	finishMetrics := s.startMetrics()
	defer func() { finishMetrics(returnErr) }()
	if offlineAfter <= 0 {
		offlineAfter = DefaultGatewayOfflineAfter
	}
	seen := make(map[string]struct{}, len(gatewayIDs))
	assignments := make([]domain.Assignment, 0)
	for _, gatewayID := range gatewayIDs {
		gatewayID = strings.TrimSpace(gatewayID)
		if gatewayID == "" {
			continue
		}
		if _, exists := seen[gatewayID]; exists {
			continue
		}
		seen[gatewayID] = struct{}{}
		items, err := s.repository.ListAssignments(ctx, gatewayID, "")
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, items...)
	}
	return s.reconcileAssignmentSet(ctx, assignments, offlineAfter)
}

func (s *Scheduler) reconcileAssignmentSet(ctx context.Context, assignments []domain.Assignment, offlineAfter time.Duration) (result []domain.Assignment, returnErr error) {
	now := time.Now().UTC()
	result = make([]domain.Assignment, 0)
	for _, assignment := range assignments {
		if assignment.State == domain.AssignmentDraining {
			continue
		}
		gateway, gatewayErr := s.repository.GetNode(ctx, assignment.GatewayID)
		if gatewayErr != nil {
			if errors.Is(gatewayErr, sql.ErrNoRows) {
				continue
			}
			return nil, gatewayErr
		}
		observed, observedErr := s.repository.GetObserved(ctx, assignment.GatewayID)
		if errors.Is(observedErr, sql.ErrNoRows) {
			// A disabled or revoked Gateway is unavailable even when its last
			// heartbeat still looks healthy. The node transaction has already
			// quarantined its assignment; let this pass select a replacement
			// immediately instead of waiting for the old heartbeat to expire.
			if gateway.Enabled && gateway.CertificateState != domain.CertificateRevoked && gateway.CertificateState != domain.CertificateExpired {
				continue
			}
			observed = domain.ObservedState{Degraded: true}
		}
		if observedErr != nil {
			if !errors.Is(observedErr, sql.ErrNoRows) {
				return nil, observedErr
			}
		}
		nodeUnavailable := !gateway.Enabled || gateway.CertificateState == domain.CertificateRevoked || gateway.CertificateState == domain.CertificateExpired
		stale := nodeUnavailable || observed.Degraded || !observed.Healthy || observed.ObservedAt.IsZero() || now.Sub(observed.ObservedAt) >= offlineAfter
		if !stale {
			continue
		}
		if assignment.State != domain.AssignmentDegraded {
			updated, stateErr := s.repository.UpdateAssignmentState(ctx, assignment.ID, domain.AssignmentDegraded, WriteOptions{IfMatch: assignment.Revision, Actor: "system"})
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
			if _, snapshotErr := s.repository.EnsureDesiredSnapshot(ctx, assignment.GatewayID); snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
				return nil, snapshotErr
			}
			if _, snapshotErr := s.repository.EnsureDesiredSnapshot(ctx, assignment.AgentID); snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
				return nil, snapshotErr
			}
		}
		failedOver, scheduleErr := s.scheduleExistingAssignment(ctx, assignment, WriteOptions{Actor: "system"})
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
