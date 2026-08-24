package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const sseWriteDeadline = 30 * time.Second

func serveDashboardSnapshot(w http.ResponseWriter, provider StatusProvider, metrics *Metrics, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	dashboard, ok := provider.(DashboardProvider)
	if !ok {
		writeActionError(w, http.StatusNotImplemented, "dashboard_unavailable", "dashboard data is unavailable")
		return
	}
	snapshot := dashboard.Dashboard()
	if snapshot.SchemaVersion == 0 {
		snapshot.SchemaVersion = DashboardSchemaVersion
	}
	if snapshot.GeneratedAt.IsZero() {
		snapshot.GeneratedAt = time.Now().UTC()
	}
	snapshot.Metrics = metrics.Snapshot()
	if err := json.NewEncoder(w).Encode(snapshot); err != nil {
		logManagementError(logger, "management dashboard serialization failed", "management.dashboard.serialize_failed")
	}
}

func serveEvents(w http.ResponseWriter, r *http.Request, hub *EventHub, metrics *Metrics, logger *slog.Logger) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	lastID, err := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if err != nil {
		writeActionError(w, http.StatusBadRequest, "invalid_last_event_id", "Last-Event-ID must be an unsigned integer")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeActionError(w, http.StatusNotImplemented, "streaming_unavailable", "streaming responses are unavailable")
		return
	}
	stream, err := hub.Open(lastID)
	if err != nil {
		if metrics != nil {
			metrics.ManagementEventStreamsRejected.Add(1)
		}
		auditManagement(logger, "management event stream rejected", "management.events.rejected", "error_kind", "subscriber_limit")
		writeActionError(w, http.StatusTooManyRequests, "event_stream_busy", "event stream subscriber limit reached")
		return
	}
	defer stream.Close()
	if metrics != nil {
		metrics.ManagementEventSubscribers.Add(1)
		defer metrics.ManagementEventSubscribers.Add(-1)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	expected := uint64(0)
	if lastID > 0 {
		expected = lastID + 1
	}
	for _, event := range stream.Replay {
		if expected > 0 && event.ID > expected {
			refreshSSEDeadline(w)
			if err := writeEventGap(w, expected, event.ID-1); err != nil {
				return
			}
		}
		refreshSSEDeadline(w)
		if err := writeSSE(w, "log", event); err != nil {
			return
		}
		expected = event.ID + 1
	}
	refreshSSEDeadline(w)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-stream.Events:
			if !ok {
				return
			}
			if expected > 0 && event.ID > expected {
				refreshSSEDeadline(w)
				if err := writeEventGap(w, expected, event.ID-1); err != nil {
					return
				}
			}
			refreshSSEDeadline(w)
			if err := writeSSE(w, "log", event); err != nil {
				return
			}
			expected = event.ID + 1
			flusher.Flush()
		case <-ticker.C:
			refreshSSEDeadline(w)
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func serveAction(w http.ResponseWriter, r *http.Request, provider ActionProvider, name string, metrics *Metrics, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		recordActionRejected(metrics, logger, name, "method_not_allowed")
		return
	}
	if provider == nil {
		recordActionRejected(metrics, logger, name, "action_unavailable")
		writeActionError(w, http.StatusNotImplemented, "action_unavailable", "action is unavailable")
		return
	}
	if name == "shutdown" {
		if deferred, ok := provider.(DeferredShutdownProvider); ok {
			if err := deferred.CanShutdown(); err != nil {
				code := writeActionFailure(w, err)
				recordActionRejected(metrics, logger, name, code)
				return
			}
			writeAcceptedAction(w, name)
			recordActionAccepted(metrics)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			go deferred.TriggerShutdown()
			return
		}
	}
	action := provider.RequestReconnect
	if name == "shutdown" {
		action = provider.RequestShutdown
	}
	if err := action(); err != nil {
		code := writeActionFailure(w, err)
		recordActionRejected(metrics, logger, name, code)
		return
	}
	recordActionAccepted(metrics)
	writeAcceptedAction(w, name)
}

func writeActionFailure(w http.ResponseWriter, err error) string {
	status := http.StatusConflict
	code := "action_unavailable"
	message := "action is unavailable"
	switch {
	case errors.Is(err, ErrActionUnsupported):
		status, code, message = http.StatusNotImplemented, "action_unsupported", "action is unsupported for this role"
	case errors.Is(err, ErrActionBusy):
		code, message = "action_busy", "action is already in progress"
	}
	writeActionError(w, status, code, message)
	return code
}

func recordActionAccepted(metrics *Metrics) {
	if metrics != nil {
		metrics.ManagementActionsAccepted.Add(1)
	}
}

func recordActionRejected(metrics *Metrics, logger *slog.Logger, name, reason string) {
	if metrics != nil {
		metrics.ManagementActionsRejected.Add(1)
	}
	auditManagement(logger, "management action rejected", "management.action.rejected", "action", name, "error_kind", reason)
}

func writeAcceptedAction(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": DashboardSchemaVersion,
		"action":         name,
		"state":          "requested",
	})
}

func parseLastEventID(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func refreshSSEDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sseWriteDeadline))
}

func writeSSE(w http.ResponseWriter, eventName string, event Event) error {
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, eventName, b); err != nil {
		return err
	}
	return nil
}

func writeEventGap(w http.ResponseWriter, from, to uint64) error {
	b, err := json.Marshal(map[string]any{"from": from, "to": to, "reason": "buffer_overflow"})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: gap\ndata: %s\n\n", b)
	return err
}

func writeActionError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeActionError(w, http.StatusTooManyRequests, "management_rate_limited", "management authentication is temporarily rate limited")
}

func auditManagement(logger *slog.Logger, message, event string, attrs ...any) {
	if logger == nil {
		return
	}
	args := make([]any, 0, len(attrs)+4)
	args = append(args, "event", event, "security_audit", true)
	args = append(args, attrs...)
	logger.Warn(message, args...)
}

func logManagementError(logger *slog.Logger, message, event string) {
	if logger == nil {
		return
	}
	logger.Error(message, "event", event, "error_kind", "serialization")
}
