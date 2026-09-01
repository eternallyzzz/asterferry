package controller

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db           *sql.DB
	path         string
	masterKey    [masterKeyBytes]byte
	metrics      *ControllerMetrics
	close        sync.Once
	err          error
	actionMu     sync.Mutex
	actionSubs   map[string]map[uint64]*actionSubscription
	snapshotSubs map[string]map[uint64]*snapshotSubscription
	changeMu     sync.Mutex
	changeSubs   map[uint64]*resourceChangeSubscription
	// Snapshot materialization reads a previous generation and then writes a
	// replacement. Serialize that check-and-write pair so concurrent control
	// streams cannot publish the same generation with different content.
	snapshotMu sync.Mutex
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
