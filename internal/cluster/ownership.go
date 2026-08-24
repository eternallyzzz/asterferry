// Package cluster contains the small, data-plane-independent seams needed for
// future Gateway coordination. The first implementation is deliberately
// local: it keeps the single-node deployment behavior unchanged while making
// ownership semantics explicit for a later Kubernetes Lease adapter.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

const (
	maxNodeIDLength    = 128
	sessionIDByteCount = 16
)

var (
	ErrInvalidNodeID = errors.New("invalid cluster node id")
	ErrInvalidOwner  = errors.New("invalid cluster owner")
)

// ResolveNodeID returns the configured node identity, falling back to the
// host name for deployments that do not need a stable cluster identity yet.
// The value is metadata only in the current release; no distributed
// coordination is enabled by setting it.
func ResolveNodeID(configured string) (string, error) {
	nodeID := strings.TrimSpace(configured)
	if nodeID == "" {
		var err error
		nodeID, err = os.Hostname()
		if err != nil {
			return "", fmt.Errorf("resolve cluster node id: %w", err)
		}
		nodeID = strings.TrimSpace(nodeID)
	}
	if err := ValidateNodeID(nodeID); err != nil {
		return "", err
	}
	return nodeID, nil
}

// ValidateNodeID accepts an auditable host-style identifier. It intentionally
// excludes whitespace and punctuation that is awkward in logs, labels, and
// future Kubernetes object names.
func ValidateNodeID(nodeID string) error {
	if nodeID == "" || len(nodeID) > maxNodeIDLength {
		return ErrInvalidNodeID
	}
	for index, ch := range nodeID {
		alphaNumeric := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		if (index == 0 || index == len(nodeID)-1) && !alphaNumeric {
			return ErrInvalidNodeID
		}
		if alphaNumeric || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return ErrInvalidNodeID
	}
	return nil
}

// NewSessionID creates an opaque identifier for one local transport session.
// It is used for ownership and logs only; it is not sent over the v4 wire.
func NewSessionID() (string, error) {
	var raw [sessionIDByteCount]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate cluster session id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Owner identifies the Gateway node currently serving one Agent session.
// SessionID is intentionally local and opaque so this metadata cannot become
// a protocol compatibility requirement.
type Owner struct {
	AgentID   string
	SessionID string
	NodeID    string
}

func (o Owner) valid() bool {
	return o.AgentID != "" && o.SessionID != "" && ValidateNodeID(o.NodeID) == nil
}

// OwnerStore is the coordination boundary for Agent ownership. A future
// Kubernetes Lease implementation can satisfy this interface without moving
// data streams or putting per-packet state in a database.
type OwnerStore interface {
	Claim(context.Context, Owner) (previous Owner, replaced bool, err error)
	Release(context.Context, Owner) error
	Lookup(context.Context, string) (Owner, bool, error)
}

// LocalOwnerStore provides atomic, process-local ownership semantics. A stale
// release is a no-op, which prevents an old session from deleting a newer
// replacement after reconnect.
type LocalOwnerStore struct {
	mu     sync.RWMutex
	owners map[string]Owner
}

func NewLocalOwnerStore() *LocalOwnerStore {
	return &LocalOwnerStore{owners: make(map[string]Owner)}
}

func (s *LocalOwnerStore) Claim(ctx context.Context, owner Owner) (Owner, bool, error) {
	if err := contextErr(ctx); err != nil {
		return Owner{}, false, err
	}
	if s == nil || !owner.valid() {
		return Owner{}, false, ErrInvalidOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, replaced := s.owners[owner.AgentID]
	s.owners[owner.AgentID] = owner
	return previous, replaced, nil
}

func (s *LocalOwnerStore) Release(ctx context.Context, owner Owner) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if s == nil || !owner.valid() {
		return ErrInvalidOwner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.owners[owner.AgentID]; ok && current == owner {
		delete(s.owners, owner.AgentID)
	}
	return nil
}

func (s *LocalOwnerStore) Lookup(ctx context.Context, agentID string) (Owner, bool, error) {
	if err := contextErr(ctx); err != nil {
		return Owner{}, false, err
	}
	if s == nil || strings.TrimSpace(agentID) == "" {
		return Owner{}, false, ErrInvalidOwner
	}
	s.mu.RLock()
	owner, ok := s.owners[agentID]
	s.mu.RUnlock()
	return owner, ok, nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
