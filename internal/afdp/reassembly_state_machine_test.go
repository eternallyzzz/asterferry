package afdp

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

type reassemblyModelKey struct {
	flowID   uint64
	sequence uint32
}

type reassemblyModelFlow struct {
	created time.Time
	count   uint16
	parts   map[uint16][]byte
	bytes   int
}

type reassemblyModelCompleted struct {
	key reassemblyModelKey
	at  time.Time
}

type reassemblyModel struct {
	flows          map[reassemblyModelKey]*reassemblyModelFlow
	completed      map[reassemblyModelKey]time.Time
	completedOrder []reassemblyModelCompleted
	maxFlows       int
	maxBytes       int
	maxPayload     int
	timeout        time.Duration
	bytes          int
}

func newReassemblyModel(maxFlows, maxBytes, maxPayload int, timeout time.Duration) *reassemblyModel {
	return &reassemblyModel{
		flows:      make(map[reassemblyModelKey]*reassemblyModelFlow),
		completed:  make(map[reassemblyModelKey]time.Time),
		maxFlows:   maxFlows,
		maxBytes:   maxBytes,
		maxPayload: maxPayload,
		timeout:    timeout,
	}
}

func (m *reassemblyModel) expire(now time.Time) {
	for key, flow := range m.flows {
		if now.Sub(flow.created) >= m.timeout {
			delete(m.flows, key)
			m.bytes -= flow.bytes
		}
	}
	for len(m.completedOrder) > 0 && now.Sub(m.completedOrder[0].at) >= m.timeout {
		entry := m.completedOrder[0]
		m.completedOrder = m.completedOrder[1:]
		if completedAt, ok := m.completed[entry.key]; ok && completedAt.Equal(entry.at) {
			delete(m.completed, entry.key)
		}
	}
}

func (m *reassemblyModel) add(header DatagramHeader, payload []byte, now time.Time) ([]byte, bool, error) {
	m.expire(now)
	key := reassemblyModelKey{flowID: header.FlowID, sequence: header.Sequence}
	if _, ok := m.completed[key]; ok {
		return nil, false, ErrMalformedFrame
	}
	flow := m.flows[key]
	if flow == nil {
		if len(m.flows) >= m.maxFlows {
			return nil, false, fmt.Errorf("%w: reassembler flow limit reached", ErrTransient)
		}
		flow = &reassemblyModelFlow{created: now, count: header.FragmentCount, parts: make(map[uint16][]byte)}
		m.flows[key] = flow
	}
	if flow.count != header.FragmentCount {
		delete(m.flows, key)
		m.bytes -= flow.bytes
		return nil, false, ErrMalformedFrame
	}
	if _, ok := flow.parts[header.FragmentIndex]; ok {
		delete(m.flows, key)
		m.bytes -= flow.bytes
		return nil, false, ErrMalformedFrame
	}
	if len(payload) > m.maxPayload || m.bytes+len(payload) > m.maxBytes {
		delete(m.flows, key)
		m.bytes -= flow.bytes
		return nil, false, fmt.Errorf("%w: reassembler byte limit reached", ErrTransient)
	}
	flow.parts[header.FragmentIndex] = append([]byte(nil), payload...)
	flow.bytes += len(payload)
	m.bytes += len(payload)
	if len(flow.parts) != int(flow.count) {
		return nil, false, nil
	}
	result := make([]byte, 0, flow.bytes)
	for index := uint16(0); index < flow.count; index++ {
		part, ok := flow.parts[index]
		if !ok {
			return nil, false, nil
		}
		result = append(result, part...)
	}
	delete(m.flows, key)
	m.bytes -= flow.bytes
	for len(m.completed) >= m.maxFlows*2 && len(m.completedOrder) > 0 {
		entry := m.completedOrder[0]
		m.completedOrder = m.completedOrder[1:]
		if completedAt, ok := m.completed[entry.key]; ok && completedAt.Equal(entry.at) {
			delete(m.completed, entry.key)
			break
		}
	}
	m.completed[key] = now
	m.completedOrder = append(m.completedOrder, reassemblyModelCompleted{key: key, at: now})
	return result, true, nil
}

type reassemblyCommand struct {
	label   string
	header  DatagramHeader
	payload []byte
}

func reassemblyFragmentHeader(flowID uint64, sequence uint32, index, count uint16) DatagramHeader {
	flags := byte(0)
	if count > 1 {
		flags |= DatagramFlagFragmented
	}
	if index == count-1 {
		flags |= DatagramFlagFin
	}
	return DatagramHeader{Flags: flags, FlowID: flowID, Sequence: sequence, FragmentIndex: index, FragmentCount: count}
}

func makeReassemblyTemplate(flowID uint64, sequence uint32, count uint16, seed int64) []reassemblyCommand {
	result := make([]reassemblyCommand, 0, count)
	for index := uint16(0); index < count; index++ {
		result = append(result, reassemblyCommand{
			label:   fmt.Sprintf("fragment-%d-%d-%d", flowID, sequence, index),
			header:  reassemblyFragmentHeader(flowID, sequence, index, count),
			payload: []byte(fmt.Sprintf("payload-%d-%d-%d-%d", flowID, sequence, index, seed)),
		})
	}
	return result
}

func reassemblyErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrTransient):
		return "transient"
	case errors.Is(err, ErrFrameTooLarge):
		return "too-large"
	case errors.Is(err, ErrMalformedFrame):
		return "malformed"
	default:
		return err.Error()
	}
}

