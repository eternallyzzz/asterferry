package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const runtimeRetention = 30 * 24 * time.Hour

type RuntimeEventRecord struct {
	ID           int64           `json:"id"`
	EventID      string          `json:"event_id"`
	NodeID       string          `json:"node_id"`
	ConnectionID string          `json:"connection_id,omitempty"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

type RuntimeTrafficRollup struct {
	BucketStart  time.Time `json:"bucket_start"`
	NodeID       string    `json:"node_id"`
	GatewayID    string    `json:"gateway_id,omitempty"`
	AgentID      string    `json:"agent_id,omitempty"`
	AssignmentID string    `json:"assignment_id,omitempty"`
	ServiceID    string    `json:"service_id,omitempty"`
	Protocol     string    `json:"protocol"`
	BytesIn      uint64    `json:"bytes_in"`
	BytesOut     uint64    `json:"bytes_out"`
	Opened       uint64    `json:"opened"`
	Closed       uint64    `json:"closed"`
	Rejected     uint64    `json:"rejected"`
	RateLimited  uint64    `json:"rate_limited"`
	ActiveMax    uint64    `json:"active_max"`
}

type RuntimeConnectionFilter struct {
	NodeID       string
	State        string
	Type         string
	SourceIP     string
	PeerNodeID   string
	GatewayID    string
	AgentID      string
	AssignmentID string
	ServiceID    string
	Protocol     string
	Limit        int
}

type runtimeChangeSubscription struct {
	id uint64
	ch chan string
}

var nextRuntimeSubscription atomic.Uint64

func runtimeSchemaStatements(types schemaTypes) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE runtime_connections (
			node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			type TEXT NOT NULL,
			state TEXT NOT NULL,
			peer_node_id TEXT NOT NULL DEFAULT '',
			gateway_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			assignment_id TEXT NOT NULL DEFAULT '',
			service_id TEXT NOT NULL DEFAULT '',
			protocol TEXT NOT NULL DEFAULT '',
			source_ip TEXT NOT NULL DEFAULT '',
			source_port %s NOT NULL DEFAULT 0,
			target TEXT NOT NULL DEFAULT '',
			parent_session_id TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			last_activity_at TEXT NOT NULL,
			ended_at TEXT,
			close_reason TEXT NOT NULL DEFAULT '',
			bytes_in %s NOT NULL DEFAULT 0,
			bytes_out %s NOT NULL DEFAULT 0,
			rate_in %s NOT NULL DEFAULT 0,
			rate_out %s NOT NULL DEFAULT 0,
			limit_json %s,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(node_id,id)
		)`, types.bigInteger, types.bigInteger, types.bigInteger, types.real, types.real, types.blob),
		fmt.Sprintf(`CREATE TABLE runtime_events (
			id %s,
			event_id TEXT NOT NULL UNIQUE,
			node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			connection_id TEXT NOT NULL DEFAULT '',
			event_type TEXT NOT NULL,
			payload_json %s NOT NULL,
			created_at TEXT NOT NULL
		)`, types.autoID, types.blob),
		fmt.Sprintf(`CREATE TABLE runtime_traffic_rollups (
			bucket_start TEXT NOT NULL,
			node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			gateway_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			assignment_id TEXT NOT NULL DEFAULT '',
			service_id TEXT NOT NULL DEFAULT '',
			protocol TEXT NOT NULL DEFAULT '',
			bytes_in %s NOT NULL DEFAULT 0,
			bytes_out %s NOT NULL DEFAULT 0,
			opened %s NOT NULL DEFAULT 0,
			closed %s NOT NULL DEFAULT 0,
			rejected %s NOT NULL DEFAULT 0,
			rate_limited %s NOT NULL DEFAULT 0,
			active_max %s NOT NULL DEFAULT 0,
			PRIMARY KEY(bucket_start,node_id,assignment_id,service_id,protocol)
		)`, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger),
		`CREATE TABLE runtime_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX idx_runtime_connections_node_state ON runtime_connections(node_id,state,last_activity_at)`,
		`CREATE INDEX idx_runtime_connections_source ON runtime_connections(source_ip,last_activity_at)`,
		`CREATE INDEX idx_runtime_connections_assignment ON runtime_connections(assignment_id,last_activity_at)`,
		`CREATE INDEX idx_runtime_events_created ON runtime_events(created_at)`,
		`CREATE INDEX idx_runtime_events_node_created ON runtime_events(node_id,created_at)`,
		`CREATE INDEX idx_runtime_rollups_bucket ON runtime_traffic_rollups(bucket_start,node_id)`,
	}
}

func runtimeSchemaIndexes() []string {
	return []string{
		"idx_runtime_connections_node_state", "idx_runtime_connections_source", "idx_runtime_connections_assignment",
		"idx_runtime_events_created", "idx_runtime_events_node_created", "idx_runtime_rollups_bucket",
	}
}

func (s *Store) SubscribeRuntimeChanges() (<-chan string, func()) {
	ch := make(chan string, 32)
	sub := &runtimeChangeSubscription{id: nextRuntimeSubscription.Add(1), ch: ch}
	s.runtimeMu.Lock()
	if s.runtimeSubs == nil {
		s.runtimeSubs = make(map[uint64]*runtimeChangeSubscription)
	}
	s.runtimeSubs[sub.id] = sub
	s.runtimeMu.Unlock()
	var once atomic.Bool
	return ch, func() {
		if once.Swap(true) {
			return
		}
		s.runtimeMu.Lock()
		if current := s.runtimeSubs[sub.id]; current == sub {
			delete(s.runtimeSubs, sub.id)
			close(current.ch)
		}
		s.runtimeMu.Unlock()
	}
}

func (s *Store) notifyRuntimeChanges(nodeID string) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	for _, sub := range s.runtimeSubs {
		select {
		case sub.ch <- nodeID:
		default:
		}
	}
}

func (s *Store) AdvancedOperationsEnabled(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key='advanced_operations_enabled'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(value), "true") || strings.TrimSpace(value) == "1", nil
}

func (s *Store) markRuntimeConnectionsUnknownOnStartup(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runtime_connections SET state='unknown' WHERE state='active'`)
	return err
}

func (s *Store) SetAdvancedOperationsEnabled(ctx context.Context, enabled bool, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	value := "false"
	if enabled {
		value = "true"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at) VALUES('advanced_operations_enabled',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, value, now); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "runtime_settings:update", "runtime_settings", "advanced_operations_enabled", 0, map[string]string{"enabled": value}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyRuntimeChanges("")
	return nil
}
