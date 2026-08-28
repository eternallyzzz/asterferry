// Package node contains the common control runtime used by Gateway and Agent
// processes. It only depends on the domain/control protocol packages and has
// no dependency on the Controller implementation.
package node

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"asterferry/internal/atomicfile"
	controlwire "asterferry/internal/controlwire"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
)

const (
	cacheMagic   = "AFSN1"
	cacheKeySize = 32
	maxCacheSize = 64 << 20
)

var (
	ErrNoSnapshot       = errors.New("no cached desired snapshot")
	ErrStaleSnapshot    = errors.New("snapshot generation is stale")
	ErrCacheUnavailable = errors.New("snapshot cache is unavailable")
)

type SnapshotCache struct {
	path string
	key  []byte
	mu   sync.Mutex
}

func NewSnapshotCache(path string, key []byte) (*SnapshotCache, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("snapshot cache path is required")
	}
	if len(key) != cacheKeySize {
		return nil, errors.New("snapshot cache key must contain exactly 32 bytes")
	}
	return &SnapshotCache{path: filepath.Clean(path), key: append([]byte(nil), key...)}, nil
}

func (c *SnapshotCache) Path() string { return c.path }

func (c *SnapshotCache) Write(snapshot domain.DesiredSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.Checksum == "" {
		withChecksum, err := snapshot.WithChecksum()
		if err != nil {
			return err
		}
		snapshot = withChecksum
	}
	document, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(document) > maxCacheSize {
		return errors.New("snapshot cache document is too large")
	}
	ciphertext, err := encrypt(c.key, document)
	if err != nil {
		return err
	}
	payload := append([]byte(cacheMagic), ciphertext...)
	c.mu.Lock()
	defer c.mu.Unlock()
	return atomicfile.AtomicWrite(c.path, payload, 0o600)
}

func (c *SnapshotCache) Read() (domain.DesiredSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.DesiredSnapshot{}, ErrNoSnapshot
	}
	if err != nil {
		return domain.DesiredSnapshot{}, fmt.Errorf("read snapshot cache: %w", err)
	}
	if len(b) < len(cacheMagic)+1 || len(b) > maxCacheSize+256 || string(b[:len(cacheMagic)]) != cacheMagic {
		return domain.DesiredSnapshot{}, errors.New("snapshot cache has an invalid header")
	}
	plaintext, err := decrypt(c.key, b[len(cacheMagic):])
	if err != nil {
		return domain.DesiredSnapshot{}, errors.New("snapshot cache authentication failed")
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(plaintext, &snapshot); err != nil {
		return domain.DesiredSnapshot{}, errors.New("snapshot cache document is invalid")
	}
	if err := snapshot.Validate(); err != nil {
		return domain.DesiredSnapshot{}, err
	}
	// A cache is a last-known-good control artifact, not an optional JSON
	// convenience. Requiring the authenticated checksum here prevents a
	// hand-edited (or truncated) document with a valid structural shape from
	// becoming the node's durable generation after a restart.
	if strings.TrimSpace(snapshot.Checksum) == "" {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "missing_checksum", Path: "checksum", Message: "cached snapshot checksum is required"}
	}
	computedChecksum, err := snapshot.ComputeChecksum()
	if err != nil {
		return domain.DesiredSnapshot{}, fmt.Errorf("compute cached snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computedChecksum, snapshot.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "cached snapshot checksum does not match its content"}
	}
	return snapshot, nil
}

type ApplyFunc func(context.Context, domain.DesiredSnapshot, *domain.DesiredSnapshot) error
type ResetFunc func(context.Context, uint64) error

type Reconciler struct {
	cache          *SnapshotCache
	apply          ApplyFunc
	reset          ResetFunc
	applyMu        sync.Mutex
	mu             sync.Mutex
	last           uint64
	lastChecksum   string
	state          domain.ObservedState
	disconnectedAt time.Time
}

func NewReconciler(cache *SnapshotCache, apply ApplyFunc) (*Reconciler, error) {
	return NewReconcilerWithReset(cache, apply, nil)
}

// NewReconcilerWithReset is the transactional form used by a node runtime
// that owns external resources (listeners, sessions and an authorization
// index). If the very first snapshot is applied successfully but publishing
// its encrypted cache fails, there is no previous document to pass to
// ApplyFunc for rollback; ResetFunc clears that speculative generation.
func NewReconcilerWithReset(cache *SnapshotCache, apply ApplyFunc, reset ResetFunc) (*Reconciler, error) {
	if cache == nil {
		return nil, errors.New("snapshot cache is required")
	}
	if apply == nil {
		apply = func(context.Context, domain.DesiredSnapshot, *domain.DesiredSnapshot) error { return nil }
	}
	reconciler := &Reconciler{cache: cache, apply: apply, reset: reset, state: domain.ObservedState{SchemaVersion: domain.SchemaVersion}}
	if snapshot, err := cache.Read(); err == nil {
		reconciler.last = snapshot.Generation
		reconciler.lastChecksum = snapshot.Checksum
		reconciler.state.NodeID = snapshot.NodeID
		reconciler.state.AppliedGeneration = snapshot.Generation
		reconciler.state.Healthy = true
		reconciler.state.ObservedAt = time.Now().UTC()
	} else if !errors.Is(err, ErrNoSnapshot) {
		reconciler.state.Healthy = false
		reconciler.state.Degraded = true
		reconciler.state.LastError = &domain.ApplyError{Code: "cache_read_failed", Message: err.Error()}
	}
	return reconciler, nil
}

