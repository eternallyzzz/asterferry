package node

import (
	"context"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func newTestDataGeneration() *dataGeneration {
	ctx, cancel := context.WithCancel(context.Background())
	return &dataGeneration{
		ctx:       ctx,
		cancel:    cancel,
		udpFlows:  make(map[uint64]*dataUDPFlow),
		udpByKey:  make(map[string]*dataUDPFlow),
		telemetry: newRuntimeTelemetry(),
	}
}

func TestDataGenerationUDPFlowCleanupIsIdentityGated(t *testing.T) {
	generation := newTestDataGeneration()
	t.Cleanup(generation.cancel)

	first := &dataUDPFlow{id: 1, key: "assignment|service|remote"}
	if got, added := generation.addUDPFlow(first); !added || got != first {
		t.Fatalf("first UDP flow add = flow:%p added:%v", got, added)
	}
	generation.removeUDPFlow(first, domain.RuntimeClosePeer)
	if generation.udpFlow(first.id) != nil || generation.udpFlowByKey(first.key) != nil {
		t.Fatal("removed UDP flow remained in one of the indexes")
	}

	replacement := &dataUDPFlow{id: first.id, key: first.key}
	if _, added := generation.addUDPFlow(replacement); !added {
		t.Fatal("replacement UDP flow was not admitted")
	}
	generation.removeUDPFlow(first, domain.RuntimeClosePeer)
	if generation.udpFlow(first.id) != replacement || generation.udpFlowByKey(first.key) != replacement {
		t.Fatal("late cleanup of the old flow removed the replacement")
	}

	generation.removeUDPFlow(replacement, domain.RuntimeClosePeer)
	generation.removeUDPFlow(replacement, domain.RuntimeClosePeer)
	if generation.udpFlow(replacement.id) != nil || generation.udpFlowByKey(replacement.key) != nil {
		t.Fatal("repeated UDP flow cleanup was not idempotent")
	}

	generation.closed.Store(true)
	if _, added := generation.addUDPFlow(&dataUDPFlow{id: 2, key: "closed"}); added {
		t.Fatal("closed generation admitted a UDP flow")
	}
}

func TestDataGenerationUDPFlowExpirationUsesCentralCleanup(t *testing.T) {
	generation := newTestDataGeneration()
	t.Cleanup(generation.cancel)
	now := time.Now().UTC()
	flow := &dataUDPFlow{id: 7, key: "expired"}
	flow.lastUnixNano.Store(now.Add(-dataPlaneFlowTTL - time.Second).UnixNano())
	if _, added := generation.addUDPFlow(flow); !added {
		t.Fatal("expired UDP flow was not admitted")
	}

	generation.expireUDPFlows(nil, now)
	if generation.udpFlow(flow.id) != nil || generation.udpFlowByKey(flow.key) != nil {
		t.Fatal("expired UDP flow remained indexed")
	}
}

func TestDataGenerationCloseUsesCentralUDPFlowCleanup(t *testing.T) {
	generation := newTestDataGeneration()
	flow := &dataUDPFlow{id: 9, key: "generation-close"}
	if _, added := generation.addUDPFlow(flow); !added {
		t.Fatal("UDP flow was not admitted")
	}

	generation.close()
	if generation.udpFlow(flow.id) != nil || generation.udpFlowByKey(flow.key) != nil {
		t.Fatal("generation close left a UDP flow indexed")
	}
	generation.close()
}
