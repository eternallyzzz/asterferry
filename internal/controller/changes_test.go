package controller

import (
	"path/filepath"
	"testing"
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
