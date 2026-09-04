package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		kind := r.URL.Query().Get("kind")
		nodes, err := s.store.ListNodes(r.Context(), kind)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": nodes})
		return
	}
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	var input struct {
		ID      string            `json:"id"`
		Name    string            `json:"name"`
		Labels  map[string]string `json:"labels"`
		Enabled *bool             `json:"enabled"`
	}
	if err := decodeJSON(r, &input, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	node := domain.Node{ID: input.ID, Name: input.Name, Labels: input.Labels, Enabled: enabled}
	if err := s.store.CreateNode(r.Context(), node, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetNode(r.Context(), node.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) nodeAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "node not found")
		return
	}
	nodeID := parts[0]
	if len(parts) == 2 && parts[1] == "bootstrap" {
		s.nodeBootstrap(w, r, nodeID)
		return
	}
	if len(parts) >= 2 && parts[1] == "spec" {
		switch {
		case len(parts) == 2:
			s.nodeSpecAction(w, r, nodeID)
		case len(parts) == 3 && parts[2] == "egress":
			s.nodeEgressAction(w, r, nodeID)
		case len(parts) >= 3 && (parts[2] == "proxies" || parts[2] == "routes"):
			if len(parts) > 4 {
				writeError(w, http.StatusNotFound, "not_found", "node spec subresource was not found")
				return
			}
			s.nodeAgentSpecSubresource(w, r, nodeID, parts[2], parts[3:])
		default:
			writeError(w, http.StatusNotFound, "not_found", "node spec resource was not found")
		}
		return
	}
	if len(parts) >= 2 && parts[1] == "runtime" {
		s.nodeRuntimeAction(w, r, nodeID, parts[2:])
		return
	}
	if len(parts) == 2 && r.Method == http.MethodGet && (parts[1] == "observed" || parts[1] == "snapshot" || parts[1] == "desired") {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		if parts[1] == "observed" {
			observed, err := s.store.GetObserved(r.Context(), nodeID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			setETagUint64(w, observed.AppliedGeneration)
			writeJSON(w, http.StatusOK, observed)
			return
		}
		// Materialize the latest complete desired document on demand. The
		// control stream also refreshes it periodically, but API callers should
		// not observe a stale/absent snapshot merely because no node is online.
		if _, ensureErr := s.store.EnsureDesiredSnapshot(r.Context(), nodeID); ensureErr != nil && !errors.Is(ensureErr, sql.ErrNoRows) {
			writeStoreError(w, ensureErr)
			return
		}
		snapshot, err := s.store.GetSnapshot(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETagUint64(w, snapshot.Generation)
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		node, err := s.store.GetNode(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, node.Revision)
		writeJSON(w, http.StatusOK, node)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		var input struct {
			Name              *string            `json:"name"`
			Labels            *map[string]string `json:"labels"`
			Enabled           *bool              `json:"enabled"`
			CertificateState  *string            `json:"certificate_state"`
			CertificateSerial *string            `json:"certificate_serial"`
		}
		if err := decodeJSON(r, &input, 1<<20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		node, getErr := s.store.GetNode(r.Context(), nodeID)
		if getErr != nil {
			writeStoreError(w, getErr)
			return
		}
		if input.Name != nil {
			node.Name = *input.Name
		}
		if input.Labels != nil {
			node.Labels = *input.Labels
		}
		if input.Enabled != nil {
			node.Enabled = *input.Enabled
		}
		if input.CertificateState != nil {
			node.CertificateState = *input.CertificateState
		}
		if input.CertificateSerial != nil {
			node.CertificateSerial = *input.CertificateSerial
		}
		expected, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
			return
		}
		if err := s.store.UpdateNode(r.Context(), node, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		if !node.Enabled || defaultCertificateState(node.CertificateState) != domain.CertificateActive {
			// Revoke is enforced at the next mTLS handshake. Ask an online node
			// to disconnect immediately as well. Disabling, expiry and a pending
			// certificate follow the same fail-closed path; an offline node is
			// rejected when it reconnects.
			delivered, actionErr := s.store.PublishAction(r.Context(), nodeID, "reconnect", "")
			if actionErr != nil || !delivered {
				if actionErr != nil {
					slog.Default().Error("failed to publish node security action", "node_id", nodeID, "error", actionErr)
				} else {
					slog.Default().Warn("node security action is not currently delivered", "node_id", nodeID)
				}
				eventType := "action_not_delivered"
				if actionErr != nil {
					eventType = "action_delivery_failed"
				}
				if eventErr := s.store.RecordEvent(context.Background(), user.Username, "", eventType, "reconnect action was not delivered immediately", nodeID, map[string]string{"action": "reconnect"}); eventErr != nil {
					slog.Default().Error("failed to record security action delivery event", "node_id", nodeID, "error", eventErr)
				}
			}
		}
		updated, err := s.store.GetNode(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, updated.Revision)
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		expected, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
			return
		}
		if err := s.store.DeleteNode(r.Context(), nodeID, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 3 && parts[1] == "actions" && r.Method == http.MethodPost {
		user, ok := s.authorize(w, r, RoleOperator)
		if !ok {
			return
		}
		action := parts[2]
		if action == "schedule" {
			assignments, scheduleErr := s.store.ScheduleAgent(r.Context(), nodeID, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
			if scheduleErr != nil {
				writeStoreError(w, scheduleErr)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"node_id": nodeID, "action": action, "assignments": assignments})
			return
		}
		if action != "drain" && action != "reconnect" && action != "resync" {
			writeError(w, http.StatusBadRequest, "unknown_action", "unsupported node action")
			return
		}
		// Persist and audit the request before publishing it to connected node
		// streams. The idempotency key covers both operations, so a retried
		// request cannot emit the same action twice.
		delivered, requestErr := s.store.RequestNodeAction(r.Context(), nodeID, action, "", WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if requestErr != nil {
			writeStoreError(w, requestErr)
			return
		}
		state := "queued"
		if delivered {
			state = "delivered"
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"node_id": nodeID, "action": action, "requested_by": user.Username, "state": state})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "node resource was not found")
}
