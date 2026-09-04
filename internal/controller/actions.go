package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"

	"asterferry/internal/domain"
)

// RuntimeAction is an ephemeral instruction sent over an already-established
// node control stream. The audit record is durable; delivery is best-effort so
// an offline node can apply the action after it reconnects only when an
// operator explicitly repeats it.
type RuntimeAction struct {
	ID      string
	Name    string
	Payload []byte
}

type actionSubscription struct {
	id uint64
	ch chan RuntimeAction
}

var nextActionSubscription atomic.Uint64

// SubscribeActions registers a node control stream as a runtime-action
// recipient. The returned unsubscribe function is idempotent and must be
// called when the stream ends.
func (b *ChangeBus) SubscribeActions(nodeID string) (<-chan RuntimeAction, func()) {
	ch := make(chan RuntimeAction, 16)
	nodeID = strings.TrimSpace(nodeID)
	sub := &actionSubscription{id: nextActionSubscription.Add(1), ch: ch}
	b.actionMu.Lock()
	if b.closed.Load() {
		close(ch)
		b.actionMu.Unlock()
		return ch, func() {}
	}
	if b.actionSubs == nil {
		b.actionSubs = make(map[string]map[uint64]*actionSubscription)
	}
	if b.actionSubs[nodeID] == nil {
		b.actionSubs[nodeID] = make(map[uint64]*actionSubscription)
	}
	b.actionSubs[nodeID][sub.id] = sub
	b.actionMu.Unlock()
	var once atomic.Bool
	return ch, func() {
		if once.Swap(true) {
			return
		}
		b.actionMu.Lock()
		if subscribers := b.actionSubs[nodeID]; subscribers != nil {
			if current := subscribers[sub.id]; current == sub {
				delete(subscribers, sub.id)
				close(current.ch)
			}
			if len(subscribers) == 0 {
				delete(b.actionSubs, nodeID)
			}
		}
		b.actionMu.Unlock()
	}
}

// PublishAction delivers an action to every currently connected stream for a
// node. It never blocks a Controller request on a slow node; a full channel is
// reported as not delivered while the durable audit event remains available.
func (s *ResourceRepository) PublishAction(ctx context.Context, nodeID, name, payload string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	nodeID = strings.TrimSpace(nodeID)
	name = strings.TrimSpace(name)
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return false, err
	}
	if err := validateRuntimeAction(name); err != nil {
		return false, err
	}
	if len(payload) > 1<<20 || strings.ContainsAny(payload, "\x00") {
		return false, errors.New("runtime action payload is too large")
	}
	id, err := randomID()
	if err != nil {
		return false, err
	}
	return s.ChangeBus().publishAction(nodeID, RuntimeAction{ID: id, Name: name, Payload: []byte(payload)}), nil
}

func validateRuntimeAction(name string) error {
	if name != "drain" && name != "reconnect" && name != "resync" && name != "runtime_connection" && name != "clear_runtime_controls" {
		return errors.New("runtime action name is unsupported")
	}
	return nil
}

func (b *ChangeBus) actionCanDeliver(nodeID string) bool {
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	for _, sub := range b.actionSubs[nodeID] {
		if len(sub.ch) < cap(sub.ch) {
			return true
		}
	}
	return false
}

func (b *ChangeBus) publishAction(nodeID string, action RuntimeAction) bool {
	delivered := false
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	if b.closed.Load() {
		return false
	}
	for _, sub := range b.actionSubs[nodeID] {
		select {
		case sub.ch <- RuntimeAction{ID: action.ID, Name: action.Name, Payload: append([]byte(nil), action.Payload...)}:
			delivered = true
		default:
		}
	}
	return delivered
}

// RequestNodeAction durably audits a runtime action and, when a stream is
// connected, publishes it after the audit transaction commits. The optional
// idempotency key covers both the audit and the delivery request, so a client
// retry never emits the same action twice.
func (s *ResourceRepository) RequestNodeAction(ctx context.Context, nodeID, name, payload string, options WriteOptions) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	nodeID = strings.TrimSpace(nodeID)
	name = strings.TrimSpace(name)
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return false, err
	}
	if err := validateRuntimeAction(name); err != nil {
		return false, err
	}
	if len(payload) > 1<<20 || strings.ContainsAny(payload, "\x00") {
		return false, errors.New("runtime action payload is too large")
	}
	if payload != "" {
		var value any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return false, errors.New("runtime action payload must be valid JSON")
		}
	}
	actionID, err := randomID()
	if err != nil {
		return false, err
	}
	request := struct {
		NodeID  string `json:"node_id"`
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}{NodeID: nodeID, Name: name, Payload: payload}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return false, err
	}
	if hit {
		var response struct {
			Delivered bool `json:"delivered"`
		}
		var data []byte
		if err := tx.QueryRowContext(ctx, `SELECT response_json FROM idempotency_keys WHERE key=?`, strings.TrimSpace(options.IdempotencyKey)).Scan(&data); err != nil {
			return false, err
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return false, err
		}
		return response.Delivered, s.commitWriteTx(ctx, tx)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, nodeID).Scan(&exists); err != nil {
		return false, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "action:"+name, "node", nodeID, 0, map[string]string{"action_id": actionID}); err != nil {
		return false, err
	}
	// Delivery is best effort and happens after commit. Persist a delivery
	// hint so an idempotent retry returns the same accepted/queued result
	// without re-publishing the action. The hint is intentionally conservative:
	// an available subscriber is expected to receive the message, while a full
	// broker is reported as queued for an explicit retry.
	deliveryHint := s.ChangeBus().actionCanDeliver(nodeID)
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"action_id": actionID, "delivered": deliveryHint}); err != nil {
		return false, err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return false, err
	}
	delivered := s.ChangeBus().publishAction(nodeID, RuntimeAction{ID: actionID, Name: name, Payload: []byte(payload)})
	if strings.TrimSpace(options.IdempotencyKey) != "" {
		// Return the durable result for idempotent requests. A concurrent stream
		// close may make the best-effort publish return false even though the
		// request was accepted and should remain replay-safe.
		return deliveryHint, nil
	}
	return delivered, nil
}
