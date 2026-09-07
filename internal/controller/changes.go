package controller

import (
	"sync"
	"sync/atomic"
)

// ResourceChange is a coalescing hint for the scheduler. The authoritative
// state remains in the configured Controller database; consumers only use these node IDs to limit the
// next reconciliation pass.
type ResourceChange struct {
	NodeIDs []string
	// PendingServices asks the reconciliation loop to retry services that do
	// not currently have an assignment. It is intentionally separate from
	// NodeIDs because that pass scans every Agent.
	PendingServices bool
}

type resourceChangeSubscription struct {
	id uint64
	ch chan ResourceChange
}

var nextResourceSubscription atomic.Uint64

func (b *ChangeBus) SubscribeResourceChanges() (<-chan ResourceChange, func()) {
	sub := &resourceChangeSubscription{id: nextResourceSubscription.Add(1), ch: make(chan ResourceChange, 1)}
	b.changeMu.Lock()
	if b.closed.Load() {
		close(sub.ch)
		b.changeMu.Unlock()
		return sub.ch, func() {}
	}
	if b.changeSubs == nil {
		b.changeSubs = make(map[uint64]*resourceChangeSubscription)
	}
	b.changeSubs[sub.id] = sub
	b.changeMu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			b.changeMu.Lock()
			if current := b.changeSubs[sub.id]; current == sub {
				delete(b.changeSubs, sub.id)
				close(current.ch)
			}
			b.changeMu.Unlock()
		})
	}
}

func (b *ChangeBus) notifyResourceChanges(nodeIDs ...string) {
	b.notifyResourceChangesWithOptions(false, nodeIDs...)
}

// notifyPendingServiceChanges marks a change that may make an otherwise
// unassigned service placeable, such as a Gateway becoming available or its
// port pool changing. The scheduler uses this bit to run the broad pending
// service pass; ordinary heartbeats must not trigger that O(N) scan.
func (b *ChangeBus) notifyPendingServiceChanges(nodeIDs ...string) {
	b.notifyResourceChangesWithOptions(true, nodeIDs...)
}

func (b *ChangeBus) notifyResourceChangesWithOptions(pendingServices bool, nodeIDs ...string) {
	if len(nodeIDs) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(nodeIDs))
	ids := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID == "" {
			continue
		}
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		ids = append(ids, nodeID)
	}
	if len(ids) == 0 {
		return
	}
	b.changeMu.Lock()
	defer b.changeMu.Unlock()
	if b.closed.Load() {
		return
	}
	for _, sub := range b.changeSubs {
		change := ResourceChange{
			NodeIDs:         append([]string(nil), ids...),
			PendingServices: pendingServices,
		}
		enqueueResourceChange(sub.ch, change)
	}
}

// enqueueResourceChange replaces the queued hint with its union until the
// merged value is successfully written. The channel consumer does not hold
// changeMu while receiving, so a non-blocking read followed by a non-blocking
// write has a race window where the consumer can remove the old hint and the
// producer can then drop the new one. A select loop keeps the old value in
// hand and retries the write whenever that window occurs.
func enqueueResourceChange(ch chan ResourceChange, change ResourceChange) {
	for {
		select {
		case ch <- change:
			return
		case pending := <-ch:
			change = mergeResourceChanges(pending, change)
		}
	}
}

func mergeResourceChanges(pending, latest ResourceChange) ResourceChange {
	merged := make([]string, 0, len(pending.NodeIDs)+len(latest.NodeIDs))
	seen := make(map[string]struct{}, cap(merged))
	for _, nodeID := range append(append([]string(nil), pending.NodeIDs...), latest.NodeIDs...) {
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		merged = append(merged, nodeID)
	}
	return ResourceChange{
		NodeIDs:         merged,
		PendingServices: pending.PendingServices || latest.PendingServices,
	}
}
