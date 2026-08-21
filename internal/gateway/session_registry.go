package gateway

import "sync"

type sessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]*Session)}
}

func (r *sessionRegistry) Add(sess *Session, max int64) (*Session, bool) {
	if r == nil || sess == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.sessions[sess.agentID]
	if old == nil && max > 0 && int64(len(r.sessions)) >= max {
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

func (r *sessionRegistry) CloseAll() {
	for _, sess := range r.Snapshot() {
		sess.Close()
	}
}
