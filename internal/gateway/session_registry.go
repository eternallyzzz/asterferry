package gateway

import (
	"context"
	"errors"
	"sync"

	"asterferry/internal/cluster"
)

// sessionDirectory is the Gateway-local seam for Agent ownership. A future
// coordinator can preserve these lifecycle operations while storing ownership
// metadata outside the process; streams themselves remain local.
type sessionDirectory interface {
	Add(*Session, int64) (*Session, bool)
	Remove(*Session)
	Snapshot() []*Session
	Count() int
	CloseAll() error
}

type sessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	nodeID   string
	owners   cluster.OwnerStore
}

func newSessionRegistry(nodeID string, stores ...cluster.OwnerStore) *sessionRegistry {
	if nodeID == "" {
		nodeID = "local"
	}
	var owners cluster.OwnerStore
	if len(stores) > 0 {
		owners = stores[0]
	}
	if owners == nil {
		owners = cluster.NewLocalOwnerStore()
	}
	return &sessionRegistry{sessions: make(map[string]*Session), nodeID: nodeID, owners: owners}
}

func (r *sessionRegistry) Add(sess *Session, max int64) (*Session, bool) {
	if r == nil || sess == nil {
		return nil, false
	}
	if sess.sessionID == "" {
		id, err := cluster.NewSessionID()
		if err != nil {
			return nil, false
		}
		sess.sessionID = id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.sessions[sess.agentID]
	if old == nil && max > 0 && int64(len(r.sessions)) >= max {
		return nil, false
	}
	if _, _, err := r.owners.Claim(context.Background(), sess.owner(r.nodeID)); err != nil {
		return nil, false
	}
	r.sessions[sess.agentID] = sess
	return old, true
}

func (r *sessionRegistry) Remove(sess *Session) {
	if r == nil || sess == nil {
		return
	}
	r.mu.Lock()
	if r.sessions[sess.agentID] == sess {
		delete(r.sessions, sess.agentID)
	}
	r.mu.Unlock()
	if r.owners != nil && sess.sessionID != "" {
		_ = r.owners.Release(context.Background(), sess.owner(r.nodeID))
	}
}

func (r *sessionRegistry) Snapshot() []*Session {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	result := make([]*Session, 0, len(r.sessions))
	for _, sess := range r.sessions {
		result = append(result, sess)
	}
	r.mu.RUnlock()
	return result
}

func (r *sessionRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

func (r *sessionRegistry) CloseAll() error {
	var errs []error
	for _, sess := range r.Snapshot() {
		if err := sess.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
