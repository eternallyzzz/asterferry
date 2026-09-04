package controller

import (
	"context"

	"asterferry/internal/domain"
)

var _ SchedulingRepository = (*ResourceRepository)(nil)

// SchedulingRepository is the persistence port used by Scheduler. Keeping
// this port at the scheduling boundary prevents placement decisions from
// depending on SQL details or the complete resource repository surface.
type SchedulingRepository interface {
	GetNode(context.Context, string) (domain.Node, error)
	GetNodeSpec(context.Context, string) (domain.NodeSpec, error)
	ListNodes(context.Context, string) ([]domain.Node, error)
	ListServices(context.Context, string) ([]domain.Service, error)
	ListAssignments(context.Context, string, string) ([]domain.Assignment, error)
	GetAssignment(context.Context, string) (domain.Assignment, error)
	GetObserved(context.Context, string) (domain.ObservedState, error)
	LoadSnapshot(context.Context, string) (SnapshotRecord, error)
	EnsureDesiredSnapshot(context.Context, string) (SnapshotRecord, error)
	UpdateAssignmentState(context.Context, string, string, WriteOptions) (domain.Assignment, error)
	PutAssignment(context.Context, domain.Assignment, WriteOptions) error
	LoadGatewayCandidates(context.Context) ([]GatewayCandidate, error)
	AllocateAssignmentID(context.Context, domain.Assignment) (string, error)
}

// Scheduler owns scheduling orchestration and metrics. It never reaches into
// a database; all persistence is performed through SchedulingRepository.
type Scheduler struct {
	repository SchedulingRepository
	metrics    *ControllerMetrics
}

func NewScheduler(repository SchedulingRepository, metrics *ControllerMetrics) (*Scheduler, error) {
	if repository == nil {
		return nil, ErrRepositoryRequired
	}
	return &Scheduler{repository: repository, metrics: metrics}, nil
}

func (s *Scheduler) gatewayCandidates(ctx context.Context) ([]GatewayCandidate, error) {
	return s.repository.LoadGatewayCandidates(ctx)
}

func (s *Scheduler) startMetrics() func(error) {
	if s == nil || s.metrics == nil {
		return func(error) {}
	}
	return s.metrics.startSchedule()
}
