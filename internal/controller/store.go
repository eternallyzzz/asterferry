package controller

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// databaseHandle owns only the physical database lifecycle. Resource and
// runtime repositories embed the same handle so they share one connection
// pool without sharing business methods.
type databaseHandle struct {
	db      *sql.DB
	path    string
	dialect databaseDialect
	close   sync.Once
	err     error
}

// ResourceRepository owns low-frequency control-plane state: users,
// enrollment, normalized resources, snapshots, observed state, and audit
// records. Runtime telemetry deliberately lives in RuntimeRepository.
type ResourceRepository struct {
	*databaseHandle
	masterKey  [masterKeyBytes]byte
	changes    *ChangeBus
	leadership *leadership
	// Snapshot materialization reads a previous generation and then writes a
	// replacement. Serialize that check-and-write pair so concurrent control
	// streams cannot publish the same generation with different content.
	snapshotMu sync.Mutex
}

// RuntimeRepository owns high-frequency runtime connections, events, and
// traffic rollups. It shares the database handle and notification bus with
// ResourceRepository, but has no resource CRUD surface.
type RuntimeRepository struct {
	*databaseHandle
	changes    *ChangeBus
	leadership *leadership
}

// ControllerRepositories is the composition root for the two repositories
// and their process-local change bus. It contains lifecycle wiring only; it
// intentionally does not proxy repository operations.
type ControllerRepositories struct {
	Resources *ResourceRepository
	Runtime   *RuntimeRepository
	Changes   *ChangeBus
	*databaseHandle
}

// SetLeadership attaches the process-local fencing guard after the database
// and schema have opened. Initialization and offline administration can keep
// using repositories before a running Controller owns a lease.
func (s *ControllerRepositories) SetLeadership(value *leadership) {
	if s == nil {
		return
	}
	if s.Resources != nil {
		s.Resources.leadership = value
	}
	if s.Runtime != nil {
		s.Runtime.leadership = value
	}
}

func (s *ResourceRepository) ChangeBus() *ChangeBus {
	if s == nil {
		return nil
	}
	return s.changes
}

func (s *RuntimeRepository) ChangeBus() *ChangeBus {
	if s == nil {
		return nil
	}
	return s.changes
}

func (s *ControllerRepositories) ChangeBus() *ChangeBus {
	if s == nil {
		return nil
	}
	return s.Changes
}

// Close wakes subscribers before releasing the shared connection pool.
func (s *ControllerRepositories) Close() error {
	if s == nil {
		return nil
	}
	if s.Changes != nil {
		s.Changes.Close()
	}
	if s.databaseHandle == nil {
		return nil
	}
	return s.databaseHandle.closeDatabase()
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
