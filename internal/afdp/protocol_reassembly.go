package afdp

import (
	"container/heap"
	"errors"
	"fmt"
	"sync"
	"time"
)

type fragmentSet struct {
	created time.Time
	count   uint16
	parts   map[uint16][]byte
	bytes   int
}

// Reassembler bounds both the number of concurrent flows and their total
// memory. Expired and over-budget fragments are discarded fail-closed.
type Reassembler struct {
	mu             sync.Mutex
	flows          map[flowKey]*fragmentSet
	completed      map[flowKey]time.Time
	completedOrder completedHeap
	maxFlows       int
	maxBytes       int
	maxPayload     int
	timeout        time.Duration
	bytes          int
}

type flowKey struct {
	flowID   uint64
	sequence uint32
}

type completedEntry struct {
	key flowKey
	at  time.Time
}

type completedHeap []completedEntry

func (h completedHeap) Len() int { return len(h) }

func (h completedHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }

func (h completedHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *completedHeap) Push(value any) { *h = append(*h, value.(completedEntry)) }

func (h *completedHeap) Pop() any {
	old := *h
	n := len(old)
	value := old[n-1]
	*h = old[:n-1]
	return value
}

func NewReassembler(maxFlows, maxBytes, maxPayload int, timeout time.Duration) (*Reassembler, error) {
	if maxFlows <= 0 || maxBytes <= 0 || maxPayload <= 0 || timeout <= 0 {
		return nil, errors.New("reassembler limits must be positive")
	}
	if maxFlows > 1<<20 || maxBytes > 64<<20 || maxPayload > maxDatagramPayload || timeout > 24*time.Hour {
		return nil, errors.New("reassembler limits exceed the supported maximum")
	}
	return &Reassembler{flows: make(map[flowKey]*fragmentSet), completed: make(map[flowKey]time.Time), maxFlows: maxFlows, maxBytes: maxBytes, maxPayload: maxPayload, timeout: timeout}, nil
}

// Add returns a complete payload only when all fragments have arrived.
func (r *Reassembler) Add(data []byte, now time.Time) ([]byte, bool, error) {
	header, payload, err := DecodeDatagram(data, r.maxPayload)
	if err != nil {
		return nil, false, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	key := flowKey{flowID: header.FlowID, sequence: header.Sequence}
	if _, duplicate := r.completed[key]; duplicate {
		return nil, false, ErrMalformedFrame
	}
	set := r.flows[key]
	if set == nil {
		if len(r.flows) >= r.maxFlows {
			return nil, false, fmt.Errorf("%w: reassembler flow limit reached", ErrTransient)
		}
		set = &fragmentSet{created: now, count: header.FragmentCount, parts: make(map[uint16][]byte)}
		r.flows[key] = set
	}
	if set.count != header.FragmentCount {
		delete(r.flows, key)
		r.bytes -= set.bytes
		return nil, false, ErrMalformedFrame
	}
	if _, exists := set.parts[header.FragmentIndex]; exists {
		delete(r.flows, key)
		r.bytes -= set.bytes
		return nil, false, ErrMalformedFrame
	}
	if r.bytes+len(payload) > r.maxBytes {
		delete(r.flows, key)
		r.bytes -= set.bytes
		return nil, false, fmt.Errorf("%w: reassembler byte limit reached", ErrTransient)
	}
	set.parts[header.FragmentIndex] = payload
	set.bytes += len(payload)
	r.bytes += len(payload)
	if len(set.parts) != int(set.count) {
		return nil, false, nil
	}
	result := make([]byte, 0, set.bytes)
	for index := uint16(0); index < set.count; index++ {
		part, ok := set.parts[index]
		if !ok {
			return nil, false, nil
		}
		result = append(result, part...)
	}
	delete(r.flows, key)
	r.bytes -= set.bytes
	// A completed flow/sequence is remembered for the reassembly TTL so a
	// replayed datagram cannot be interpreted as a fresh payload. This map is
	// bounded alongside the in-flight flow map and is expired under the same
	// lock.
	for len(r.completed) >= r.maxFlows*2 && r.completedOrder.Len() > 0 {
		entry := heap.Pop(&r.completedOrder).(completedEntry)
		if completedAt, ok := r.completed[entry.key]; ok && completedAt.Equal(entry.at) {
			delete(r.completed, entry.key)
			break
		}
	}
	r.completed[key] = now
	heap.Push(&r.completedOrder, completedEntry{key: key, at: now})
	return result, true, nil
}

func (r *Reassembler) expireLocked(now time.Time) {
	for key, set := range r.flows {
		if now.Sub(set.created) >= r.timeout {
			delete(r.flows, key)
			r.bytes -= set.bytes
		}
	}
	for r.completedOrder.Len() > 0 {
		entry := r.completedOrder[0]
		if now.Sub(entry.at) < r.timeout {
			break
		}
		heap.Pop(&r.completedOrder)
		if completedAt, ok := r.completed[entry.key]; ok && completedAt.Equal(entry.at) {
			delete(r.completed, entry.key)
		}
	}
}

func (r *Reassembler) Expire(now time.Time) {
	r.mu.Lock()
	r.expireLocked(now)
	r.mu.Unlock()
}

func (r *Reassembler) InFlight() (flows, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.flows), r.bytes
}

// Fragments splits one payload into bounded datagrams. A zero-length payload
// is represented by one empty fragment so FIN remains explicit.
func Fragments(flowID uint64, sequence uint32, payload []byte, mtu int) ([][]byte, error) {
	if flowID == 0 {
		return nil, ErrMalformedFrame
	}
	if mtu <= datagramHeaderBytes {
		return nil, errors.New("datagram MTU is too small")
	}
	partSize := mtu - datagramHeaderBytes
	count := (len(payload) + partSize - 1) / partSize
	if count == 0 {
		count = 1
	}
	if count > maxDatagramFragments {
		return nil, ErrFrameTooLarge
	}
	result := make([][]byte, 0, count)
	for index := 0; index < count; index++ {
		start := index * partSize
		end := start + partSize
		if end > len(payload) {
			end = len(payload)
		}
		flags := byte(0)
		if count > 1 {
			flags |= DatagramFlagFragmented
		}
		if index == count-1 {
			flags |= DatagramFlagFin
		}
		frame, err := EncodeDatagram(DatagramHeader{Flags: flags, FlowID: flowID, Sequence: sequence, FragmentIndex: uint16(index), FragmentCount: uint16(count)}, payload[start:end], partSize)
		if err != nil {
			return nil, err
		}
		result = append(result, frame)
	}
	return result, nil
}
