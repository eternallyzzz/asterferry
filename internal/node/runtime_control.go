package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	v1 "asterferry/internal/controlwire/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func runtimeActionAllows(action *v1.Action, capability string) bool {
	if action == nil || len(action.GetPayloadJson()) == 0 {
		return false
	}
	var payload struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(action.GetPayloadJson(), &payload) != nil {
		return false
	}
	for _, value := range payload.Capabilities {
		if value == capability {
			return true
		}
	}
	return false
}

func (r *Runtime) runtimeTelemetryLoop(ctx context.Context, send func(*v1.NodeMessage) error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Send the first point immediately, then use a slower snapshot cadence than
	// the event cadence.  Events make lifecycle changes fast; snapshots repair
	// a dropped event and carry current byte counters.
	if err := r.sendRuntimeTelemetryBatch(ctx, send, true); err != nil && ctx.Err() == nil {
		r.logger.Warn("runtime telemetry send failed", "error", err)
		return
	}
	eventTicker := time.NewTicker(time.Second)
	defer eventTicker.Stop()
	snapshotTicker := time.NewTicker(5 * time.Second)
	defer snapshotTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-eventTicker.C:
			if err := r.sendRuntimeTelemetryBatch(ctx, send, false); err != nil {
				if ctx.Err() == nil {
					r.logger.Warn("runtime telemetry event send failed", "error", err)
				}
				return
			}
		case <-snapshotTicker.C:
			if err := r.sendRuntimeTelemetryBatch(ctx, send, true); err != nil {
				if ctx.Err() == nil {
					r.logger.Warn("runtime telemetry snapshot send failed", "error", err)
				}
				return
			}
		}
	}
}

func (r *Runtime) sendRuntimeTelemetryBatch(ctx context.Context, send func(*v1.NodeMessage) error, includeSnapshot bool) error {
	if send == nil {
		return errors.New("runtime telemetry sender is required")
	}
	dataPlane := r.DataPlane()
	if dataPlane == nil {
		return nil
	}
	events := dataPlane.PeekRuntimeEvents(128)
	protoEvents := make([]*v1.Event, 0, len(events)+1)
	for _, event := range events {
		attributes, err := json.Marshal(event)
		if err != nil {
			return err
		}
		protoEvents = append(protoEvents, &v1.Event{Id: event.ID, Type: "runtime_connection", Message: event.Message, AttributesJson: attributes, CreatedAt: timestamppb.New(event.CreatedAt)})
	}
	if includeSnapshot {
		snapshot, ok := dataPlane.RuntimeSnapshot()
		if ok {
			if err := snapshot.Validate(); err != nil {
				return fmt.Errorf("validate runtime snapshot: %w", err)
			}
			attributes, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			protoEvents = append(protoEvents, &v1.Event{Id: runtimeConnectionID(), Type: "runtime_snapshot", AttributesJson: attributes, CreatedAt: timestamppb.New(snapshot.ObservedAt)})
		}
	}
	if len(protoEvents) == 0 {
		return nil
	}
	message := &v1.NodeMessage{Body: &v1.NodeMessage_EventBatch{EventBatch: &v1.EventBatch{Events: protoEvents}}}
	if err := send(message); err != nil {
		return err
	}
	dataPlane.AckRuntimeEvents(events)
	return nil
}

func (r *Runtime) sendRuntimeActionResult(send func(*v1.NodeMessage) error, actionID, action string, affected int, actionErr error) error {
	result := struct {
		ActionID string `json:"action_id,omitempty"`
		Action   string `json:"action"`
		Affected int    `json:"affected"`
		Error    string `json:"error,omitempty"`
	}{ActionID: actionID, Action: action, Affected: affected}
	if actionErr != nil {
		result.Error = actionErr.Error()
	}
	attributes, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return send(&v1.NodeMessage{Body: &v1.NodeMessage_EventBatch{EventBatch: &v1.EventBatch{Events: []*v1.Event{{
		Id: runtimeConnectionID(), Type: "runtime_action_result", Message: action, AttributesJson: attributes, CreatedAt: timestamppb.New(time.Now().UTC()),
	}}}}})
}
