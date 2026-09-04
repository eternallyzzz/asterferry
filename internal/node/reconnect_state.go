package node

import (
	"fmt"
	"time"
)

const maxAgentReconnectBackoff = 30 * time.Second

type agentReconnectPhase uint8

const (
	agentReconnectDisconnected agentReconnectPhase = iota
	agentReconnectDialing
	agentReconnectHandshaking
	agentReconnectAdmitting
	agentReconnectServing
	agentReconnectBackoff
)

func (p agentReconnectPhase) String() string {
	switch p {
	case agentReconnectDisconnected:
		return "disconnected"
	case agentReconnectDialing:
		return "dialing"
	case agentReconnectHandshaking:
		return "handshaking"
	case agentReconnectAdmitting:
		return "admitting"
	case agentReconnectServing:
		return "serving"
	case agentReconnectBackoff:
		return "backoff"
	default:
		return "unknown"
	}
}

type agentReconnectEvent uint8

const (
	agentReconnectStartDial agentReconnectEvent = iota
	agentReconnectDialFailed
	agentReconnectConnected
	agentReconnectHandshakeFailed
	agentReconnectStartAdmission
	agentReconnectAdmissionRejected
	agentReconnectAdmitted
	agentReconnectServingEnded
	agentReconnectCanceled
)

func (e agentReconnectEvent) String() string {
	switch e {
	case agentReconnectStartDial:
		return "start-dial"
	case agentReconnectDialFailed:
		return "dial-failed"
	case agentReconnectConnected:
		return "connected"
	case agentReconnectHandshakeFailed:
		return "handshake-failed"
	case agentReconnectStartAdmission:
		return "admitting"
	case agentReconnectAdmissionRejected:
		return "admission-rejected"
	case agentReconnectAdmitted:
		return "admitted"
	case agentReconnectServingEnded:
		return "serving-ended"
	case agentReconnectCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

type agentReconnectState struct {
	phase   agentReconnectPhase
	backoff time.Duration
}

func newAgentReconnectState() agentReconnectState {
	return agentReconnectState{phase: agentReconnectDisconnected, backoff: time.Second}
}

func (s *agentReconnectState) transition(event agentReconnectEvent) error {
	if s == nil {
		return fmt.Errorf("reconnect state is nil")
	}
	switch event {
	case agentReconnectStartDial:
		if s.phase != agentReconnectDisconnected && s.phase != agentReconnectBackoff {
			return s.invalid(event)
		}
		s.phase = agentReconnectDialing
	case agentReconnectDialFailed:
		if s.phase != agentReconnectDialing {
			return s.invalid(event)
		}
		s.phase = agentReconnectBackoff
	case agentReconnectConnected:
		if s.phase != agentReconnectDialing {
			return s.invalid(event)
		}
		s.phase = agentReconnectHandshaking
	case agentReconnectHandshakeFailed:
		if s.phase != agentReconnectHandshaking {
			return s.invalid(event)
		}
		s.phase = agentReconnectBackoff
	case agentReconnectStartAdmission:
		if s.phase != agentReconnectHandshaking {
			return s.invalid(event)
		}
		s.phase = agentReconnectAdmitting
	case agentReconnectAdmissionRejected:
		if s.phase != agentReconnectAdmitting {
			return s.invalid(event)
		}
		s.phase = agentReconnectBackoff
	case agentReconnectAdmitted:
		if s.phase != agentReconnectAdmitting {
			return s.invalid(event)
		}
		s.phase = agentReconnectServing
		s.backoff = time.Second
	case agentReconnectServingEnded:
		if s.phase != agentReconnectServing {
			return s.invalid(event)
		}
		s.phase = agentReconnectBackoff
	case agentReconnectCanceled:
		s.phase = agentReconnectDisconnected
	default:
		return s.invalid(event)
	}
	return nil
}

func (s *agentReconnectState) invalid(event agentReconnectEvent) error {
	return fmt.Errorf("cannot apply reconnect event %s in phase %s", event, s.phase)
}

// nextWait returns the current delay and advances the exponential backoff for
// the next attempt. It is intentionally deterministic so the reconnect policy
// can be tested without sleeping or using wall-clock time.
func (s *agentReconnectState) nextWait() time.Duration {
	if s == nil || s.backoff <= 0 {
		return time.Second
	}
	wait := s.backoff
	if s.backoff < maxAgentReconnectBackoff {
		s.backoff *= 2
		if s.backoff > maxAgentReconnectBackoff {
			s.backoff = maxAgentReconnectBackoff
		}
	}
	return wait
}
