package controller

// Shared SQL and scalar helpers for the normalized resource codecs. Each
// aggregate codec lives in its own file; this file contains only mechanics
// that are independent of Gateway, Agent, Service, Assignment, or Observed
// state.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"asterferry/internal/domain"
)

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func storedUint64(value int64, field string) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("stored %s is negative", field)
	}
	return uint64(value), nil
}

func storedUint16(value int64, field string) (uint16, error) {
	if value <= 0 || value > 65535 {
		return 0, fmt.Errorf("stored %s is outside the uint16 range", field)
	}
	return uint16(value), nil
}

func storedPort(value int64, field string) (uint16, error) {
	if value < 0 || value > 65535 {
		return 0, fmt.Errorf("stored %s is outside the uint16 range", field)
	}
	return uint16(value), nil
}

// requireStoredPosition turns the position columns used by ordered child
// tables into an integrity check. The writers always emit dense zero-based
// positions, so a gap or negative value is corruption rather than a harmless
// ordering detail.
func requireStoredPosition(got int64, expected int, field string) error {
	if got < 0 || got != int64(expected) {
		return fmt.Errorf("stored %s position %d, want %d", field, got, expected)
	}
	return nil
}

func requireStoredKind(kind, field string, allowed ...string) error {
	for _, candidate := range allowed {
		if kind == candidate {
			return nil
		}
	}
	return fmt.Errorf("stored %s kind %q is invalid", field, kind)
}

func loadNodeLabels(ctx context.Context, q sqlQueryer, nodeID string) (map[string]string, error) {
	return loadStringMap(ctx, q, "node_labels", "node_id", nodeID)
}

func loadNodeLabelsForIDs(ctx context.Context, q sqlQueryer, nodeIDs []string) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return result, nil
	}
	for start := 0; start < len(nodeIDs); start += gatewayCandidateBatchSize {
		end := start + gatewayCandidateBatchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		args := make([]any, 0, end-start)
		for _, nodeID := range nodeIDs[start:end] {
			args = append(args, nodeID)
		}
		rows, err := q.QueryContext(ctx, `SELECT node_id,key,value FROM node_labels WHERE node_id IN (`+questionMarks(end-start)+`) ORDER BY node_id,key`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var nodeID, key, value string
			if err := rows.Scan(&nodeID, &key, &value); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if result[nodeID] == nil {
				result[nodeID] = make(map[string]string)
			}
			result[nodeID][key] = value
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadStringMapsForIDs(ctx context.Context, q sqlQueryer, table, ownerColumn string, ids []string) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string, len(ids))
	for start := 0; start < len(ids); start += gatewayCandidateBatchSize {
		end := start + gatewayCandidateBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, `SELECT `+ownerColumn+`,key,value FROM `+table+` WHERE `+ownerColumn+` IN (`+questionMarks(end-start)+`) ORDER BY `+ownerColumn+`,key`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var owner, key, value string
			if err := rows.Scan(&owner, &key, &value); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if result[owner] == nil {
				result[owner] = make(map[string]string)
			}
			result[owner][key] = value
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadNodeIdentity(ctx context.Context, q sqlQueryer, nodeID string) (domain.Node, error) {
	var node domain.Node
	var enabled int
	var created, updated string
	if err := q.QueryRowContext(ctx, `SELECT id,name,enabled,certificate_state,certificate_serial,revision,created_at,updated_at FROM nodes WHERE id=?`, nodeID).Scan(&node.ID, &node.Name, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated); err != nil {
		return domain.Node{}, err
	}
	var err error
	node.Labels, err = loadNodeLabels(ctx, q, node.ID)
	if err != nil {
		return domain.Node{}, err
	}
	node.Enabled = enabled != 0
	node.CreatedAt, err = parseStoredTime("node.created_at", created)
	if err != nil {
		return domain.Node{}, err
	}
	node.UpdatedAt, err = parseStoredTime("node.updated_at", updated)
	if err != nil {
		return domain.Node{}, err
	}
	if err := node.Validate(); err != nil {
		return domain.Node{}, fmt.Errorf("stored node is invalid: %w", err)
	}
	return node, nil
}

func insertNodeLabelsTx(ctx context.Context, tx *sql.Tx, nodeID string, labels map[string]string) error {
	return replaceStringMapTx(ctx, tx, "node_labels", "node_id", nodeID, labels)
}

func loadStringMap(ctx context.Context, q sqlQueryer, table, ownerColumn, owner string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key,value FROM `+table+` WHERE `+ownerColumn+`=? ORDER BY key`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

func replaceStringMapTx(ctx context.Context, tx *sql.Tx, table, ownerColumn, owner string, values map[string]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+ownerColumn+`=?`, owner); err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(`+ownerColumn+`,key,value) VALUES(?,?,?)`, owner, key, values[key]); err != nil {
			return err
		}
	}
	return nil
}
