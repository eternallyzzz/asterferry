package controller

import (
	"sync"
	"sync/atomic"
)

// ResourceChange is a coalescing hint for the scheduler. The authoritative
// state remains in SQLite; consumers only use these node IDs to limit the
// next reconciliation pass.
type ResourceChange struct {
	NodeIDs         []string
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

func (s *Store) SubscribeResourceChanges() (<-chan ResourceChange, func()) {
	sub := &resourceChangeSubscription{id: nextResourceSubscription.Add(1), ch: make(chan ResourceChange, 1)}
	s.changeMu.Lock()
	if s.changeSubs == nil {
		s.changeSubs = make(map[uint64]*resourceChangeSubscription)
	}
	s.changeSubs[sub.id] = sub
	s.changeMu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			s.changeMu.Lock()
			if current := s.changeSubs[sub.id]; current == sub {
				delete(s.changeSubs, sub.id)
				close(current.ch)
			}
			s.changeMu.Unlock()
		})
	}
}

func (s *Store) notifyResourceChanges(nodeIDs ...string) {
	s.notifyResourceChangesWithOptions(false, nodeIDs...)
}

// notifyPendingServiceChanges marks a change that may make an otherwise
// unassigned service placeable, such as a Gateway becoming available or its
// port pool changing. The scheduler uses this bit to run the broad pending
// service pass; ordinary heartbeats must not trigger that O(N) scan.
func (s *Store) notifyPendingServiceChanges(nodeIDs ...string) {
	s.notifyResourceChangesWithOptions(true, nodeIDs...)
}

func (s *Store) notifyResourceChangesWithOptions(pendingServices bool, nodeIDs ...string) {
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
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	for _, sub := range s.changeSubs {
		change := ResourceChange{
			NodeIDs:         append([]string(nil), ids...),
			PendingServices: pendingServices,
		}
		select {
		case sub.ch <- change:
		default:
			// Preserve the union of pending IDs so a second write cannot be
			// lost while the scheduler is processing the first hint.
			select {
			case pending := <-sub.ch:
				merged := append(pending.NodeIDs, ids...)
				seen := make(map[string]struct{}, len(merged))
				coalesced := make([]string, 0, len(merged))
				for _, nodeID := range merged {
					if _, ok := seen[nodeID]; ok {
						continue
					}
					seen[nodeID] = struct{}{}
					coalesced = append(coalesced, nodeID)
				}
				select {
				case sub.ch <- ResourceChange{
					NodeIDs:         coalesced,
					PendingServices: pending.PendingServices || pendingServices,
				}:
				default:
				}
			default:
			}
		}
	}
}
