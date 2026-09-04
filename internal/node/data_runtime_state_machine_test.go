package node

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"asterferry/internal/afdp"
	"asterferry/internal/domain"
)

type udpModelFlow struct {
	id  uint64
	key string
}

type udpFlowModel struct {
	closed  bool
	active  map[uint64]*udpModelFlow
	byKey   map[string]*udpModelFlow
	removed map[uint64]int
}

func newUDPFlowModel() *udpFlowModel {
	return &udpFlowModel{active: make(map[uint64]*udpModelFlow), byKey: make(map[string]*udpModelFlow), removed: make(map[uint64]int)}
}

func (m *udpFlowModel) add(flow *udpModelFlow) bool {
	if m.closed || flow == nil || m.active[flow.id] != nil || m.byKey[flow.key] != nil || len(m.active) >= dataPlaneFlowLimit {
		return false
	}
	m.active[flow.id] = flow
	m.byKey[flow.key] = flow
	return true
}

func (m *udpFlowModel) remove(flow *udpModelFlow) {
	if flow == nil || m.active[flow.id] != flow {
		return
	}
	delete(m.active, flow.id)
	if m.byKey[flow.key] == flow {
		delete(m.byKey, flow.key)
	}
	m.removed[flow.id]++
}

func (m *udpFlowModel) removeAll() {
	for _, flow := range m.active {
		m.remove(flow)
	}
}

type udpFlowRecord struct {
	model   *udpModelFlow
	actual  *dataUDPFlow
	runtime *runtimeConnection
	added   bool
}

func newUDPFlowRecord(seed int64, id uint64, key string) *udpFlowRecord {
	runtime := &runtimeConnection{
		registry: newRuntimeTelemetry(),
		meta:     domain.RuntimeConnection{ID: fmt.Sprintf("state-machine-%d-%d", seed, id)},
	}
	return &udpFlowRecord{
		model:   &udpModelFlow{id: id, key: key},
		actual:  &dataUDPFlow{id: id, key: key, runtime: runtime},
		runtime: runtime,
	}
}

func TestDataGenerationUDPFlowStateMachineContract(t *testing.T) {
	for seedOffset := int64(0); seedOffset < 256; seedOffset++ {
		seed := int64(0xA57EF2) + seedOffset
		random := rand.New(rand.NewSource(seed))
		generation := newTestDataGeneration()
		model := newUDPFlowModel()
		records := make([]*udpFlowRecord, 0, 64)
		trace := make([]string, 0, 64)

		check := func() {
			generation.udpMu.Lock()
			actualCount := len(generation.udpFlows)
			generation.udpMu.Unlock()
			if actualCount != len(model.active) {
				t.Fatalf("seed=%d trace=%s active flow count=%d, want=%d", seed, strings.Join(trace, ","), actualCount, len(model.active))
			}
			for id, expected := range model.active {
				actual := generation.udpFlow(id)
				if actual == nil || actual.id != expected.id || actual.key != expected.key {
					t.Fatalf("seed=%d trace=%s flow %d = %#v, want=%#v", seed, strings.Join(trace, ","), id, actual, expected)
				}
				if generation.udpFlowByKey(expected.key) != actual {
					t.Fatalf("seed=%d trace=%s key index %q does not point to active flow", seed, strings.Join(trace, ","), expected.key)
				}
			}
			for _, record := range records {
				record.runtime.mu.Lock()
				closed := record.runtime.closed
				record.runtime.mu.Unlock()
				wantClosed := model.active[record.model.id] != record.model
				if closed != (record.added && wantClosed) {
					t.Fatalf("seed=%d trace=%s flow id=%d runtime closed=%v, want=%v", seed, strings.Join(trace, ","), record.model.id, closed, wantClosed)
				}
			}
		}

		for step := 0; step < 64; step++ {
			kind := random.Intn(6)
			switch kind {
			case 0, 1:
				id := uint64(1 + random.Intn(8))
				key := fmt.Sprintf("flow-key-%d", 1+random.Intn(6))
				record := newUDPFlowRecord(seed, id, key)
				records = append(records, record)
				_, added := generation.addUDPFlow(record.actual)
				modelAdded := model.add(record.model)
				trace = append(trace, fmt.Sprintf("add(%d,%s)", id, key))
				record.added = added
				if added != modelAdded {
					t.Fatalf("seed=%d trace=%s add=%v, want=%v", seed, strings.Join(trace, ","), added, modelAdded)
				}
			case 2, 3:
				if len(records) == 0 {
					continue
				}
				record := records[random.Intn(len(records))]
				trace = append(trace, fmt.Sprintf("remove(%d)", record.actual.id))
				generation.removeUDPFlow(record.actual, domain.RuntimeClosePeer)
				model.remove(record.model)
			case 4:
				trace = append(trace, "remove-all")
				generation.removeAllUDPFlows(domain.RuntimeCloseSession)
				model.removeAll()
			case 5:
				if random.Intn(3) == 0 {
					trace = append(trace, "generation-close")
					generation.close()
					model.closed = true
					model.removeAll()
					check()
					continue
				}
				trace = append(trace, "expire")
				now := time.Unix(1000+int64(step), 0).UTC()
				for _, record := range records {
					if model.active[record.model.id] == record.model {
						record.actual.lastUnixNano.Store(now.Add(-dataPlaneFlowTTL - time.Second).UnixNano())
					}
				}
				generation.expireUDPFlows(nil, now)
				model.removeAll()
			}
			check()
		}
		generation.cancel()
	}
}

func TestDataGenerationSessionReplacementIsIdentityGatedContract(t *testing.T) {
	generation := newTestDataGeneration()
	defer generation.cancel()
	oldSession := &afdp.Session{}
	newSession := &afdp.Session{}
	generation.setAgentSessionWithRuntimeID("assignment", oldSession, "old-runtime")
	generation.setAgentSessionWithRuntimeID("assignment", newSession, "new-runtime")
	generation.clearAgentSession("assignment", oldSession)
	if got := generation.agentSession("assignment"); got != newSession {
		t.Fatalf("late old-session cleanup replaced active session: got=%p want=%p", got, newSession)
	}
	generation.clearAgentSession("assignment", newSession)
	if got := generation.agentSession("assignment"); got != nil {
		t.Fatalf("active session cleanup left session=%p", got)
	}
}
