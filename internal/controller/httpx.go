package controller

import (
	"asterferry/internal/domain"
	"asterferry/internal/jsonutil"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

func decodeJSON(r *http.Request, value any, max int64) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	defer r.Body.Close()
	if max <= 0 {
		max = 1 << 20
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > max {
		return errors.New("request body is too large")
	}
	if err := jsonutil.DecodeStrict(data, value); err != nil {
		if errors.Is(err, jsonutil.ErrTrailingJSON) {
			return errors.New("request contains trailing JSON")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func setETag(w http.ResponseWriter, revision int64) {
	if revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(revision, 10)))
	}
}

func setETagUint64(w http.ResponseWriter, revision uint64) {
	if revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(revision, 10)))
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeAlreadyCreatedSecret(w http.ResponseWriter, metadataField string, metadata any) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":             map[string]string{"code": "already_created", "message": "token was already created; its plaintext cannot be recovered"},
		"token_recoverable": false,
		metadataField:       metadata,
	})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}

func parseIfMatch(value string) (int64, error) {
	value = strings.Trim(strings.TrimSpace(value), "\"")
	if value == "" {
		return 0, errors.New("If-Match revision is required")
	}
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil || result <= 0 {
		return 0, errors.New("If-Match must be a positive revision")
	}
	return result, nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	// modernc.org/sqlite exposes extended result codes through Code(). Classify
	// duplicate resources from the code rather than matching driver prose, which
	// may contain SQL fragments, paths, or change between driver versions.
	if isSQLiteUniqueConstraint(err) {
		writeError(w, http.StatusConflict, "already_exists", "resource already exists")
		return
	}
	var conflict *RevisionConflictError
	if errors.As(err, &conflict) {
		writeError(w, http.StatusConflict, "revision_conflict", conflict.Error())
		return
	}
	var portConflict *PortConflictError
	if errors.As(err, &portConflict) {
		writeError(w, http.StatusConflict, "port_conflict", portConflict.Error())
		return
	}
	var applyErr *domain.ApplyError
	if errors.As(err, &applyErr) {
		// Domain conflicts are safe to retry only after the caller resolves the
		// conflicting resource; expose them as HTTP 409 instead of collapsing
		// them into a generic malformed-request response.
		if applyErr.Code == "resource_conflict" || applyErr.Code == "port_conflict" || applyErr.Code == "bind_mismatch" || applyErr.Code == "port_mismatch" || applyErr.Code == "already_exists" || applyErr.Code == "bootstrap_pending" {
			writeApplyError(w, http.StatusConflict, applyErr)
			return
		}
		writeApplyError(w, http.StatusBadRequest, applyErr)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "resource was not found")
		return
	}
	if errors.Is(err, ErrStorageFailure) || isSQLiteError(err) || errors.Is(err, sql.ErrConnDone) {
		slog.Default().Error("controller store operation failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "controller storage is temporarily unavailable")
		return
	}
	// Do not reflect unclassified repository/driver errors. They can contain
	// SQL statements, filesystem paths, or other implementation details.
	writeError(w, http.StatusBadRequest, "request_rejected", "request was rejected")
}

type sqliteError interface{ Code() int }

func isSQLiteError(err error) bool {
	var coded sqliteError
	return errors.As(err, &coded)
}

func isSQLiteUniqueConstraint(err error) bool {
	var coded sqliteError
	if !errors.As(err, &coded) {
		return false
	}
	// SQLITE_CONSTRAINT_PRIMARYKEY and SQLITE_CONSTRAINT_UNIQUE are the
	// extended result codes used by SQLite for duplicate resource identities.
	return coded.Code() == 1555 || coded.Code() == 2067
}

func writeApplyError(w http.ResponseWriter, status int, applyErr *domain.ApplyError) {
	if applyErr == nil {
		writeError(w, status, "request_rejected", "request was rejected")
		return
	}
	fields := map[string]any{
		"code":      applyErr.Code,
		"message":   applyErr.Message,
		"retryable": applyErr.Retryable,
	}
	if applyErr.Path != "" {
		fields["path"] = applyErr.Path
	}
	writeJSON(w, status, map[string]any{"error": fields})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
