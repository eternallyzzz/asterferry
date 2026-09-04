package controller

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	controllerLeaseTTL        = 15 * time.Second
	controllerLeaseRenewEvery = 5 * time.Second
	controllerLeaseRetryEvery = time.Second
)

var (
	// ErrControllerNotLeader is returned to business surfaces while this
	// process is a standby or has lost its database lease.
	ErrControllerNotLeader = errors.New("controller is not the active leader")
	// ErrLeadershipLost is returned by a fenced write that started while this
	// process was leader but crossed a lease/epoch change before commit.
	ErrLeadershipLost = errors.New("controller leadership was lost during the operation")
)

type leadershipEvent struct {
	Leader bool
	Epoch  uint64
	Err    error
}

// leadership owns the process-local view of a PostgreSQL lease. The durable
// row is the authority; the local state is only an admission/readiness cache.
// ownerID is generated for every process start so a restarted or duplicated
// pod can never renew an older process's lease by reusing a configured name.
type leadership struct {
	database *databaseHandle
	enabled  bool
	ownerID  string
	metrics  *ControllerMetrics

	mu           sync.RWMutex
	active       bool
	epoch        uint64
	observers    map[uint64]func(leadershipEvent)
	nextObserver uint64
}

func newLeadership(database *databaseHandle, enabled bool, metrics *ControllerMetrics) (*leadership, error) {
	if database == nil {
		return nil, errors.New("leadership database is required")
	}
	ownerID, err := randomLeadershipOwnerID()
	if err != nil {
		return nil, err
	}
	result := &leadership{database: database, enabled: enabled, ownerID: ownerID, metrics: metrics, observers: make(map[uint64]func(leadershipEvent))}
	if !enabled {
		result.active = true
		if metrics != nil {
			metrics.setLeadership(true, 0)
		}
	}
	return result, nil
}

func randomLeadershipOwnerID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate controller owner id: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func (l *leadership) Enabled() bool {
	return l != nil && l.enabled
}

func (l *leadership) IsLeader() bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.active
}

func (l *leadership) Epoch() uint64 {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.epoch
}

func (l *leadership) OwnerID() string {
	if l == nil {
		return ""
	}
	return l.ownerID
}

func (l *leadership) observe(callback func(leadershipEvent)) func() {
	if l == nil || callback == nil {
		return func() {}
	}
	l.mu.Lock()
	l.nextObserver++
	id := l.nextObserver
	if l.observers == nil {
		l.observers = make(map[uint64]func(leadershipEvent))
	}
	l.observers[id] = callback
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.observers, id)
			l.mu.Unlock()
		})
	}
}

func (l *leadership) RequireLeader() error {
	if l == nil || !l.IsLeader() {
		return ErrControllerNotLeader
	}
	return nil
}

func (l *leadership) Run(ctx context.Context) {
	if l == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.enabled {
		<-ctx.Done()
		return
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		l.release(releaseCtx)
		cancel()
	}()

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if l.IsLeader() {
			if err := l.renew(ctx); err != nil {
				if l.metrics != nil {
					l.metrics.recordLeaseRenewFailure()
				}
				l.setStandby(err)
			}
		} else if epoch, err := l.acquire(ctx); err != nil {
			if !errors.Is(err, ErrControllerNotLeader) && ctx.Err() == nil {
				l.log("controller leadership acquisition failed", err)
			}
		} else {
			l.setLeader(epoch)
		}
		delay := controllerLeaseRetryEvery
		if l.IsLeader() {
			delay = controllerLeaseRenewEvery
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
}

func controllerLeaseExpirySQL() string {
	return fmt.Sprintf("CURRENT_TIMESTAMP + INTERVAL '%d seconds'", int(controllerLeaseTTL/time.Second))
}

func (l *leadership) acquire(ctx context.Context) (uint64, error) {
	tx, err := l.database.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE controller_leases
		SET owner_id=?, fencing_epoch=fencing_epoch+1,
			lease_until=`+controllerLeaseExpirySQL()+`,
			updated_at=CURRENT_TIMESTAMP
		WHERE singleton=1 AND (owner_id='' OR lease_until <= CURRENT_TIMESTAMP)`, l.ownerID)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, ErrControllerNotLeader
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT fencing_epoch FROM controller_leases WHERE singleton=1`+l.database.selectForUpdateClause()).Scan(&epoch); err != nil {
		return 0, err
	}
	if epoch <= 0 {
		return 0, errors.New("controller lease returned an invalid fencing epoch")
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint64(epoch), nil
}

func (l *leadership) renew(ctx context.Context) error {
	if l == nil || !l.IsLeader() {
		return ErrControllerNotLeader
	}
	epoch := l.Epoch()
	result, err := l.database.db.ExecContext(ctx, `UPDATE controller_leases
		SET lease_until=`+controllerLeaseExpirySQL()+`, updated_at=CURRENT_TIMESTAMP
		WHERE singleton=1 AND owner_id=? AND fencing_epoch=? AND lease_until > CURRENT_TIMESTAMP`, l.ownerID, epoch)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeadershipLost
	}
	return nil
}

