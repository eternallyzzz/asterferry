// Package afdp implements the small, data-plane-only AFDP/1 wire helpers.
// QUIC connection setup is intentionally left to the gateway/agent runtime;
// this package only knows about bytes on an already authenticated QUIC
// session and therefore cannot accidentally import the Controller.
package afdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "asterferry/internal/data/v1"
	"asterferry/internal/domain"
	"google.golang.org/protobuf/proto"
)

const (
	// Keep the AFDP wire identity local to the data-plane package. This avoids
	// importing the retired relay codec's protocol package into the new data
	// path and makes the version boundary explicit at the transport edge.
	Version byte = 1
	ALPN         = "asterferry-data/1"

	SessionHelloKind  byte = 1
	SessionAcceptKind byte = 2
	OpenKind          byte = 3

	maxSessionCapabilities = 64
	maxAssignmentIDBytes   = 128
	maxServiceIDBytes      = 128
	maxTargetBytes         = 2048
	datagramHeaderBytes    = 24
)

var (
	ErrInvalidVersion    = errors.New("unsupported AFDP version")
	ErrMalformedFrame    = errors.New("malformed AFDP frame")
	ErrFrameTooLarge     = errors.New("AFDP frame exceeds configured limit")
	ErrUnauthorizedAgent = errors.New("agent is not present in assignment")
)

// AssignmentView is the only control state the data plane needs for session
// admission. It can be built from a domain.Assignment without giving AFDP any
// knowledge of the Controller or its persistence.
type AssignmentView struct {
	ID         string
	AgentID    string
	Generation uint64
	State      string
	ServiceIDs map[string]struct{}
	MaxStreams int
}

func AssignmentFromDomain(value domain.Assignment, maxStreams int) AssignmentView {
	services := make(map[string]struct{}, len(value.ServiceIDs))
	for _, id := range value.ServiceIDs {
		services[id] = struct{}{}
	}
	return AssignmentView{ID: value.ID, AgentID: value.AgentID, Generation: value.Generation, State: value.State, ServiceIDs: services, MaxStreams: maxStreams}
}

func (a AssignmentView) Clone() AssignmentView {
	a.ServiceIDs = make(map[string]struct{}, len(a.ServiceIDs))
	for id := range a.ServiceIDs {
		a.ServiceIDs[id] = struct{}{}
	}
	return a
}

func AuthorizeSession(hello SessionHello, assignment AssignmentView) error {
	if err := validateSession(hello); err != nil {
		return err
	}
	if hello.AssignmentID != assignment.ID || hello.AgentID != assignment.AgentID || hello.Generation != assignment.Generation {
		return ErrUnauthorizedAgent
	}
	// Only an assignment that the Controller has observed as applied is a
	// valid placement. Pending assignments are deliberately kept out of the
	// data path until both participating nodes acknowledge the generation;
	// degraded and draining placements fail closed while they are being
	// replaced. A zero state is retained for in-memory protocol fixtures that
	// predate the lifecycle field; persisted/API assignments normalize to
	// pending before they reach a node.
	if assignment.State == domain.AssignmentPending || assignment.State == domain.AssignmentDraining || assignment.State == domain.AssignmentDegraded {
		return ErrUnauthorizedAgent
	}
	return nil
}

func AuthorizeOpen(open OpenMetadata, assignment AssignmentView) error {
	if err := EncodeOpenValidation(open); err != nil {
		return err
	}
	if assignment.State == domain.AssignmentPending || assignment.State == domain.AssignmentDraining || assignment.State == domain.AssignmentDegraded {
		return ErrUnauthorizedAgent
	}
	if open.Egress {
		// An egress open is authorized by the authenticated assignment itself.
		// It deliberately carries no ServiceID: accepting one here would let a
		// peer make a gateway egress request look like a reverse service stream.
		return nil
	}
	if _, ok := assignment.ServiceIDs[open.ServiceID]; !ok {
		return ErrUnauthorizedAgent
	}
	return nil
}

func EncodeOpenValidation(value OpenMetadata) error {
	if value.Protocol != "tcp" && value.Protocol != "udp" || (!value.Egress && !validWireID(value.ServiceID, maxServiceIDBytes)) || (value.Egress && value.ServiceID != "") || len(value.Target) == 0 || len(value.Target) > maxTargetBytes || strings.ContainsAny(value.Target, "\x00\r\n") {
		return ErrMalformedFrame
	}
	host, portText, err := net.SplitHostPort(value.Target)
	if err != nil || strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) || strings.ContainsAny(portText, "\t ") {
		return ErrMalformedFrame
	} else {
		port, portErr := strconv.Atoi(portText)
		if portErr != nil || port < 1 || port > 65535 {
			return ErrMalformedFrame
		}
	}
	if value.Protocol == "udp" && value.FlowID == 0 {
		return ErrMalformedFrame
	}
	if value.Protocol == "tcp" && value.FlowID != 0 {
		return ErrMalformedFrame
	}
	return nil
}