func TestReassemblerStateMachineContract(t *testing.T) {
	const (
		maxFlows   = 8
		maxBytes   = 2048
		maxPayload = 128
		timeout    = 5 * time.Second
	)
	for seedOffset := int64(0); seedOffset < 256; seedOffset++ {
		seed := int64(0xA57EF2) + seedOffset
		random := rand.New(rand.NewSource(seed))
		sut, err := NewReassembler(maxFlows, maxBytes, maxPayload, timeout)
		if err != nil {
			t.Fatal(err)
		}
		model := newReassemblyModel(maxFlows, maxBytes, maxPayload, timeout)
		now := time.Unix(0, 0).UTC()
		trace := make([]string, 0, 64)
		templates := make(map[reassemblyModelKey][]reassemblyCommand)
		last := make([]reassemblyCommand, 0, 16)

		runAdd := func(command reassemblyCommand) {
			trace = append(trace, command.label)
			frame, encodeErr := EncodeDatagram(command.header, command.payload, maxPayload)
			if encodeErr != nil {
				t.Fatalf("seed=%d trace=%s encode %s: %v", seed, strings.Join(trace, ","), command.label, encodeErr)
			}
			wantPayload, wantComplete, wantErr := model.add(command.header, command.payload, now)
			gotPayload, gotComplete, gotErr := sut.Add(frame, now)
			if gotComplete != wantComplete || reassemblyErrorClass(gotErr) != reassemblyErrorClass(wantErr) || !bytes.Equal(gotPayload, wantPayload) {
				t.Fatalf("seed=%d trace=%s result=(%q,%v,%s), want=(%q,%v,%s)", seed, strings.Join(trace, ","), gotPayload, gotComplete, reassemblyErrorClass(gotErr), wantPayload, wantComplete, reassemblyErrorClass(wantErr))
			}
			gotFlows, gotBytes := sut.InFlight()
			if gotFlows != len(model.flows) || gotBytes != model.bytes {
				t.Fatalf("seed=%d trace=%s inflight=(%d,%d), want=(%d,%d)", seed, strings.Join(trace, ","), gotFlows, gotBytes, len(model.flows), model.bytes)
			}
			last = append(last, command)
			if len(last) > 16 {
				last = last[1:]
			}
		}

		// Always exercise a complete out-of-order message and its replay guard
		// before the generated command stream begins.
		initial := makeReassemblyTemplate(1, 1, 3, seed)
		templates[reassemblyModelKey{flowID: 1, sequence: 1}] = initial
		for _, index := range []int{1, 0, 2} {
			runAdd(initial[index])
		}
		runAdd(initial[2])

		for step := 0; step < 64; step++ {
			now = now.Add(time.Duration(random.Intn(2)) * time.Second)
			kind := random.Intn(6)
			if kind == 5 {
				now = now.Add(timeout + time.Second)
				trace = append(trace, "expire")
				model.expire(now)
				sut.Expire(now)
				gotFlows, gotBytes := sut.InFlight()
				if gotFlows != len(model.flows) || gotBytes != model.bytes {
					t.Fatalf("seed=%d trace=%s after expiry inflight=(%d,%d), want=(%d,%d)", seed, strings.Join(trace, ","), gotFlows, gotBytes, len(model.flows), model.bytes)
				}
				continue
			}

			if len(templates) == 0 || kind == 0 {
				flowID := uint64(10 + len(templates))
				sequence := uint32(1 + random.Intn(1000))
				count := uint16(1 + random.Intn(3))
				key := reassemblyModelKey{flowID: flowID, sequence: sequence}
				templates[key] = makeReassemblyTemplate(flowID, sequence, count, seed+int64(step))
			}

			keys := make([]reassemblyModelKey, 0, len(templates))
			for key := range templates {
				keys = append(keys, key)
			}
			key := keys[random.Intn(len(keys))]
			template := templates[key]
			switch kind {
			case 1, 2:
				runAdd(template[random.Intn(len(template))])
			case 3:
				if len(last) == 0 {
					runAdd(template[0])
				} else {
					runAdd(last[random.Intn(len(last))])
				}
			case 4:
				command := template[0]
				command.label += "-mismatched-count"
				command.header.FragmentCount++
				command.header.Flags = DatagramFlagFragmented
				runAdd(command)
			}
		}
	}
}

func TestReassemblerResourceLimitsContract(t *testing.T) {
	sut, err := NewReassembler(2, 64, 32, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(0, 0).UTC()
	first := makeReassemblyTemplate(100, 1, 2, 1)
	second := makeReassemblyTemplate(101, 1, 2, 2)
	third := makeReassemblyTemplate(102, 1, 2, 3)
	for _, command := range []reassemblyCommand{first[0], second[0]} {
		frame, err := EncodeDatagram(command.header, command.payload, 32)
		if err != nil {
			t.Fatal(err)
		}
		if _, complete, err := sut.Add(frame, now); err != nil || complete {
			t.Fatalf("incomplete flow add = complete:%v err:%v", complete, err)
		}
	}
	frame, err := EncodeDatagram(third[0].header, third[0].payload, 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sut.Add(frame, now); !errors.Is(err, ErrTransient) {
		t.Fatalf("third flow error = %v, want transient flow limit", err)
	}
	flows, bytesInFlight := sut.InFlight()
	if flows != 2 || bytesInFlight == 0 {
		t.Fatalf("resource limit changed existing flows: flows=%d bytes=%d", flows, bytesInFlight)
	}
}
