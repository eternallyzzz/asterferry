package observability

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"asterferry/internal/configstore"
)

type configRequest struct {
	BaseRevision string          `json:"base_revision"`
	YAML         string          `json:"yaml"`
	Config       json.RawMessage `json:"config"`
}

func serveConfigSnapshot(w http.ResponseWriter, r *http.Request, manager *configstore.Manager) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := manager.Snapshot()
	if err != nil {
		writeActionError(w, http.StatusInternalServerError, "config_unavailable", "configuration is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func serveConfigValidate(w http.ResponseWriter, r *http.Request, manager *configstore.Manager, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	request, err := decodeConfigRequest(w, r, true)
	if err != nil {
		writeActionError(w, http.StatusBadRequest, "invalid_config_request", err.Error())
		return
	}
	candidate, err := request.candidate()
	if err != nil {
		writeActionError(w, http.StatusBadRequest, "invalid_config_request", err.Error())
		return
	}
	result, err := manager.Validate(request.BaseRevision, candidate)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	auditManagement(logger, "management configuration validated", "management.config.validated", "changed", result.Changed)
	writeJSON(w, http.StatusOK, result)
}

func serveConfigApply(w http.ResponseWriter, r *http.Request, manager *configstore.Manager, restart func() bool, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if restart == nil {
		writeActionError(w, http.StatusNotImplemented, "restart_unavailable", "configuration restart is unavailable")
		return
	}
	request, err := decodeConfigRequest(w, r, true)
	if err != nil {
		writeActionError(w, http.StatusBadRequest, "invalid_config_request", err.Error())
		return
	}
	candidate, err := request.candidate()
	if err != nil {
		writeActionError(w, http.StatusBadRequest, "invalid_config_request", err.Error())
		return
	}
	result, err := manager.Apply(request.BaseRevision, candidate)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"schema_version": result.SchemaVersion,
		"role":           result.Role,
		"revision":       result.Revision,
		"backup":         result.Backup,
		"state":          "restart_requested",
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	auditManagement(logger, "management configuration applied", "management.config.applied", "role", result.Role)
	go restart()
}

func serveConfigRollback(w http.ResponseWriter, r *http.Request, manager *configstore.Manager, restart func() bool, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if restart == nil {
		writeActionError(w, http.StatusNotImplemented, "restart_unavailable", "configuration restart is unavailable")
		return
	}
	request, err := decodeConfigRequest(w, r, false)
	if err != nil {
		writeActionError(w, http.StatusBadRequest, "invalid_config_request", err.Error())
		return
	}
	result, err := manager.Rollback(request.BaseRevision)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"schema_version": result.SchemaVersion,
		"role":           result.Role,
		"revision":       result.Revision,
		"backup":         result.Backup,
		"state":          "restart_requested",
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	auditManagement(logger, "management configuration rolled back", "management.config.rolled_back", "role", result.Role)
	go restart()
}

func decodeConfigRequest(w http.ResponseWriter, r *http.Request, requireYAML bool) (configRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, configstore.MaxDocumentBytes+64<<10)
	defer r.Body.Close()
	var request configRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return configRequest{}, errors.New("request body is required")
		}
		return configRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return configRequest{}, errors.New("multiple JSON values are not allowed")
		}
		return configRequest{}, err
	}
	if requireYAML && request.YAML == "" {
		if len(request.Config) == 0 {
			return configRequest{}, errors.New("yaml or config is required")
		}
	}
	if strings.TrimSpace(request.BaseRevision) == "" {
		return configRequest{}, errors.New("base_revision is required")
	}
	return request, nil
}

func (r configRequest) candidate() ([]byte, error) {
	if strings.TrimSpace(r.YAML) != "" {
		return []byte(r.YAML), nil
	}
	if len(r.Config) == 0 || string(r.Config) == "null" {
		return nil, errors.New("yaml or config is required")
	}
	return configstore.JSONToYAML(r.Config)
}

func writeConfigError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	code := "invalid_config"
	message := err.Error()
	switch {
	case errors.Is(err, configstore.ErrRevisionConflict):
		status, code, message = http.StatusConflict, "config_revision_conflict", "configuration changed since it was loaded"
	case errors.Is(err, configstore.ErrReadOnly):
		status, code, message = http.StatusConflict, "config_read_only", "configuration file is read-only"
	case errors.Is(err, configstore.ErrNoBackup):
		status, code, message = http.StatusNotFound, "config_backup_missing", "no configuration backup is available"
	}
	writeActionError(w, status, code, message)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