func validWireID(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for i, r := range value {
		if i == 0 && (r == '-' || r == '_' || r == '.') {
			return false
		}
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	last := value[len(value)-1]
	if last == '-' || last == '_' || last == '.' {
		return false
	}
	return true
}

type SessionHello struct {
	AssignmentID string   `json:"assignment_id"`
	Generation   uint64   `json:"generation"`
	AgentID      string   `json:"agent_id"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type SessionAccept struct {
	AssignmentID string   `json:"assignment_id"`
	Generation   uint64   `json:"generation"`
	Capabilities []string `json:"capabilities,omitempty"`
	ServiceIDs   []string `json:"service_ids,omitempty"`
}

type OpenMetadata struct {
	Protocol  string `json:"protocol"`
	ServiceID string `json:"service_id,omitempty"`
	Target    string `json:"target,omitempty"`
	FlowID    uint64 `json:"flow_id,omitempty"`
	Egress    bool   `json:"egress,omitempty"`
}

func EncodeSessionHello(value SessionHello, max int) ([]byte, error) {
	if err := validateSession(value); err != nil {
		return nil, err
	}
	return encodeControl(SessionHelloKind, &v1.SessionHello{AssignmentId: value.AssignmentID, Generation: value.Generation, AgentId: value.AgentID, Capabilities: CanonicalCapabilities(value.Capabilities)}, max)
}

func DecodeSessionHello(data []byte, max int) (SessionHello, error) {
	var wire v1.SessionHello
	if err := decodeControl(data, SessionHelloKind, &wire, max); err != nil {
		return SessionHello{}, err
	}
	value := SessionHello{AssignmentID: wire.AssignmentId, Generation: wire.Generation, AgentID: wire.AgentId, Capabilities: append([]string(nil), wire.Capabilities...)}
	if err := validateSession(value); err != nil {
		return SessionHello{}, err
	}
	canonical, err := EncodeSessionHello(value, max)
	if err != nil || !bytes.Equal(canonical, data) {
		return SessionHello{}, ErrMalformedFrame
	}
	return value, nil
}

func EncodeSessionAccept(value SessionAccept, max int) ([]byte, error) {
	if err := validateSessionAccept(value); err != nil {
		return nil, err
	}
	return encodeControl(SessionAcceptKind, &v1.SessionAccept{AssignmentId: value.AssignmentID, Generation: value.Generation, Capabilities: CanonicalCapabilities(value.Capabilities), ServiceIds: CanonicalServiceIDs(value.ServiceIDs)}, max)
}

func DecodeSessionAccept(data []byte, max int) (SessionAccept, error) {
	var wire v1.SessionAccept
	if err := decodeControl(data, SessionAcceptKind, &wire, max); err != nil {
		return SessionAccept{}, err
	}
	value := SessionAccept{AssignmentID: wire.AssignmentId, Generation: wire.Generation, Capabilities: append([]string(nil), wire.Capabilities...), ServiceIDs: append([]string(nil), wire.ServiceIds...)}
	if err := validateSessionAccept(value); err != nil {
		return SessionAccept{}, err
	}
	canonical, err := EncodeSessionAccept(value, max)
	if err != nil || !bytes.Equal(canonical, data) {
		return SessionAccept{}, ErrMalformedFrame
	}
	return value, nil
}

func EncodeOpen(value OpenMetadata, max int) ([]byte, error) {
	if err := EncodeOpenValidation(value); err != nil {
		return nil, err
	}
	return encodeControl(OpenKind, &v1.OpenMetadata{Protocol: value.Protocol, ServiceId: value.ServiceID, Target: value.Target, FlowId: value.FlowID, Egress: value.Egress}, max)
}

func DecodeOpen(data []byte, max int) (OpenMetadata, error) {
	var wire v1.OpenMetadata
	if err := decodeControl(data, OpenKind, &wire, max); err != nil {
		return OpenMetadata{}, err
	}
	value := OpenMetadata{Protocol: wire.Protocol, ServiceID: wire.ServiceId, Target: wire.Target, FlowID: wire.FlowId, Egress: wire.Egress}
	encoded, err := EncodeOpen(value, max)
	if err != nil {
		return OpenMetadata{}, err
	}
	if !bytes.Equal(encoded, data) {
		return OpenMetadata{}, ErrMalformedFrame
	}
	return value, nil
}

func encodeControl(kind byte, value proto.Message, max int) ([]byte, error) {
	max = normalizeFrameLimit(max)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(b) > max || len(b) > int(^uint32(0)) {
		return nil, ErrFrameTooLarge
	}
	result := make([]byte, 6+len(b))
	result[0] = Version
	result[1] = kind
	binary.BigEndian.PutUint32(result[2:6], uint32(len(b)))
	copy(result[6:], b)
	return result, nil
}

func decodeControl(data []byte, kind byte, out any, max int) error {
	max = normalizeFrameLimit(max)
	if len(data) < 6 || len(data) > max+6 || data[0] != Version || data[1] != kind {
		if len(data) >= 1 && data[0] != Version {
			return ErrInvalidVersion
		}
		return ErrMalformedFrame
	}
	length := int(binary.BigEndian.Uint32(data[2:6]))
	if length > max || length != len(data)-6 {
		return ErrMalformedFrame
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data[6:], out.(proto.Message)); err != nil {
		return ErrMalformedFrame
	}
	return nil
}

func normalizeFrameLimit(max int) int {
	if max <= 0 || max > maxSessionFrame {
		return maxSessionFrame
	}
	return max
}

func validateSession(value SessionHello) error {
	if !validWireID(value.AssignmentID, maxAssignmentIDBytes) || value.Generation == 0 || !validWireID(value.AgentID, maxAssignmentIDBytes) {
		return ErrMalformedFrame
	}
	if len(value.Capabilities) > maxSessionCapabilities {
		return ErrFrameTooLarge
	}
	seen := make(map[string]struct{}, len(value.Capabilities))
	for _, capability := range value.Capabilities {
		if capability == "" || len(capability) > 64 || strings.ContainsAny(capability, "\x00\r\n") {
			return ErrMalformedFrame
		}
		if _, ok := seen[capability]; ok {
			return ErrMalformedFrame
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func validateSessionAccept(value SessionAccept) error {
	if !validWireID(value.AssignmentID, maxAssignmentIDBytes) || value.Generation == 0 {
		return ErrMalformedFrame
	}
	if len(value.Capabilities) > maxSessionCapabilities {
		return ErrFrameTooLarge
	}
	seen := make(map[string]struct{}, len(value.Capabilities))
	for _, capability := range value.Capabilities {
		if capability == "" || len(capability) > 64 || strings.ContainsAny(capability, "\x00\r\n") {
			return ErrMalformedFrame
		}
		if _, ok := seen[capability]; ok {
			return ErrMalformedFrame
		}
		seen[capability] = struct{}{}
	}
	if len(value.ServiceIDs) > maxSessionCapabilities*4 {
		return ErrFrameTooLarge
	}
	seenServices := make(map[string]struct{}, len(value.ServiceIDs))
	for _, serviceID := range value.ServiceIDs {
		if !validWireID(serviceID, maxServiceIDBytes) {
			return ErrMalformedFrame
		}
		if _, ok := seenServices[serviceID]; ok {
			return ErrMalformedFrame
		}
		seenServices[serviceID] = struct{}{}
	}
	return nil
}

// DatagramHeader is the fixed AFDP/1 header carried in a QUIC DATAGRAM. The
// payload itself is not wrapped in a per-record envelope.
type DatagramHeader struct {
	Flags         byte
	FlowID        uint64
	Sequence      uint32
	FragmentIndex uint16
	FragmentCount uint16
}

const (
	DatagramFlagFragmented byte = 1 << iota
	DatagramFlagFin
)

// DatagramHeaderSize exposes the fixed wire size to forwarding adapters
// without allowing callers to mutate the protocol constant.
func DatagramHeaderSize() int { return datagramHeaderBytes }

func EncodeDatagram(header DatagramHeader, payload []byte, maxPayload int) ([]byte, error) {
	if header.FlowID == 0 {
		return nil, ErrMalformedFrame
	}
	if header.FragmentCount == 0 || header.FragmentIndex >= header.FragmentCount {
		return nil, ErrMalformedFrame
	}
	if header.FragmentCount > 1024 {
		return nil, ErrFrameTooLarge
	}
	if header.FragmentCount == 1 && header.FragmentIndex != 0 {
		return nil, ErrMalformedFrame
	}
	if header.FragmentCount == 1 && header.Flags&DatagramFlagFragmented != 0 {
		return nil, ErrMalformedFrame
	}
	if header.FragmentCount > 1 && header.Flags&DatagramFlagFragmented == 0 {
		return nil, ErrMalformedFrame
	}
	if header.Flags & ^byte(DatagramFlagFragmented|DatagramFlagFin) != 0 {
		return nil, ErrMalformedFrame
	}
	if header.FragmentIndex == header.FragmentCount-1 && header.Flags&DatagramFlagFin == 0 {
		return nil, ErrMalformedFrame
	}
	if header.FragmentIndex != header.FragmentCount-1 && header.Flags&DatagramFlagFin != 0 {
		return nil, ErrMalformedFrame
	}
	if maxPayload <= 0 {
		maxPayload = 64 << 10
	}
	if len(payload) > maxPayload || len(payload) > 0xffff {
		return nil, ErrFrameTooLarge
	}
	result := make([]byte, datagramHeaderBytes+len(payload))
	result[0] = Version
	result[1] = header.Flags
	binary.BigEndian.PutUint16(result[2:4], datagramHeaderBytes)
	binary.BigEndian.PutUint64(result[4:12], header.FlowID)
	binary.BigEndian.PutUint32(result[12:16], header.Sequence)
	binary.BigEndian.PutUint16(result[16:18], header.FragmentIndex)
	binary.BigEndian.PutUint16(result[18:20], header.FragmentCount)
	binary.BigEndian.PutUint16(result[20:22], uint16(len(payload)))
	// Bytes 22..23 are reserved and must stay zero.
	copy(result[datagramHeaderBytes:], payload)
	return result, nil
}

func DecodeDatagram(data []byte, maxPayload int) (DatagramHeader, []byte, error) {
	if len(data) < datagramHeaderBytes {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	if data[0] != Version {
		return DatagramHeader{}, nil, ErrInvalidVersion
	}
	if binary.BigEndian.Uint16(data[2:4]) != datagramHeaderBytes || binary.BigEndian.Uint16(data[22:24]) != 0 {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	payloadLength := int(binary.BigEndian.Uint16(data[20:22]))
	if payloadLength != len(data)-datagramHeaderBytes {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	if maxPayload <= 0 {
		maxPayload = 64 << 10
	}
	if payloadLength > maxPayload {
		return DatagramHeader{}, nil, ErrFrameTooLarge
	}
	header := DatagramHeader{Flags: data[1], FlowID: binary.BigEndian.Uint64(data[4:12]), Sequence: binary.BigEndian.Uint32(data[12:16]), FragmentIndex: binary.BigEndian.Uint16(data[16:18]), FragmentCount: binary.BigEndian.Uint16(data[18:20])}
	if header.FlowID == 0 {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	if header.FragmentCount == 0 || header.FragmentIndex >= header.FragmentCount || header.FragmentCount > 1024 || header.FragmentCount == 1 && header.Flags&DatagramFlagFragmented != 0 || header.FragmentCount > 1 && header.Flags&DatagramFlagFragmented == 0 || header.Flags & ^byte(DatagramFlagFragmented|DatagramFlagFin) != 0 {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	if header.FragmentIndex == header.FragmentCount-1 && header.Flags&DatagramFlagFin == 0 || header.FragmentIndex != header.FragmentCount-1 && header.Flags&DatagramFlagFin != 0 {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	return header, append([]byte(nil), data[datagramHeaderBytes:]...), nil
}

type fragmentSet struct {
	created time.Time
	count   uint16
	parts   map[uint16][]byte
	bytes   int
}

// Reassembler bounds both the number of concurrent flows and their total
// memory. Expired and over-budget fragments are discarded fail-closed.
type Reassembler struct {
	mu         sync.Mutex
	flows      map[flowKey]*fragmentSet
	completed  map[flowKey]time.Time
	maxFlows   int
	maxBytes   int
	maxPayload int
	timeout    time.Duration
	bytes      int
}

type flowKey struct {
	flowID   uint64
	sequence uint32
}

func NewReassembler(maxFlows, maxBytes, maxPayload int, timeout time.Duration) (*Reassembler, error) {
	if maxFlows <= 0 || maxBytes <= 0 || maxPayload <= 0 || timeout <= 0 {
		return nil, errors.New("reassembler limits must be positive")
	}
	if maxFlows > 1<<20 || maxBytes > 64<<20 || maxPayload > 64<<10 || timeout > 24*time.Hour {
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
			return nil, false, errors.New("reassembler flow limit reached")
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
		return nil, false, errors.New("reassembler byte limit reached")
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
	if len(r.completed) >= r.maxFlows*2 {
		var oldest flowKey
		var oldestAt time.Time
		for candidate, completedAt := range r.completed {
			if oldestAt.IsZero() || completedAt.Before(oldestAt) {
				oldest, oldestAt = candidate, completedAt
			}
		}
		if !oldestAt.IsZero() {
			delete(r.completed, oldest)
		}
	}
	r.completed[key] = now
	return result, true, nil
}

func (r *Reassembler) expireLocked(now time.Time) {
	for key, set := range r.flows {
		if now.Sub(set.created) >= r.timeout {
			delete(r.flows, key)
			r.bytes -= set.bytes
		}
	}
	for key, completedAt := range r.completed {
		if now.Sub(completedAt) >= r.timeout {
			delete(r.completed, key)
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
	if count > 1024 {
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

func CanonicalCapabilities(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func CanonicalServiceIDs(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
