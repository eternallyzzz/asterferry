package observability

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultEventBufferSize = 512
	maxEventSubscribers    = 32
	eventSubscriberBuffer  = 64
	maxEventAttributeBytes = 128
)

// Event is the deliberately reduced representation exposed to Dashboard
// clients. It contains event identity and safe correlation fields, never raw
// log messages, errors, credentials, paths, or proxy destinations.
type Event struct {
	ID            uint64            `json:"id"`
	Time          time.Time         `json:"time"`
	Level         string            `json:"level"`
	Event         string            `json:"event"`
	Role          string            `json:"role,omitempty"`
	SecurityAudit bool              `json:"security_audit,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

type EventHub struct {
	mu          sync.Mutex
	capacity    int
	nextID      uint64
	events      []Event
	subscribers map[uint64]*eventSubscriber
	nextSubID   uint64
}

type eventSubscriber struct {
	ch chan Event
}

type EventStream struct {
	Replay []Event
	Events <-chan Event
	close  func()
	once   sync.Once
}

func NewEventHub(capacity int) *EventHub {
	if capacity <= 0 {
		capacity = defaultEventBufferSize
	}
	return &EventHub{
		capacity:    capacity,
		events:      make([]Event, 0, capacity),
		subscribers: make(map[uint64]*eventSubscriber),
	}
}

func (h *EventHub) Publish(record slog.Record, base []slog.Attr) {
	if h == nil {
		return
	}
	event := eventFromRecord(record, base)
	h.mu.Lock()
	h.nextID++
	event.ID = h.nextID
	h.events = append(h.events, event)
	if len(h.events) > h.capacity {
		h.events = h.events[len(h.events)-h.capacity:]
	}
	for _, subscriber := range h.subscribers {
		select {
		case subscriber.ch <- event:
		default:
			// The SSE writer detects the resulting ID gap and emits a compact
			// gap notification. Logging must never block on a browser.
		}
	}
	h.mu.Unlock()
}

func (h *EventHub) Open(lastID uint64) (*EventStream, error) {
	if h == nil {
		return nil, errors.New("event hub is unavailable")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers) >= maxEventSubscribers {
		return nil, errors.New("event stream subscriber limit reached")
	}
	replay := append([]Event(nil), h.events...)
	if lastID > 0 {
		start := 0
		for start < len(replay) && replay[start].ID <= lastID {
			start++
		}
		replay = replay[start:]
	} else if len(replay) > 100 {
		replay = replay[len(replay)-100:]
	}
	h.nextSubID++
	id := h.nextSubID
	subscriber := &eventSubscriber{ch: make(chan Event, eventSubscriberBuffer)}
	h.subscribers[id] = subscriber
	stream := &EventStream{Replay: replay, Events: subscriber.ch}
	stream.close = func() {
		stream.once.Do(func() {
			h.mu.Lock()
			if current, ok := h.subscribers[id]; ok && current == subscriber {
				delete(h.subscribers, id)
				close(subscriber.ch)
			}
			h.mu.Unlock()
		})
	}
	return stream, nil
}

func (s *EventStream) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

func eventFromRecord(record slog.Record, base []slog.Attr) Event {
	event := Event{
		Time:  record.Time.UTC(),
		Level: strings.ToLower(record.Level.String()),
		Event: "log.uncategorized",
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	read := func(attr slog.Attr) bool {
		if attr.Key == "" {
			return true
		}
		if attr.Key == "event" && attr.Value.Kind() == slog.KindString {
			value := safeEventValue(attr.Value.String())
			if value != "" {
				event.Event = value
			}
			return true
		}
		if attr.Key == "role" && attr.Value.Kind() == slog.KindString {
			event.Role = safeEventValue(attr.Value.String())
			return true
		}
		if attr.Key == "security_audit" && attr.Value.Kind() == slog.KindBool {
			event.SecurityAudit = attr.Value.Bool()
			return true
		}
		if !safeEventAttribute(attr.Key) {
			return true
		}
		value := eventValue(attr.Value)
		if value != "" {
			if event.Attributes == nil {
				event.Attributes = make(map[string]string)
			}
			event.Attributes[attr.Key] = value
		}
		return true
	}
	for _, attr := range base {
		read(attr)
	}
	record.Attrs(read)
	return event
}

func safeEventAttribute(key string) bool {
	switch key {
	case "agent_id", "session_id", "node_id", "mapping", "inbound", "error_kind", "protocol", "transport_obfuscation", "network", "action":
		return true
	default:
		return false
	}
}

func eventValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return safeEventValue(value.String())
	case slog.KindInt64:
		return safeEventValue(strconv.FormatInt(value.Int64(), 10))
	case slog.KindUint64:
		return safeEventValue(strconv.FormatUint(value.Uint64(), 10))
	case slog.KindFloat64:
		return safeEventValue(strconv.FormatFloat(value.Float64(), 'f', -1, 64))
	case slog.KindBool:
		if value.Bool() {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func safeEventValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > maxEventAttributeBytes {
		value = value[:maxEventAttributeBytes]
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}
