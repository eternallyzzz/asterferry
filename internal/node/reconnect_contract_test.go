package node

import (
	"testing"
	"time"
)

func TestAgentReconnectBackoffOnlyResetsAfterAdmittedSessionContract(t *testing.T) {
	state := newAgentReconnectState()
	state.backoff = maxAgentReconnectBackoff
	for _, event := range []agentReconnectEvent{
		agentReconnectStartDial,
		agentReconnectConnected,
		agentReconnectStartAdmission,
		agentReconnectAdmissionRejected,
	} {
		if err := state.transition(event); err != nil {
			t.Fatal(err)
		}
	}
	if got := state.nextWait(); got != maxAgentReconnectBackoff || state.backoff != maxAgentReconnectBackoff {
		t.Fatalf("admission rejection changed backoff to wait=%s next=%s", got, state.backoff)
	}
	for _, event := range []agentReconnectEvent{
		agentReconnectStartDial,
		agentReconnectConnected,
		agentReconnectStartAdmission,
		agentReconnectAdmitted,
	} {
		if err := state.transition(event); err != nil {
			t.Fatal(err)
		}
	}
	if got := state.nextWait(); got != time.Second || state.backoff != 2*time.Second {
		t.Fatalf("admitted session backoff = wait:%s next:%s, want wait:1s next:2s", got, state.backoff)
	}
}
