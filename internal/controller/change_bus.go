package controller

import (
	"strings"
	"sync"
	"sync/atomic"
)

// ChangeBus owns all process-local subscriptions. It has no database handle
// and is safe to close before the shared database handle is released.
type ChangeBus struct {
	actionMu       sync.Mutex
	actionSubs     map[string]map[uint64]*actionSubscription
	snapshotSubs   map[string]map[uint64]*snapshotSubscription
	changeMu       sync.Mutex
	changeSubs     map[uint64]*resourceChangeSubscription
	runtimeMu      sync.Mutex
	runtimeSubs    map[uint64]*runtimeChangeSubscription
	capabilityMu   sync.RWMutex
	capabilities   map[string]nodeCapability
	nextCapability atomic.Uint64
	close          sync.Once
	closed         atomic.Bool
}

type nodeCapability struct {
	token uint64
	items []string
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

		b.capabilityMu.Lock()
		b.capabilities = nil
		b.capabilityMu.Unlock()
	})
}

// SetNodeCapabilities records the capabilities from the currently
// authenticated control stream. The token lets a reconnecting stream remove
// only its own registration while an overlapping old stream unwinds.
func (b *ChangeBus) SetNodeCapabilities(nodeID string, capabilities []string) uint64 {
	if b == nil {
		return 0
	}
	token := b.nextCapability.Add(1)
	items := append([]string(nil), capabilities...)
	b.capabilityMu.Lock()
	if b.capabilities == nil {
		b.capabilities = make(map[string]nodeCapability)
	}
	if !b.closed.Load() {
		b.capabilities[strings.TrimSpace(nodeID)] = nodeCapability{token: token, items: items}
	}
	b.capabilityMu.Unlock()
	return token
}

func (b *ChangeBus) ClearNodeCapabilities(nodeID string, token uint64) {
	if b == nil {
		return
	}
	b.capabilityMu.Lock()
	defer b.capabilityMu.Unlock()
	if current, ok := b.capabilities[strings.TrimSpace(nodeID)]; ok && current.token == token {
		delete(b.capabilities, strings.TrimSpace(nodeID))
	}
}

func (b *ChangeBus) NodeCapabilities(nodeID string) []string {
	if b == nil {
		return nil
	}
	b.capabilityMu.RLock()
	defer b.capabilityMu.RUnlock()
	current, ok := b.capabilities[strings.TrimSpace(nodeID)]
	if !ok {
		return nil
	}
	return append([]string(nil), current.items...)
}
