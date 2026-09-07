package controller

import (
	"context"
	"path/filepath"
	"testing"
)

func TestApplyNodeUpgradeEventPersistsTerminalState(t *testing.T) {
	dir := t.TempDir()
	repositories, err := openTestRepositories(filepath.Join(dir, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	server, err := NewControlServer(DefaultConfig(dir), repositories)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx := context.Background()
	if err := server.applyNodeUpgradeEvent(ctx, "event-node", map[string]string{
		"state": "applying", "action_id": "event-action", "version": "1.1.0", "current_version": "1.0.0", "deployment": "systemd",
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.applyNodeUpgradeEvent(ctx, "event-node", map[string]string{
		"state": "up_to_date", "action_id": "event-action", "version": "1.1.0", "current_version": "1.1.0", "deployment": "systemd",
	}); err != nil {
		t.Fatal(err)
	}
	state, err := repositories.Resources.GetNodeUpdateState(ctx, "event-node")
	if err != nil {
		t.Fatal(err)
	}
	if state.State != UpdateStateUpToDate || state.CurrentVersion != "1.1.0" || state.LastError != "" || !state.Supported {
		t.Fatalf("persisted Node event state = %#v", state)
	}
	if err := server.applyNodeUpgradeEvent(ctx, "event-node", map[string]string{"state": "bogus"}); err == nil {
		t.Fatal("invalid Node upgrade event state was accepted")
	}
}

func TestNodeSupportsSelfUpdateRequiresNativeModeAndCapability(t *testing.T) {
	if supported, _ := nodeSupportsSelfUpdate("container", []string{"node-upgrade-v1"}); supported {
		t.Fatal("container Node was marked self-upgrade capable")
	}
	if supported, _ := nodeSupportsSelfUpdate("systemd", []string{"runtime-control-v1"}); supported {
		t.Fatal("Node without upgrade capability was marked capable")
	}
	if supported, reason := nodeSupportsSelfUpdate("systemd", []string{"node-upgrade-v1"}); !supported || reason != "" {
		t.Fatalf("systemd Node capability result = %v, %q", supported, reason)
	}
}

func TestSaveNodeUpdateStateIfActivePreservesTerminalResult(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	pending := NodeUpdateState{NodeID: "upgrade-state-node", ActionID: "upgrade-action", CurrentVersion: "1.0.0", TargetVersion: "1.1.0", State: UpdateStateApplying, Supported: true, Deployment: "systemd"}
	if saved, wrote, err := store.SaveNodeUpdateStateIfActive(ctx, pending); err != nil || !wrote || saved.State != UpdateStateApplying {
		t.Fatalf("initial update state save = %#v, wrote=%v, err=%v", saved, wrote, err)
	}
	terminal := pending
	terminal.CurrentVersion = "1.1.0"
	terminal.State = UpdateStateUpToDate
	if err := store.SaveNodeUpdateState(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	saved, wrote, err := store.SaveNodeUpdateStateIfActive(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("request-side applying state overwrote a terminal Node result")
	}
	if saved.State != UpdateStateUpToDate || saved.CurrentVersion != "1.1.0" {
		t.Fatalf("preserved terminal state = %#v", saved)
	}
}

func TestChangeBusCapabilityRegistrationDoesNotClearReconnect(t *testing.T) {
	bus := newChangeBus()
	defer bus.Close()
	first := bus.SetNodeCapabilities("node", []string{"old"})
	second := bus.SetNodeCapabilities("node", []string{"node-upgrade-v1"})
	bus.ClearNodeCapabilities("node", first)
	capabilities := bus.NodeCapabilities("node")
	if len(capabilities) != 1 || capabilities[0] != "node-upgrade-v1" {
		t.Fatalf("reconnecting Node capabilities = %#v", capabilities)
	}
	bus.ClearNodeCapabilities("node", second)
	if capabilities := bus.NodeCapabilities("node"); len(capabilities) != 0 {
		t.Fatalf("cleared Node capabilities = %#v", capabilities)
	}
}
