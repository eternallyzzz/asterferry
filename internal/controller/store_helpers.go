package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"time"

	"asterferry/internal/domain"
)

// RecordEvent persists a node-originated event in the same audit stream used
// by API writes. Event payloads are intentionally bounded and stored as
// attributes rather than allowing arbitrary SQL-visible columns.
func (s *Store) RecordEvent(ctx context.Context, actor, eventID, eventType, message, resourceID string, attributes map[string]string) error {
	eventID = strings.TrimSpace(eventID)
	eventType = strings.TrimSpace(eventType)
	if len(eventID) > 128 || len(eventType) == 0 || len(eventType) > 128 || strings.ContainsAny(eventID+eventType, "\x00\r\n") {
		return errors.New("event id or type is invalid")
	}
	if len(message) > 4096 || len(attributes) > 64 {
		return errors.New("event payload is too large")
	}
	for key, value := range attributes {
		if len(key) > 128 || len(value) > 2048 || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("event attribute is invalid")
		}
	}
	if len(attributes)+2 > 64 {
		return errors.New("event attributes are too numerous")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAudit(ctx, tx, actor, "event:"+strings.TrimSpace(eventType), "event", strings.TrimSpace(resourceID), 0, attributesWithMessage(attributes, message, eventID)); err != nil {
		return err
	}
	return tx.Commit()
}

func attributesWithMessage(attributes map[string]string, message, eventID string) map[string]string {
	result := make(map[string]string, len(attributes)+2)
	for key, value := range attributes {
		result[key] = value
	}
	if eventID != "" {
		result["event_id"] = eventID
	}
	if message != "" {
		result["message"] = message
	}
	return result
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,role,enabled,revision,created_at,updated_at,password_changed_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []User{}
	for rows.Next() {
		user, scanErr := scanUser(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, user)
	}
	return result, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT id,username,role,enabled,revision,created_at,updated_at,password_changed_at FROM users WHERE id=?`, id))
}

func getUserTx(ctx context.Context, tx *sql.Tx, field, value string) (User, error) {
	if field != "id" && field != "username" {
		return User{}, errors.New("invalid user lookup field")
	}
	query := `SELECT id,username,role,enabled,revision,created_at,updated_at,password_changed_at FROM users WHERE ` + field + `=?`
	return scanUser(tx.QueryRowContext(ctx, query, value))
}

/*
The public User model deliberately does not expose PasswordChangedAt, but
the controller keeps it on every authenticated copy so in-memory sessions
can be invalidated without retaining or comparing password material.
*/
func scanUser(row scanner) (User, error) {
	var user User
	var enabled int
	var created, updated, passwordChanged string
	if err := row.Scan(&user.ID, &user.Username, &user.Role, &enabled, &user.Revision, &created, &updated, &passwordChanged); err != nil {
		return User{}, err
	}
	user.Enabled = enabled != 0
	var err error
	user.CreatedAt, err = parseStoredTime("user.created_at", created)
	if err != nil {
		return User{}, err
	}
	user.UpdatedAt, err = parseStoredTime("user.updated_at", updated)
	if err != nil {
		return User{}, err
	}
	user.PasswordChangedAt, err = parseStoredTime("user.password_changed_at", passwordChanged)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,actor,action,resource,resource_id,revision,attributes_json,created_at FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AuditRecord{}
	for rows.Next() {
		var record AuditRecord
		var attributes []byte
		var created string
		if err := rows.Scan(&record.ID, &record.Actor, &record.Action, &record.Resource, &record.ResourceID, &record.Revision, &attributes, &created); err != nil {
			return nil, err
		}
		if len(attributes) > 0 {
			if err := json.Unmarshal(attributes, &record.Attributes); err != nil {
				return nil, fmt.Errorf("%w: invalid audit attributes: %w", ErrStorageFailure, err)
			}
		}
		record.CreatedAt, err = parseStoredTime("audit.created_at", created)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func insertAudit(ctx context.Context, tx *sql.Tx, actor, action, resource, resourceID string, revision int64, attributes map[string]string) error {
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	b, err := json.Marshal(attributes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,resource,resource_id,revision,attributes_json,created_at) VALUES(?,?,?,?,?,?,?)`, actor, action, resource, resourceID, revision, b, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanNode(row scanner) (domain.Node, error) {
	var node domain.Node
	var labels []byte
	var enabled int
	var created, updated string
	if err := row.Scan(&node.ID, &node.Name, &labels, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated); err != nil {
		return domain.Node{}, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &node.Labels); err != nil {
			return domain.Node{}, err
		}
	}
	node.Enabled = enabled != 0
	parsed, err := parseStoredTime("node.created_at", created)
	if err != nil {
		return domain.Node{}, err
	}
	node.CreatedAt = parsed
	node.UpdatedAt, err = parseStoredTime("node.updated_at", updated)
	if err != nil {
		return domain.Node{}, err
	}
	if err := node.Validate(); err != nil {
		return domain.Node{}, fmt.Errorf("stored node is invalid: %w", err)
	}
	return node, nil
}

func validateNode(node domain.Node) error {
	return node.Validate()
}

func normalizeBind(value string) string {
	value = strings.TrimSpace(value)
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String()
	}
	return value
}

func defaultCertificateState(state string) string {
	if state == "" {
		return domain.CertificatePending
	}
	return state
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func uint64ToRevision(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func selectorsEqual(left, right domain.Selector) bool {
	if len(left.MatchLabels) != len(right.MatchLabels) {
		return false
	}
	for key, value := range left.MatchLabels {
		if right.MatchLabels[key] != value {
			return false
		}
	}
	return true
}

func sameServiceContent(left, right domain.Service) bool {
	left.Revision, right.Revision = 0, 0
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	leftHash, leftErr := requestHash(left)
	rightHash, rightErr := requestHash(right)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}
