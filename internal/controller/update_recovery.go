package controller

import (
	"context"
	"fmt"
	"strings"

	"asterferry/internal/buildinfo"
	"asterferry/internal/update"
)

// recoverPendingReplacement reconciles the journal with the executable files
// before using the running version or readiness endpoint as recovery evidence.
// This closes the crash window in which the executable rename is durable while
// the prepared journal is the last durable state.
func (m *UpdateManager) recoverPendingReplacement(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	result, err := update.ReadReplacementResult(m.controllerUpdateStatusPath())
	if err != nil {
		return
	}
	currentVersion := buildinfo.Current().Version
	if result.State == UpdateStateUpToDate && currentVersion != result.Version {
		return
	}
	if isTerminalControllerReplacementState(result.State) {
		if result.State == UpdateStateUpToDate && currentVersion == result.Version {
			update.CleanupReplacementArtifacts(result)
		}
		m.persistControllerReplacementState(result)
		return
	}
	if !update.ReplacementNeedsRecovery(result.State) {
		return
	}

	diskState, inspectErr := update.ClassifyReplacementFiles(result)
	if inspectErr != nil {
		m.finishPendingControllerReplacement(result, UpdateStateManualRequired, inspectErr.Error(), false)
		return
	}
	switch diskState {
	case update.ReplacementDiskStateOriginalIntact:
		m.finishPendingControllerReplacement(
			result,
			UpdateStateFailed,
			"replacement did not install the target executable; the previous executable remains active",
			true,
		)
		return
	case update.ReplacementDiskStatePartial, update.ReplacementDiskStateIndeterminate:
		m.finishPendingControllerReplacement(
			result,
			UpdateStateManualRequired,
			fmt.Sprintf("replacement files are in an indeterminate state (%s); manual recovery is required", diskState),
			false,
		)
		return
	case update.ReplacementDiskStateTargetInstalled:
		if strings.TrimSpace(result.Version) == "" {
			m.finishPendingControllerReplacement(result, UpdateStateManualRequired, "replacement journal has no target Controller version", false)
			return
		}
		if currentVersion != result.Version {
			m.finishPendingControllerReplacement(result, UpdateStateManualRequired, "replacement target does not match the running Controller version", false)
			return
		}
	default:
		m.finishPendingControllerReplacement(result, UpdateStateManualRequired, "replacement disk state is unsupported", false)
		return
	}

	result.State = update.ReplacementStateRecovering
	result.Error = ""
	if err := update.WriteReplacementResult(m.controllerUpdateStatusPath(), result); err != nil {
		return
	}
	healthURL, err := controllerHealthURL(m.config.HTTPListen)
	if err == nil {
		healthCtx, cancel := context.WithTimeout(parent, update.HealthTimeout)
		err = update.WaitForHTTPSHealth(healthCtx, healthURL)
		cancel()
	}
	if err != nil {
		m.finishPendingControllerReplacement(
			result,
			UpdateStateManualRequired,
			fmt.Sprintf("replacement recovery could not prove Controller readiness: %v", err),
			false,
		)
		return
	}
	m.finishPendingControllerReplacement(result, UpdateStateUpToDate, "", true)
}

func isTerminalControllerReplacementState(state string) bool {
	switch state {
	case UpdateStateUpToDate, UpdateStateRolledBack, UpdateStateFailed, UpdateStateManualRequired:
		return true
	default:
		return false
	}
}

func (m *UpdateManager) finishPendingControllerReplacement(result update.ReplacementResult, state, message string, cleanup bool) {
	result.SchemaVersion = update.ReplacementResultSchemaVersion
	result.State = state
	result.Error = message
	if err := update.WriteReplacementResult(m.controllerUpdateStatusPath(), result); err != nil {
		return
	}
	if cleanup {
		update.CleanupReplacementArtifacts(result)
	}
	m.persistControllerReplacementState(result)
}

func (m *UpdateManager) persistControllerReplacementState(result update.ReplacementResult) {
	state, err := m.resources.GetControllerUpdateState(context.Background(), m.config)
	if err != nil {
		return
	}
	state.State = result.State
	state.TargetVersion = result.Version
	state.LastError = result.Error
	if result.State == UpdateStateUpToDate {
		state.CurrentVersion = buildinfo.Current().Version
	}
	_ = m.resources.SaveControllerUpdateState(context.Background(), state)
}