func (r *Reconciler) AppliedGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// AppliedChecksum returns the checksum of the last successfully applied
// snapshot. It is carried in Hello so the Controller can repair a same-
// generation cache divergence without advancing the desired generation.
func (r *Reconciler) AppliedChecksum() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastChecksum
}

func (r *Reconciler) State() domain.ObservedState {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := r.state
	clone.Sessions = append([]domain.SessionSummary(nil), r.state.Sessions...)
	clone.Listeners = append([]domain.ListenerState(nil), r.state.Listeners...)
	if r.state.Metrics != nil {
		clone.Metrics = make(map[string]float64, len(r.state.Metrics))
		for key, value := range r.state.Metrics {
			clone.Metrics[key] = value
		}
	}
	return clone
}

// SetNodeID initializes the observed identity before the first snapshot is
// received. A bootstrap with no cached state must still be able to report a
// valid observed document to the Controller.
func (r *Reconciler) SetNodeID(nodeID string) error {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.NodeID != "" && r.state.NodeID != nodeID {
		return errors.New("reconciler node id does not match bootstrap")
	}
	r.state.NodeID = nodeID
	if r.state.ObservedAt.IsZero() {
		r.state.ObservedAt = time.Now().UTC()
	}
	return nil
}

func (r *Reconciler) Apply(ctx context.Context, snapshot domain.DesiredSnapshot) *v1.ApplyResult {
	if ctx == nil {
		ctx = context.Background()
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	// A Controller may resend the same generation with a different checksum
	// when a node's encrypted cache diverged (for example after a local disk
	// restore).  Normalize and validate before comparing generations so this
	// authoritative repair is distinguishable from an exact replay.
	if snapshot.Checksum == "" {
		canonical, err := snapshot.WithChecksum()
		if err != nil {
			return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, toApplyError(err))
		}
		snapshot = canonical
	}
	if err := snapshot.Validate(); err != nil {
		applyErr := toApplyError(err)
		return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyErr)
	}
	r.mu.Lock()
	if r.state.NodeID != "" && snapshot.NodeID != r.state.NodeID {
		r.mu.Unlock()
		return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, &domain.ApplyError{Code: "node_mismatch", Path: "node_id", Message: "snapshot node id does not match reconciler"})
	}
	if snapshot.Generation == 0 || snapshot.Generation < r.last || (snapshot.Generation == r.last && strings.EqualFold(snapshot.Checksum, r.lastChecksum)) {
		generation := snapshot.Generation
		if generation == 0 {
			generation = r.last
		}
		r.mu.Unlock()
		return controlwire.ApplyResult(generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, &domain.ApplyError{Code: "stale_generation", Path: "generation", Message: ErrStaleSnapshot.Error(), Retryable: false})
	}
	r.mu.Unlock()
	var previous *domain.DesiredSnapshot
	if old, err := r.cache.Read(); err == nil {
		r.mu.Lock()
		last := r.last
		r.mu.Unlock()
		// The in-memory generation is initialized from the cache and advances
		// only after a successful apply+cache write. If the file is replaced
		// underneath a running node, do not pass a newer/older unrelated
		// document to the component rollback callback.
		if last != 0 && old.Generation != last {
			applyErr := &domain.ApplyError{Code: "cache_generation_mismatch", Path: "generation", Message: "snapshot cache generation differs from the applied generation", Retryable: true}
			r.mu.Lock()
			r.state.Healthy = false
			r.state.Degraded = true
			r.state.LastError = applyErr
			r.mu.Unlock()
			return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyErr)
		}
		previous = &old
	}
	if err := r.apply(ctx, snapshot, previous); err != nil {
		// Component callbacks may already have classified a failure (for
		// example an invalid listener bind with a field path or a non-retryable
		// authorization error). Preserve that stable error envelope across the
		// reconciler boundary instead of flattening every failure to
		// apply_failed. Unknown callback errors remain retryable because the
		// Controller can safely resend the immutable snapshot.
		applyErr := toApplyFailure(err)
		r.mu.Lock()
		r.state.Healthy = false
		r.state.Degraded = true
		r.state.LastError = applyErr
		r.mu.Unlock()
		return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyErr)
	}
	if err := r.cache.Write(snapshot); err != nil {
		rollbackErr := error(nil)
		if previous != nil {
			// Best-effort component rollback. The callback must itself be
			// transactional; this keeps the applied generation aligned with the
			// durable cache when the final atomic file publish fails.
			rollbackErr = r.apply(ctx, *previous, &snapshot)
		} else if r.reset != nil {
			// The first snapshot had no durable last-known-good generation. Do
			// not leave listeners or authorization entries active when the
			// encrypted cache cannot be published; a retry must start from an
			// empty, drained data plane.
			rollbackErr = r.reset(ctx, snapshot.Generation)
		}
		message := err.Error()
		if rollbackErr != nil {
			message += "; data-plane rollback failed: " + rollbackErr.Error()
		}
		applyErr := &domain.ApplyError{Code: "cache_write_failed", Message: message, Retryable: true}
		r.mu.Lock()
		r.state.Healthy = false
		r.state.Degraded = true
		r.state.LastError = applyErr
		r.mu.Unlock()
		return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyErr)
	}
	r.mu.Lock()
	r.last = snapshot.Generation
	r.lastChecksum = snapshot.Checksum
	r.state = domain.ObservedState{SchemaVersion: domain.SchemaVersion, NodeID: snapshot.NodeID, AppliedGeneration: snapshot.Generation, Healthy: true, ObservedAt: time.Now().UTC()}
	r.disconnectedAt = time.Time{}
	r.mu.Unlock()
	return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_APPLIED, nil)
}

