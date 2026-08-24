package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func serveDashboardSnapshot(w http.ResponseWriter, provider StatusProvider, metrics *Metrics) {
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
		return
	}
}

func serveEvents(w http.ResponseWriter, r *http.Request, hub *EventHub) {
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
	stream, err := hub.Open(lastID)
	if err != nil {
		writeActionError(w, http.StatusTooManyRequests, "event_stream_busy", "event stream subscriber limit reached")
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeActionError(w, http.StatusNotImplemented, "streaming_unavailable", "streaming responses are unavailable")
		return
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
			if err := writeEventGap(w, expected, event.ID-1); err != nil {
				return
			}
		}
		if err := writeSSE(w, "log", event); err != nil {
			return
		}
		expected = event.ID + 1
	}
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
				if err := writeEventGap(w, expected, event.ID-1); err != nil {
					return
				}
			}
			if err := writeSSE(w, "log", event); err != nil {
				return
			}
			expected = event.ID + 1
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func serveAction(w http.ResponseWriter, r *http.Request, provider ActionProvider, name string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if provider == nil {
		writeActionError(w, http.StatusNotImplemented, "action_unavailable", "action is unavailable")
		return
	}
	if name == "shutdown" {
		if deferred, ok := provider.(DeferredShutdownProvider); ok {
			if err := deferred.CanShutdown(); err != nil {
				writeActionFailure(w, err)
				return
			}
			writeAcceptedAction(w, name)
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
		writeActionFailure(w, err)
		return
	}
	writeAcceptedAction(w, name)
}

func writeActionFailure(w http.ResponseWriter, err error) {
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
