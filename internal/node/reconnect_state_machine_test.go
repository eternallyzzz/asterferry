package node

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

const reconnectStateMachineSeed int64 = 0xA57EF2

type reconnectModel struct {
	phase   string
	backoff time.Duration
}

func newReconnectModel() reconnectModel {
	return reconnectModel{phase: "disconnected", backoff: time.Second}
}

func (m *reconnectModel) transition(event string) error {
	switch event {
	case "start-dial":
		if m.phase != "disconnected" && m.phase != "backoff" {
			return fmt.Errorf("invalid start from %s", m.phase)
		}
		m.phase = "dialing"
	case "dial-failed":
		if m.phase != "dialing" {
			return fmt.Errorf("invalid dial failure from %s", m.phase)
		}
		m.phase = "backoff"
	case "connected":
		if m.phase != "dialing" {
			return fmt.Errorf("invalid connection from %s", m.phase)
		}
		m.phase = "handshaking"
	case "handshake-failed":
		if m.phase != "handshaking" {
			return fmt.Errorf("invalid handshake failure from %s", m.phase)
		}
		m.phase = "backoff"
	case "admitting":
		if m.phase != "handshaking" {
			return fmt.Errorf("invalid admission start from %s", m.phase)
		}
		m.phase = "admitting"
	case "admission-rejected":
		if m.phase != "admitting" {
			return fmt.Errorf("invalid admission rejection from %s", m.phase)
		}
		m.phase = "backoff"
	case "admitted":
		if m.phase != "admitting" {
			return fmt.Errorf("invalid admission success from %s", m.phase)
		}
		m.phase = "serving"
		m.backoff = time.Second
	case "serving-ended":
		if m.phase != "serving" {
			return fmt.Errorf("invalid serving end from %s", m.phase)
		}
		m.phase = "backoff"
	case "canceled":
		m.phase = "disconnected"
	default:
		return fmt.Errorf("unknown event %q", event)
	}
	return nil
}

func (m *reconnectModel) nextWait() (time.Duration, error) {
	if m.phase != "backoff" {
		return 0, fmt.Errorf("invalid wait from %s", m.phase)
	}
	wait := m.backoff
	if m.backoff < maxAgentReconnectBackoff {
		m.backoff *= 2
		if m.backoff > maxAgentReconnectBackoff {
			m.backoff = maxAgentReconnectBackoff
		}
	}
	return wait, nil
}

func reconnectEventForModel(model reconnectModel, random *rand.Rand) string {
	switch model.phase {
	case "disconnected", "backoff":
		if model.phase == "backoff" && random.Intn(4) == 0 {
			return "wait"
		}
		return "start-dial"
	case "dialing":
		if random.Intn(4) == 0 {
			return "dial-failed"
		}
		return "connected"
	case "handshaking":
		if random.Intn(4) == 0 {
			return "handshake-failed"
		}
		return "admitting"
	case "admitting":
		if random.Intn(4) == 0 {
			return "admission-rejected"
		}
		return "admitted"
	case "serving":
		return "serving-ended"
	default:
		return "canceled"
	}
}

func reconnectEventValue(event string) (agentReconnectEvent, bool) {
	values := map[string]agentReconnectEvent{
		"start-dial":         agentReconnectStartDial,
		"dial-failed":        agentReconnectDialFailed,
		"connected":          agentReconnectConnected,
		"handshake-failed":   agentReconnectHandshakeFailed,
		"admitting":          agentReconnectStartAdmission,
		"admission-rejected": agentReconnectAdmissionRejected,
		"admitted":           agentReconnectAdmitted,
		"serving-ended":      agentReconnectServingEnded,
		"canceled":           agentReconnectCanceled,
	}
	value, ok := values[event]
	return value, ok
}

func TestAgentReconnectStateMachineContract(t *testing.T) {
	for seedOffset := int64(0); seedOffset < 256; seedOffset++ {
		seed := reconnectStateMachineSeed + seedOffset
		random := rand.New(rand.NewSource(seed))
		model := newReconnectModel()
		state := newAgentReconnectState()
		trace := make([]string, 0, 64)

		for step := 0; step < 64; step++ {
			event := reconnectEventForModel(model, random)
			trace = append(trace, event)
			if event == "wait" {
				want, wantErr := model.nextWait()
				got := state.nextWait()
				if wantErr != nil || got != want {
					t.Fatalf("seed=%d step=%d trace=%s next wait=%s, want=%s (model err=%v)", seed, step, strings.Join(trace, ","), got, want, wantErr)
				}
			} else {
				wantErr := model.transition(event)
				value, ok := reconnectEventValue(event)
				if !ok {
					t.Fatalf("seed=%d step=%d unknown test event %q", seed, step, event)
				}
				gotErr := state.transition(value)
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("seed=%d step=%d trace=%s transition error=%v, want=%v", seed, step, strings.Join(trace, ","), gotErr, wantErr)
				}
			}
			if state.phase.String() != model.phase || state.backoff != model.backoff {
				t.Fatalf("seed=%d step=%d trace=%s state=(%s,%s), want=(%s,%s)", seed, step, strings.Join(trace, ","), state.phase, state.backoff, model.phase, model.backoff)
			}
		}
	}
}

func TestAgentReconnectStateMachineRejectsInvalidTransitionsContract(t *testing.T) {
	state := newAgentReconnectState()
	if err := state.transition(agentReconnectServingEnded); err == nil {
		t.Fatal("serving-ended was accepted before a session was admitted")
	}
	if state.phase != agentReconnectDisconnected || state.backoff != time.Second {
		t.Fatalf("invalid transition mutated state: %#v", state)
	}
}
