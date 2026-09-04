package node

import (
	"asterferry/internal/domain"
	"context"
	"errors"
)

func (r *Runtime) applySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
	kind, err := snapshotRuntimeKind(snapshot)
	if err != nil {
		return err
	}
	var previousKind domain.NodeSpecKind
	if previous != nil {
		previousKind, _ = snapshotRuntimeKind(*previous)
	}
	if kind == "" {
		return r.disableRuntime()
	}
	engine, dataPlane, err := r.ensureRuntime(snapshot)
	if err != nil {
		if previous != nil && previousKind != "" && previousKind != kind {
			restoreErr := r.restoreRuntime(ctx, *previous)
			return errors.Join(err, restoreErr)
		}
		return err
	}
	sameKind := previous == nil || previousKind == kind
	enginePrevious := previous
	if !sameKind {
		// An engine rejects a snapshot for the other behavior. A kind switch is
		// a complete component replacement, so the new engine starts without a
		// behavior-mismatched rollback baseline.
		enginePrevious = nil
	}
	wasDraining := engine.IsDraining()
	engine.BeginDrain()
	if err := engine.ApplySnapshot(ctx, snapshot, enginePrevious); err != nil {
		if previous != nil && !sameKind {
			restoreErr := r.restoreRuntime(ctx, *previous)
			return errors.Join(err, restoreErr)
		}
		// Engine.ApplySnapshot builds its replacement indexes before taking
		// effect, so a same-kind failure leaves the previous generation intact.
		// Restore the admission state that existed before this speculative
		// apply; otherwise a valid last-known-good generation would remain
		// permanently drained after a rejected update.
		if !wasDraining {
			engine.EndDrain()
		}
		return err
	}
	if err := dataPlane.ApplySnapshot(ctx, snapshot, enginePrevious); err != nil {
		r.logger.Warn("data-plane snapshot apply failed", "generation", snapshot.Generation, "error", err)
		rollbackCtx := context.WithoutCancel(ctx)
		if previous == nil {
			return errors.Join(err, engine.ResetSnapshot(snapshot.Generation))
		}
		if sameKind {
			var rollbackErrs []error
			if rollbackErr := engine.ApplySnapshot(rollbackCtx, *previous, &snapshot); rollbackErr != nil {
				rollbackErrs = append(rollbackErrs, rollbackErr)
			}
			if rollbackErr := dataPlane.ApplySnapshot(rollbackCtx, *previous, nil); rollbackErr != nil {
				rollbackErrs = append(rollbackErrs, rollbackErr)
			}
			if len(rollbackErrs) > 0 {
				return errors.Join(err, errors.Join(rollbackErrs...))
			}
			if !wasDraining {
				engine.EndDrain()
			}
			return err
		}
		// A failed kind switch is restored by constructing the old behavior again.
		oldEngine, oldDataPlane, restoreErr := r.ensureRuntimeKind(previousKind)
		if restoreErr == nil {
			restoreErr = oldEngine.ApplySnapshot(rollbackCtx, *previous, nil)
		}
		if restoreErr == nil {
			restoreErr = oldDataPlane.ApplySnapshot(rollbackCtx, *previous, nil)
		}
		return errors.Join(err, restoreErr)
	}
	if !wasDraining {
		engine.EndDrain()
	}
	return nil
}

func (r *Runtime) restoreRuntime(ctx context.Context, snapshot domain.DesiredSnapshot) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rollbackCtx := context.WithoutCancel(ctx)
	kind, err := snapshotRuntimeKind(snapshot)
	if err != nil {
		return err
	}
	if kind == "" {
		return r.disableRuntime()
	}
	engine, dataPlane, err := r.ensureRuntimeKind(kind)
	if err != nil {
		return err
	}
	if err := engine.ApplySnapshot(rollbackCtx, snapshot, nil); err != nil {
		return err
	}
	return dataPlane.ApplySnapshot(rollbackCtx, snapshot, nil)
}

func (r *Runtime) resetSnapshot(ctx context.Context, generation uint64) error {
	engine, dataPlane := r.Engine(), r.DataPlane()
	if engine == nil || dataPlane == nil {
		return nil
	}
	return errors.Join(dataPlane.ResetSnapshot(generation), engine.ResetSnapshot(generation))
}

func (r *Runtime) observedState() domain.ObservedState {
	observed := r.reconciler.State()
	dataPlane := r.DataPlane()
	if dataPlane == nil {
		return observed
	}
	dataObserved, ok := dataPlane.ObservedState()
	if !ok {
		return observed
	}
	observed.Sessions = dataObserved.Sessions
	observed.Listeners = dataObserved.Listeners
	observed.Metrics = dataObserved.Metrics
	if observed.AppliedGeneration == dataObserved.AppliedGeneration && !dataObserved.Healthy {
		observed.Healthy = false
		observed.Degraded = true
	}
	observed.ObservedAt = dataObserved.ObservedAt
	return observed
}
