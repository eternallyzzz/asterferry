package controller

import (
	"sync"
	"sync/atomic"
)

// ChangeBus owns all process-local subscriptions. It has no database handle
// and is safe to close before the shared database handle is released.
type ChangeBus struct {
	actionMu     sync.Mutex
	actionSubs   map[string]map[uint64]*actionSubscription
	snapshotSubs map[string]map[uint64]*snapshotSubscription
	changeMu     sync.Mutex
	changeSubs   map[uint64]*resourceChangeSubscription
	runtimeMu    sync.Mutex
	runtimeSubs  map[uint64]*runtimeChangeSubscription
	close        sync.Once
	closed       atomic.Bool
}

func newChangeBus() *ChangeBus { return &ChangeBus{} }

func (b *ChangeBus) Close() {
	if b == nil {
		return
	}
	b.close.Do(func() {
		b.closed.Store(true)
		b.actionMu.Lock()
		for nodeID, subscribers := range b.actionSubs {
			for id, subscription := range subscribers {
				close(subscription.ch)
				delete(subscribers, id)
			}
			delete(b.actionSubs, nodeID)
		}
		for nodeID, subscribers := range b.snapshotSubs {
			for id, subscription := range subscribers {
				close(subscription.ch)
				delete(subscribers, id)
			}
			delete(b.snapshotSubs, nodeID)
		}
		b.actionMu.Unlock()

		b.changeMu.Lock()
		for id, subscription := range b.changeSubs {
			close(subscription.ch)
			delete(b.changeSubs, id)
		}
		b.changeMu.Unlock()

		b.runtimeMu.Lock()
		for id, subscription := range b.runtimeSubs {
			close(subscription.ch)
			delete(b.runtimeSubs, id)
		}
		b.runtimeMu.Unlock()
	})
}
