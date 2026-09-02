package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeSQLiteError struct {
	message string
}

func (e fakeSQLiteError) Error() string { return e.message }
func (fakeSQLiteError) Code() int       { return 2067 }

func TestWriteStoreErrorMapsSQLiteUniqueConstraintToConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeStoreError(recorder, fakeSQLiteError{message: "create node: constraint failed: UNIQUE constraint failed: nodes.id (2067)"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if got := recorder.Body.String(); !containsAll(got, `"code":"already_exists"`, "resource already exists") {
		t.Fatalf("body = %s, missing duplicate-resource error", got)
	}
}

func TestWriteStoreErrorMapsPostgresUniqueConstraintToConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeStoreError(recorder, &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
