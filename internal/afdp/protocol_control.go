package afdp

import (
	v1 "asterferry/internal/afdp/v1"
	"asterferry/internal/domain"
	"bytes"
	"encoding/binary"
	"errors"
	"google.golang.org/protobuf/proto"
	"net"
	"strconv"
	"strings"
)

const (
	// Keep the AFDP wire identity local to the data-plane package. This avoids
	// importing the retired relay codec's protocol package into the new data
	// path and makes the version boundary explicit at the transport edge.
	Version byte = 2
	ALPN         = "asterferry-data/2"

	SessionHelloKind  byte = 1
	SessionAcceptKind byte = 2
	OpenKind          byte = 3

	maxSessionCapabilities = 64
	maxSessionServiceIDs   = 4096
	maxAssignmentIDBytes   = 128
	maxServiceIDBytes      = 128
	maxTargetBytes         = 2048
	datagramHeaderBytes    = 24
	maxDatagramPayload     = 1<<16 - 1
	maxDatagramFragments   = 1024
)

var defaultCapabilities = []string{"http", "socks5", "tcp", "udp"}

var (
	ErrInvalidVersion    = errors.New("unsupported AFDP version")
	ErrMalformedFrame    = errors.New("malformed AFDP frame")
	ErrFrameTooLarge     = errors.New("AFDP frame exceeds configured limit")
	ErrUnauthorizedAgent = errors.New("agent is not present in assignment")
	// ErrTransient marks a local resource decision that rejects one stream or
	// datagram but does not invalidate the authenticated session.
	ErrTransient = errors.New("AFDP transient resource limit")
	// ErrProtocolViolation marks a malformed or otherwise session-fatal peer
	// message. Callers may close the authenticated session for this class.
	ErrProtocolViolation = errors.New("AFDP protocol violation")
	// ErrUnauthorizedOpen is scoped to one already-authenticated stream. The
	// stream is closed, but the peer session remains usable.
	ErrUnauthorizedOpen = errors.New("AFDP open is unauthorized")
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
	services := make(map[string]struct{}, len(a.ServiceIDs))
	for id := range a.ServiceIDs {
		services[id] = struct{}{}
	}
	a.ServiceIDs = services
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
	if value.Protocol != "tcp" && value.Protocol != "udp" {
		return ErrMalformedFrame
	}
	if value.Egress {
		if value.ServiceID != "" {
			return ErrMalformedFrame
		}
	} else if !validWireID(value.ServiceID, maxServiceIDBytes) {
		return ErrMalformedFrame
	}
	if len(value.Target) == 0 || len(value.Target) > maxTargetBytes || strings.ContainsAny(value.Target, "\x00\r\n") {
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
	if value == "" || len(value) > max || domain.ValidateID(value, "wire_id") != nil {
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
	message, ok := out.(proto.Message)
	if !ok {
		return ErrMalformedFrame
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data[6:], message); err != nil {
		return ErrMalformedFrame
	}
	// Unknown fields are retained by the protobuf runtime when
	// DiscardUnknown is false. Retention alone would let a newer peer add a
	// field while an older peer silently accepts it and re-emits the bytes.
	// AFDP control frames are intentionally fail-closed: a field added to the
	// wire contract must first be negotiated/versioned rather than ignored.
	if len(message.ProtoReflect().GetUnknown()) != 0 {
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
	if len(value.ServiceIDs) > maxSessionServiceIDs {
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