// Resync reapplies the durable last-known-good snapshot without advancing its
// generation. It is used by the Controller's explicit resync action after a
// local component restart; normal DesiredSnapshot delivery rejects exact
// replays and older generations while allowing an authoritative same-generation
// checksum repair.
func (r *Reconciler) Resync(ctx context.Context) *v1.ApplyResult {
	if ctx == nil {
		ctx = context.Background()
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	snapshot, err := r.cache.Read()
	if err != nil {
		return controlwire.ApplyResult(r.AppliedGeneration(), "", v1.ApplyStatus_APPLY_STATUS_REJECTED, &domain.ApplyError{Code: "cache_read_failed", Message: err.Error(), Retryable: true})
	}
	if err := snapshot.Validate(); err != nil {
		return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, toApplyError(err))
	}
	if err := r.apply(ctx, snapshot, &snapshot); err != nil {
		applyErr := toApplyFailure(err)
		if applyErr.Code == "apply_failed" {
			applyErr.Code = "resync_failed"
		}
		return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyErr)
	}
	r.mu.Lock()
	r.state.Healthy = true
	r.state.Degraded = false
	r.state.LastError = nil
	r.state.ObservedAt = time.Now().UTC()
	r.mu.Unlock()
	return controlwire.ApplyResult(snapshot.Generation, snapshot.Checksum, v1.ApplyStatus_APPLY_STATUS_APPLIED, nil)
}

// MarkConnected records a successful control-stream handshake and clears the
// offline grace timer. A failed apply remains visible until the next success.
func (r *Reconciler) MarkConnected(now time.Time) domain.ObservedState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	r.disconnectedAt = time.Time{}
	if r.state.LastError == nil {
		r.state.Healthy = true
		r.state.Degraded = false
	}
	r.state.ObservedAt = now.UTC()
	return r.state
}

func (r *Reconciler) MarkDisconnected(now time.Time, grace time.Duration) domain.ObservedState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if grace <= 0 {
		grace = 30 * time.Second
	}
	if now.IsZero() {
		now = time.Now()
	}
	if r.disconnectedAt.IsZero() {
		r.disconnectedAt = now
	}
	if now.Sub(r.disconnectedAt) >= grace {
		r.state.Degraded = true
		r.state.Healthy = false
	}
	r.state.ObservedAt = now.UTC()
	return r.state
}

func toApplyError(err error) *domain.ApplyError {
	var applyErr *domain.ApplyError
	if errors.As(err, &applyErr) {
		return applyErr
	}
	return &domain.ApplyError{Code: "invalid_snapshot", Message: err.Error(), Retryable: false}
}

func toApplyFailure(err error) *domain.ApplyError {
	var applyErr *domain.ApplyError
	if errors.As(err, &applyErr) && applyErr != nil {
		clone := *applyErr
		if strings.TrimSpace(clone.Code) == "" {
			clone.Code = "apply_failed"
		}
		if strings.TrimSpace(clone.Message) == "" {
			clone.Message = err.Error()
		}
		return &clone
	}
	return &domain.ApplyError{Code: "apply_failed", Message: err.Error(), Retryable: true}
}

func encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, plaintext, nil)...), nil
}

func decrypt(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("ciphertext is truncated")
	}
	return gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
}

func CacheKeyString(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }
