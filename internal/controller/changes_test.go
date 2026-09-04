package controller

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotChangeNotificationTargetsOnlyAffectedNode(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	gatewayChanges, unsubscribeGateway := store.SubscribeSnapshotChanges("gw")
	defer unsubscribeGateway()
	agentChanges, unsubscribeAgent := store.SubscribeSnapshotChanges("agent")
	defer unsubscribeAgent()

	store.notifySnapshotChanges("gw")
	select {
	case <-gatewayChanges:
	default:
		t.Fatal("affected node did not receive a snapshot change")
	}
	select {
	case <-agentChanges:
		t.Fatal("unaffected node received a snapshot change")
	default:
	}
}

func TestResourceChangeNotificationCoalescesIDsWithoutDroppingWrites(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	changes, unsubscribe := store.SubscribeResourceChanges()
	defer unsubscribe()
	store.notifyResourceChanges("gw", "agent", "gw", "")
	store.notifyResourceChanges("agent", "other")

	change := <-changes
	seen := make(map[string]bool)
	for _, nodeID := range change.NodeIDs {
		seen[nodeID] = true
	}
	if len(seen) != 3 || !seen["gw"] || !seen["agent"] || !seen["other"] {
		t.Fatalf("coalesced resource change IDs = %#v", change.NodeIDs)
	}
}

func TestResourceChangeNotificationPreservesPendingServiceHint(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	changes, unsubscribe := store.SubscribeResourceChanges()
	defer unsubscribe()
	store.notifyResourceChanges("agent")
	store.notifyPendingServiceChanges("gateway")

	change := <-changes
	if !change.PendingServices {
		t.Fatal("coalesced resource change lost pending-service hint")
	}
	if len(change.NodeIDs) != 2 || change.NodeIDs[0] != "agent" || change.NodeIDs[1] != "gateway" {
		t.Fatalf("coalesced resource change IDs = %#v", change.NodeIDs)
	}
}

func TestResourceChangeNotificationDoesNotDropConcurrentWrites(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	changes, unsubscribe := store.SubscribeResourceChanges()
	defer unsubscribe()
	const count = 512
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for index := 0; index < count; index++ {
			store.notifyResourceChanges(fmt.Sprintf("node-%d", index))
		}
	}()

	seen := make(map[string]struct{}, count)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(seen) < count {
		select {
		case change, ok := <-changes:
			if !ok {
				t.Fatal("resource change subscription closed before all writes were delivered")
			}
			for _, nodeID := range change.NodeIDs {
				seen[nodeID] = struct{}{}
			}
		case <-deadline.C:
			t.Fatalf("resource change notification lost writes: received %d of %d node IDs", len(seen), count)
		}
	}
	<-producerDone
}
