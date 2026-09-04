package controller

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Repository struct {
	db        *sql.DB
	path      string
	dialect   databaseDialect
	masterKey [masterKeyBytes]byte
	changes   *ChangeBus

	close     sync.Once
	err       error
	changesMu sync.Mutex
	// Snapshot materialization reads a previous generation and then writes a
	// replacement. Serialize that check-and-write pair so concurrent control
	// streams cannot publish the same generation with different content.
	snapshotMu sync.Mutex
}

// ChangeBus returns the process-local notification bus owned by the
// Controller composition root. Repository persistence and subscriptions are
// deliberately separate concerns; this accessor exists only while composing
// the two at the application boundary.
func (s *Repository) ChangeBus() *ChangeBus {
	if s == nil {
		return nil
	}
	s.changesMu.Lock()
	defer s.changesMu.Unlock()
	if s.changes == nil {
		s.changes = newChangeBus()
	}
	return s.changes
}

type RevisionConflictError struct {
	Resource string
	Expected int64
	Actual   int64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("%s revision conflict: expected %d, current %d", e.Resource, e.Expected, e.Actual)
}

func IsRevisionConflict(err error) bool {
	var conflict *RevisionConflictError
	return errors.As(err, &conflict)
}

type WriteOptions struct {
	IfMatch        int64
	IdempotencyKey string
	Actor          string
}

type AuditRecord struct {
	ID         int64             `json:"id"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	Resource   string            `json:"resource"`
	ResourceID string            `json:"resource_id"`
	Revision   int64             `json:"revision"`
	Attributes map[string]string `json:"attributes,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

type SnapshotRecord struct {
	NodeID     string
	Generation uint64
	Checksum   string
	Document   []byte
	CreatedAt  time.Time
}

type ObservedRecord struct {
	NodeID     string
	Generation uint64
	Document   []byte
	UpdatedAt  time.Time
}

// ErrSecretAlreadyCreated means an idempotent retry found a one-time secret
// that was already persisted. The metadata can be replayed safely, but the
// plaintext was deliberately never stored and cannot be recovered.
var ErrSecretAlreadyCreated = errors.New("one-time token was already created and its plaintext cannot be recovered")
