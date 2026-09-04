package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"asterferry/internal/domain"
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

func (s *Store) RecordRuntimeEvent(ctx context.Context, nodeID, eventID, eventType string, payload []byte, createdAt time.Time) error {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return err
	}
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		eventID, _ = randomID()
	}
	if err := domain.ValidateID(eventID, "runtime_event.id"); err != nil {
		return err
	}
	eventType = strings.TrimSpace(eventType)
	if eventType != "runtime_connection" && eventType != "runtime_snapshot" && eventType != "runtime_action_result" {
		return errors.New("runtime event type is unsupported")
	}
	if len(payload) == 0 || len(payload) > 4<<20 {
		return errors.New("runtime event payload is empty or too large")
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	createdAt = createdAt.UTC()
	var connectionID string
	var connection *domain.RuntimeConnection
	var snapshot *domain.RuntimeSnapshot
	var runtimeEvent domain.RuntimeEvent
	switch eventType {
	case "runtime_connection":
		if err := json.Unmarshal(payload, &runtimeEvent); err != nil {
			return errors.New("runtime connection event payload is invalid")
		}
		if err := runtimeEvent.Validate(); err != nil {
			return fmt.Errorf("validate runtime connection event: %w", err)
		}
		if runtimeEvent.NodeID != nodeID {
			return errors.New("runtime event node identity does not match stream")
		}
		connectionID = runtimeEvent.ConnectionID
		if runtimeEvent.Connection != nil {
			connection = runtimeEvent.Connection
		}
	case "runtime_snapshot":
		if err := json.Unmarshal(payload, &snapshot); err != nil || snapshot == nil {
			return errors.New("runtime snapshot payload is invalid")
		}
		if err := snapshot.Validate(); err != nil {
			return fmt.Errorf("validate runtime snapshot: %w", err)
		}
		if snapshot.NodeID != nodeID {
			return errors.New("runtime snapshot node identity does not match stream")
		}
	case "runtime_action_result":
		var result struct {
			ActionID string `json:"action_id"`
			Action   string `json:"action"`
			Affected int    `json:"affected"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(payload, &result); err != nil || strings.TrimSpace(result.Action) == "" || len(result.Error) > 2048 {
			return errors.New("runtime action result payload is invalid")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	persistEvent := true
	if eventType == "runtime_snapshot" {
		// Snapshots are repair frames, not lifecycle history.  Keep at most one
		// durable repair point per node/minute; otherwise a 100-node Controller
		// would write millions of identical state frames per month.
		var last string
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM runtime_events WHERE node_id=? AND event_type='runtime_snapshot' ORDER BY id DESC LIMIT 1`, nodeID).Scan(&last)
		if err == nil {
			if previous, parseErr := parseStoredTime("runtime_snapshot.created_at", last); parseErr == nil && createdAt.Sub(previous) < time.Minute {
				persistEvent = false
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	inserted := int64(1)
	if persistEvent {
		insertSQL := `INSERT OR IGNORE INTO runtime_events(event_id,node_id,connection_id,event_type,payload_json,created_at) VALUES(?,?,?,?,?,?)`
		if s.dialect != nil && s.dialect.backend() == databaseBackendPostgres {
			insertSQL = `INSERT INTO runtime_events(event_id,node_id,connection_id,event_type,payload_json,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`
		}
		result, err := tx.ExecContext(ctx, insertSQL, eventID, nodeID, connectionID, eventType, payload, createdAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		inserted, err = result.RowsAffected()
		if err != nil {
			return err
		}
	}
	if inserted == 0 {
		return tx.Commit()
	}
	if connection != nil {
		if err := upsertRuntimeConnectionTx(ctx, tx, *connection, createdAt); err != nil {
			return err
		}
		opened, closed, rejected, limited := uint64(0), uint64(0), uint64(0), uint64(0)
		switch runtimeEvent.Type {
		case domain.RuntimeEventOpened:
			opened = 1
		case domain.RuntimeEventClosed:
			closed = 1
		case domain.RuntimeEventRejected:
			rejected = 1
		case domain.RuntimeEventRateLimited:
			limited = 1
		}
		if err := upsertRuntimeRollupTx(ctx, tx, *connection, createdAt, opened, closed, rejected, limited, 0); err != nil {
			return err
		}
	} else if eventType == "runtime_connection" && runtimeEvent.Type == domain.RuntimeEventRejected {
		// A rejection can happen before a connection ID or service is known
		// (for example, a public listener with no authenticated Agent session).
		// Keep that signal in a node-level rollup without inventing a current
		// connection row.
		ended := createdAt
		rejected := domain.RuntimeConnection{ID: eventID, Type: domain.RuntimeConnectionSession, NodeID: nodeID, State: domain.RuntimeStateClosed, StartedAt: createdAt, LastActivityAt: createdAt, EndedAt: &ended}
		if err := upsertRuntimeRollupTx(ctx, tx, rejected, createdAt, 0, 0, 1, 0, 0); err != nil {
			return err
		}
	}
	if snapshot != nil {
		if err := upsertRuntimeSnapshotTx(ctx, tx, *snapshot); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyRuntimeChanges(nodeID)
	return nil
}

func (s *Store) ListRuntimeConnections(ctx context.Context, filter RuntimeConnectionFilter) ([]domain.RuntimeConnection, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT node_id,id,type,state,peer_node_id,gateway_id,agent_id,assignment_id,service_id,protocol,source_ip,source_port,target,parent_session_id,started_at,last_activity_at,ended_at,close_reason,bytes_in,bytes_out,rate_in,rate_out,limit_json FROM runtime_connections WHERE 1=1`
	args := make([]any, 0, 10)
	for _, field := range []struct{ column, value string }{
		{"node_id", filter.NodeID}, {"state", filter.State}, {"type", filter.Type}, {"source_ip", filter.SourceIP},
		{"peer_node_id", filter.PeerNodeID}, {"gateway_id", filter.GatewayID}, {"agent_id", filter.AgentID},
		{"assignment_id", filter.AssignmentID}, {"service_id", filter.ServiceID}, {"protocol", filter.Protocol},
	} {
		if strings.TrimSpace(field.value) != "" {
			query += ` AND ` + field.column + `=?`
			args = append(args, strings.TrimSpace(field.value))
		}
	}
	query += ` ORDER BY CASE state WHEN 'active' THEN 0 WHEN 'unknown' THEN 1 ELSE 2 END,last_activity_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.RuntimeConnection, 0)
	for rows.Next() {
		connection, err := scanRuntimeConnection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, connection)
	}
	return result, rows.Err()
}

// MarkRuntimeConnectionsUnknown prevents an abandoned control stream from
// leaving rows that look active forever. A reconnect immediately repairs the
// rows with its next runtime snapshot; closed rows are preserved unchanged.
func (s *Store) MarkRuntimeConnectionsUnknown(ctx context.Context, nodeID string, at time.Time) error {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runtime_connections SET state='unknown',updated_at=? WHERE node_id=? AND state='active'`, at.UTC().Format(time.RFC3339Nano), nodeID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		s.notifyRuntimeChanges(nodeID)
	}
	return nil
}

func (s *Store) GetRuntimeConnection(ctx context.Context, nodeID, connectionID string) (domain.RuntimeConnection, error) {
	return scanRuntimeConnection(s.db.QueryRowContext(ctx, `SELECT node_id,id,type,state,peer_node_id,gateway_id,agent_id,assignment_id,service_id,protocol,source_ip,source_port,target,parent_session_id,started_at,last_activity_at,ended_at,close_reason,bytes_in,bytes_out,rate_in,rate_out,limit_json FROM runtime_connections WHERE node_id=? AND id=?`, nodeID, connectionID))
}

func (s *Store) ListRuntimeEvents(ctx context.Context, nodeID string, limit int) ([]RuntimeEventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id,event_id,node_id,connection_id,event_type,payload_json,created_at FROM runtime_events`
	args := []any{}
	if strings.TrimSpace(nodeID) != "" {
		query += ` WHERE node_id=?`
		args = append(args, strings.TrimSpace(nodeID))
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RuntimeEventRecord, 0)
	for rows.Next() {
		var value RuntimeEventRecord
		var payload []byte
		var created string
		if err := rows.Scan(&value.ID, &value.EventID, &value.NodeID, &value.ConnectionID, &value.Type, &payload, &created); err != nil {
			return nil, err
		}
		value.Payload = append(json.RawMessage(nil), payload...)
		var err error
		value.CreatedAt, err = parseStoredTime("runtime_event.created_at", created)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) ListRuntimeTraffic(ctx context.Context, nodeID string, limit int) ([]RuntimeTrafficRollup, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	query := `SELECT bucket_start,node_id,gateway_id,agent_id,assignment_id,service_id,protocol,bytes_in,bytes_out,opened,closed,rejected,rate_limited,active_max FROM runtime_traffic_rollups`
	args := []any{}
	if strings.TrimSpace(nodeID) != "" {
		query += ` WHERE node_id=?`
		args = append(args, strings.TrimSpace(nodeID))
	}
	query += ` ORDER BY bucket_start DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RuntimeTrafficRollup, 0)
	for rows.Next() {
		var value RuntimeTrafficRollup
		var bucket string
		var bytesIn, bytesOut, opened, closed, rejected, limited, active int64
		if err := rows.Scan(&bucket, &value.NodeID, &value.GatewayID, &value.AgentID, &value.AssignmentID, &value.ServiceID, &value.Protocol, &bytesIn, &bytesOut, &opened, &closed, &rejected, &limited, &active); err != nil {
			return nil, err
		}
		var err error
		value.BucketStart, err = parseStoredTime("runtime_rollup.bucket_start", bucket)
		if err != nil {
			return nil, err
		}
		value.BytesIn, err = nonNegativeUint64(bytesIn)
		if err != nil {
			return nil, err
		}
		value.BytesOut, err = nonNegativeUint64(bytesOut)
		if err != nil {
			return nil, err
		}
		value.Opened, err = nonNegativeUint64(opened)
		if err != nil {
			return nil, err
		}
		value.Closed, err = nonNegativeUint64(closed)
		if err != nil {
			return nil, err
		}
		value.Rejected, err = nonNegativeUint64(rejected)
		if err != nil {
			return nil, err
		}
		value.RateLimited, err = nonNegativeUint64(limited)
		if err != nil {
			return nil, err
		}
		value.ActiveMax, err = nonNegativeUint64(active)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func upsertRuntimeConnectionTx(ctx context.Context, tx *sql.Tx, connection domain.RuntimeConnection, updatedAt time.Time) error {
	if err := connection.Validate(); err != nil {
		return err
	}
	var limitJSON []byte
	var err error
	if connection.Limit != nil {
		limitJSON, err = json.Marshal(connection.Limit)
		if err != nil {
			return err
		}
	}
	var ended any
	if connection.EndedAt != nil {
		ended = connection.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_connections(node_id,id,type,state,peer_node_id,gateway_id,agent_id,assignment_id,service_id,protocol,source_ip,source_port,target,parent_session_id,started_at,last_activity_at,ended_at,close_reason,bytes_in,bytes_out,rate_in,rate_out,limit_json,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(node_id,id) DO UPDATE SET type=excluded.type,state=excluded.state,peer_node_id=excluded.peer_node_id,gateway_id=excluded.gateway_id,agent_id=excluded.agent_id,assignment_id=excluded.assignment_id,service_id=excluded.service_id,protocol=excluded.protocol,source_ip=excluded.source_ip,source_port=excluded.source_port,target=excluded.target,parent_session_id=excluded.parent_session_id,started_at=excluded.started_at,last_activity_at=excluded.last_activity_at,ended_at=excluded.ended_at,close_reason=excluded.close_reason,bytes_in=excluded.bytes_in,bytes_out=excluded.bytes_out,rate_in=excluded.rate_in,rate_out=excluded.rate_out,limit_json=excluded.limit_json,updated_at=excluded.updated_at`,
		connection.NodeID, connection.ID, connection.Type, connection.State, connection.PeerNodeID, connection.GatewayID, connection.AgentID, connection.AssignmentID, connection.ServiceID, connection.Protocol, connection.SourceIP, connection.SourcePort, connection.Target, connection.ParentSessionID, connection.StartedAt.UTC().Format(time.RFC3339Nano), connection.LastActivityAt.UTC().Format(time.RFC3339Nano), ended, connection.CloseReason, uint64ToDatabaseInt(connection.BytesIn), uint64ToDatabaseInt(connection.BytesOut), connection.RateIn, connection.RateOut, limitJSON, updatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func upsertRuntimeSnapshotTx(ctx context.Context, tx *sql.Tx, snapshot domain.RuntimeSnapshot) error {
	now := snapshot.ObservedAt.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_connections SET state='unknown',updated_at=? WHERE node_id=? AND state='active'`, now.Format(time.RFC3339Nano), snapshot.NodeID); err != nil {
		return err
	}
	byGroup := make(map[string]struct {
		connection domain.RuntimeConnection
		count      uint64
	})
	for _, connection := range snapshot.Connections {
		if err := upsertRuntimeConnectionTx(ctx, tx, connection, now); err != nil {
			return err
		}
		key := connection.AssignmentID + "\x00" + connection.ServiceID + "\x00" + connection.Protocol
		group := byGroup[key]
		group.connection = connection
		group.count++
		byGroup[key] = group
	}
	for _, group := range byGroup {
		rollupConnection := group.connection
		rollupConnection.BytesIn, rollupConnection.BytesOut = 0, 0
		if err := upsertRuntimeRollupTx(ctx, tx, rollupConnection, now, 0, 0, 0, 0, group.count); err != nil {
			return err
		}
	}
	return nil
}

func upsertRuntimeRollupTx(ctx context.Context, tx *sql.Tx, txConnection domain.RuntimeConnection, at time.Time, opened, closed, rejected, limited, activeMax uint64) error {
	bucket := at.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT INTO runtime_traffic_rollups(bucket_start,node_id,gateway_id,agent_id,assignment_id,service_id,protocol,bytes_in,bytes_out,opened,closed,rejected,rate_limited,active_max) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bucket_start,node_id,assignment_id,service_id,protocol) DO UPDATE SET gateway_id=excluded.gateway_id,agent_id=excluded.agent_id,bytes_in=runtime_traffic_rollups.bytes_in+excluded.bytes_in,bytes_out=runtime_traffic_rollups.bytes_out+excluded.bytes_out,opened=runtime_traffic_rollups.opened+excluded.opened,closed=runtime_traffic_rollups.closed+excluded.closed,rejected=runtime_traffic_rollups.rejected+excluded.rejected,rate_limited=runtime_traffic_rollups.rate_limited+excluded.rate_limited,active_max=CASE WHEN excluded.active_max>runtime_traffic_rollups.active_max THEN excluded.active_max ELSE runtime_traffic_rollups.active_max END`, bucket, txConnection.NodeID, txConnection.GatewayID, txConnection.AgentID, txConnection.AssignmentID, txConnection.ServiceID, txConnection.Protocol, uint64ToDatabaseInt(txConnection.BytesIn), uint64ToDatabaseInt(txConnection.BytesOut), uint64ToDatabaseInt(opened), uint64ToDatabaseInt(closed), uint64ToDatabaseInt(rejected), uint64ToDatabaseInt(limited), uint64ToDatabaseInt(activeMax))
	return err
}

func scanRuntimeConnection(row scanner) (domain.RuntimeConnection, error) {
	var connection domain.RuntimeConnection
	var sourcePort int64
	var started, last string
	var ended sql.NullString
	var bytesIn, bytesOut int64
	var limitJSON []byte
	if err := row.Scan(&connection.NodeID, &connection.ID, &connection.Type, &connection.State, &connection.PeerNodeID, &connection.GatewayID, &connection.AgentID, &connection.AssignmentID, &connection.ServiceID, &connection.Protocol, &connection.SourceIP, &sourcePort, &connection.Target, &connection.ParentSessionID, &started, &last, &ended, &connection.CloseReason, &bytesIn, &bytesOut, &connection.RateIn, &connection.RateOut, &limitJSON); err != nil {
		return domain.RuntimeConnection{}, err
	}
	if sourcePort < 0 || sourcePort > math.MaxUint16 {
		return domain.RuntimeConnection{}, errors.New("stored runtime source port is invalid")
	}
	connection.SourcePort = uint16(sourcePort)
	var err error
	connection.StartedAt, err = parseStoredTime("runtime_connection.started_at", started)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	connection.LastActivityAt, err = parseStoredTime("runtime_connection.last_activity_at", last)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	if ended.Valid && ended.String != "" {
		value, parseErr := parseStoredTime("runtime_connection.ended_at", ended.String)
		if parseErr != nil {
			return domain.RuntimeConnection{}, parseErr
		}
		connection.EndedAt = &value
	}
	connection.BytesIn, err = nonNegativeUint64(bytesIn)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	connection.BytesOut, err = nonNegativeUint64(bytesOut)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	if len(limitJSON) > 0 {
		var value domain.RuntimeRateLimit
		if err := json.Unmarshal(limitJSON, &value); err != nil {
			return domain.RuntimeConnection{}, err
		}
		connection.Limit = &value
	}
	if err := connection.Validate(); err != nil {
		return domain.RuntimeConnection{}, fmt.Errorf("stored runtime connection is invalid: %w", err)
	}
	return connection, nil
}

func nonNegativeUint64(value int64) (uint64, error) {
	if value < 0 {
		return 0, errors.New("stored runtime counter is negative")
	}
	return uint64(value), nil
}

func uint64ToDatabaseInt(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func (s *Store) PruneRuntimeHistory(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-runtimeRetention).Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_events WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_traffic_rollups WHERE bucket_start < ?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_connections WHERE ended_at IS NOT NULL AND ended_at < ?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}