func (l *leadership) release(ctx context.Context) {
	if l == nil || !l.enabled {
		return
	}
	if !l.IsLeader() {
		return
	}
	_, err := l.database.db.ExecContext(ctx, `UPDATE controller_leases SET owner_id='', lease_until=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE singleton=1 AND owner_id=? AND fencing_epoch=?`, l.ownerID, l.Epoch())
	if err != nil {
		l.log("controller leadership release failed", err)
	}
	l.setStandby(err)
}

func (l *leadership) setLeader(epoch uint64) {
	l.mu.Lock()
	if l.active && l.epoch == epoch {
		l.mu.Unlock()
		return
	}
	l.active = true
	l.epoch = epoch
	l.mu.Unlock()
	if l.metrics != nil {
		l.metrics.setLeadership(true, epoch)
		l.metrics.recordLeadershipChange()
	}
	l.notify(leadershipEvent{Leader: true, Epoch: epoch})
	l.log("controller became leader", nil)
}

func (l *leadership) setStandby(err error) {
	l.mu.Lock()
	wasActive := l.active
	l.active = false
	l.epoch = 0
	l.mu.Unlock()
	if l.metrics != nil {
		l.metrics.setLeadership(false, 0)
	}
	if wasActive {
		if l.metrics != nil {
			l.metrics.recordLeadershipChange()
		}
		l.notify(leadershipEvent{Leader: false, Err: err})
		l.log("controller became standby", err)
	}
}

func (l *leadership) notify(event leadershipEvent) {
	if l == nil {
		return
	}
	l.mu.RLock()
	callbacks := make([]func(leadershipEvent), 0, len(l.observers))
	for _, callback := range l.observers {
		callbacks = append(callbacks, callback)
	}
	l.mu.RUnlock()
	for _, callback := range callbacks {
		callback(event)
	}
}

func (l *leadership) assertWrite(ctx context.Context, tx *sql.Tx) error {
	if l == nil || !l.enabled {
		return nil
	}
	if err := l.RequireLeader(); err != nil {
		return err
	}
	var owner string
	var epoch int64
	var valid bool
	query := `SELECT owner_id, fencing_epoch, lease_until > CURRENT_TIMESTAMP FROM controller_leases WHERE singleton=1` + l.database.selectForUpdateClause()
	if err := tx.QueryRowContext(ctx, query).Scan(&owner, &epoch, &valid); err != nil {
		return err
	}
	if epoch <= 0 || owner != l.ownerID || uint64(epoch) != l.Epoch() || !valid {
		return ErrLeadershipLost
	}
	return nil
}

func (l *leadership) log(message string, err error) {
	if l == nil {
		return
	}
	if err == nil {
		slog.Default().Info(message, "owner_id", l.ownerID, "epoch", l.Epoch())
		return
	}
	slog.Default().Warn(message, "owner_id", l.ownerID, "epoch", l.Epoch(), "error", err)
}

func (s *ResourceRepository) beginWriteTx(ctx context.Context) (*sql.Tx, error) {
	if s == nil || s.databaseHandle == nil {
		return nil, errors.New("resource repository is not initialized")
	}
	if s.leadership != nil {
		if err := s.leadership.RequireLeader(); err != nil {
			return nil, err
		}
	}
	return s.db.BeginTx(ctx, nil)
}

func (s *ResourceRepository) commitWriteTx(ctx context.Context, tx *sql.Tx) error {
	if s != nil && s.leadership != nil {
		if err := s.leadership.assertWrite(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *RuntimeRepository) beginWriteTx(ctx context.Context) (*sql.Tx, error) {
	if s == nil || s.databaseHandle == nil {
		return nil, errors.New("runtime repository is not initialized")
	}
	if s.leadership != nil {
		if err := s.leadership.RequireLeader(); err != nil {
			return nil, err
		}
	}
	return s.db.BeginTx(ctx, nil)
}

func (s *RuntimeRepository) commitWriteTx(ctx context.Context, tx *sql.Tx) error {
	if s != nil && s.leadership != nil {
		if err := s.leadership.assertWrite(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
