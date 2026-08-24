package transport

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"google.golang.org/protobuf/proto"

	"asterferry/internal/protocol"
	"asterferry/internal/transport/wirev4"
)

const (
	Version         byte = protocol.Version
	DefaultMaxFrame      = 16 << 20

	TypeHello byte = iota + 1
	TypeChallenge
	TypeAuth
	TypeAuthOK
	TypeRegister
	TypeRegisterResult
	TypeOpenProxy
	TypeOpenReverse
	TypeOpenOK
	TypeOpenError
	TypeData
	TypePing
	TypePong
	TypeError
)

// ErrorCode is a stable, machine-readable v4 protocol failure category.
type ErrorCode uint32

const (
	ErrorCodeUnspecified ErrorCode = iota
	ErrorInvalidFrame
	ErrorUnsupportedVersion
	ErrorCapabilityMismatch
	ErrorLimitMismatch
	ErrorAuthFailed
	ErrorAgentLimit
	ErrorResourceExhausted
	ErrorPolicyDenied
	ErrorMappingRejected
	ErrorInternal
)

// Capability identifies an optional v4 application feature. The first two
// capabilities are mandatory for every v4 connection.
type Capability uint32

const (
	CapabilityUnspecified Capability = iota
	CapabilityErrorsV1
	CapabilityLimitsV1
	CapabilityReverseTCP    Capability = 10
	CapabilityReverseUDP    Capability = 11
	CapabilityEgressProxy   Capability = 12
	CapabilityRelayBalanced Capability = 13
)

// Limits are the bounded values negotiated during the v4 handshake.
type Limits struct {
	MaxFrameBytes  int64
	MaxRecordBytes int64
	MaxUDPBytes    int64
	MaxStreams     int64
}

// ProtocolError is safe to send over the wire. Detailed internal errors stay
// in local logs and are never copied into Detail.
type ProtocolError struct {
	Code      ErrorCode
	Detail    string
	Retryable bool
}

func NewProtocolError(code ErrorCode, detail string, retryable bool) *ProtocolError {
	if len(detail) > 128 {
		detail = detail[:128]
	}
	return &ProtocolError{Code: code, Detail: detail, Retryable: retryable}
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail != "" {
		return e.Detail
	}
	return errorCodeName(e.Code)
}

type Frame struct {
	Version   byte
	Type      byte
	RequestID uint64
	// Payload is the serialized protobuf message for Type. The outer Frame
	// itself is protobuf encoded by WriteFrame and ReadFrame.
	Payload []byte
}

func WriteFrame(w io.Writer, f Frame, max int64) error {
	if f.Version == 0 {
		f.Version = Version
	}
	if f.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", f.Version)
	}
	if !validType(f.Type) {
		return fmt.Errorf("unsupported frame type %d", f.Type)
	}
	if max <= 0 {
		max = DefaultMaxFrame
	}
	wireFrame := &wirev4.Frame{
		Version:   uint32(f.Version),
		Type:      wirev4.FrameType(f.Type),
		RequestId: f.RequestID,
		Payload:   append([]byte(nil), f.Payload...),
	}
	b, err := proto.Marshal(wireFrame)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if int64(len(b)) > max {
		return errors.New("frame exceeds configured limit")
	}
	if uint64(len(b)) > uint64(^uint32(0)) {
		return errors.New("frame exceeds wire length limit")
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(b)))
	if err := writeAll(w, length[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}

func ReadFrame(r io.Reader, max int64) (Frame, error) {
	if max <= 0 {
		max = DefaultMaxFrame
	}
	var lengthBytes [4]byte
	if _, err := io.ReadFull(r, lengthBytes[:]); err != nil {
		return Frame{}, err
	}
	length := int64(binary.BigEndian.Uint32(lengthBytes[:]))
	if length < 1 || length > max {
		return Frame{}, fmt.Errorf("invalid frame length %d", length)
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return Frame{}, err
	}
	var wireFrame wirev4.Frame
	if err := proto.Unmarshal(b, &wireFrame); err != nil {
		return Frame{}, fmt.Errorf("decode frame: %w", err)
	}
	if wireFrame.Version != uint32(Version) {
		return Frame{}, fmt.Errorf("unsupported protocol version %d", wireFrame.Version)
	}
	if !validType(byte(wireFrame.Type)) {
		return Frame{}, fmt.Errorf("unsupported frame type %d", wireFrame.Type)
	}
	return Frame{
		Version:   Version,
		Type:      byte(wireFrame.Type),
		RequestID: wireFrame.RequestId,
		Payload:   append([]byte(nil), wireFrame.Payload...),
	}, nil
}

// MessageFrame serializes one typed v4 payload into an outer frame.
func MessageFrame(typ byte, requestID uint64, v any) (Frame, error) {
	if !validType(typ) {
		return Frame{}, fmt.Errorf("unsupported frame type %d", typ)
	}
	payload, err := marshalPayload(typ, v)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Version: Version, Type: typ, RequestID: requestID, Payload: payload}, nil
}

// DecodeMessage decodes a frame's inner protobuf payload into the matching
// domain type. It rejects a type/payload mismatch before the caller acts on
// any fields.
func DecodeMessage(f Frame, v any) error {
	if f.Version != 0 && f.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", f.Version)
	}
	return unmarshalPayload(f.Type, f.Payload, v)
}

func validType(typ byte) bool { return typ >= TypeHello && typ <= TypeError }

func isEmptyType(typ byte) bool { return typ == TypePing || typ == TypePong }

type Hello struct {
	AgentID      string
	Capabilities []Capability
	Limits       Limits
}

type Challenge struct {
	Nonce        []byte
	Capabilities []Capability
	Limits       Limits
}

type Auth struct{ MAC []byte }

type AuthResult struct{ Error *ProtocolError }

type TunnelRegistration struct {
	Name        string
	Protocol    string
	GatewayPort uint16
	Profile     string
}

type Register struct{ Mappings []TunnelRegistration }

type RegisterResult struct {
	Error    *ProtocolError
	Mappings []TunnelRegistration
}

type OpenProxy struct {
	Network string
	Address string
	Port    uint16
	Profile string
}

type OpenReverse struct {
	Name     string
	Protocol string
	Profile  string
}

type OpenResult struct{ Error *ProtocolError }

type Data struct {
	Payload []byte
	Padding []byte
}

func DecodeData(f Frame, maxPayload, maxPadding int64) (Data, error) {
	var data Data
	if err := DecodeMessage(f, &data); err != nil {
		return Data{}, err
	}
	if maxPayload < 1 || int64(len(data.Payload)) > maxPayload || int64(len(data.Padding)) > maxPadding {
		return Data{}, errors.New("data record exceeds configured limit")
	}
	return data, nil
}

func NewData(payload []byte, profile string, maxPadding int64) Data {
	data := Data{Payload: append([]byte(nil), payload...)}
	if profile != "balanced" || maxPadding <= 0 {
		return data
	}
	base := int64(len(payload) + 64)
	for _, bucket := range []int64{512, 1024, 2048, 4096, 8192, 16384} {
		if bucket < base || bucket-base > maxPadding {
			continue
		}
		available := bucket - base
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return data
		}
		paddingLen := int(binary.BigEndian.Uint16(b[:])) % int(available+1)
		if paddingLen > 0 {
			data.Padding = make([]byte, paddingLen)
			_, _ = rand.Read(data.Padding)
		}
		return data
	}
	return data
}

func NewNonce() ([]byte, error) {
	n := make([]byte, 32)
	_, err := rand.Read(n)
	return n, err
}

// SignChallenge binds authentication to the exact v4 capability and limit
// negotiation. The transcript encoding is explicit rather than protobuf
// serialization because protobuf serialization order is not canonical.
func SignChallenge(token, nonce []byte, agentID string, capabilities []Capability, limits Limits) []byte {
	h := hmac.New(sha256.New, token)
	_, _ = h.Write([]byte(protocol.AuthDomain))
	writeTranscriptBytes(h, []byte(agentID))
	writeTranscriptBytes(h, nonce)
	for _, capability := range normalizedCapabilities(capabilities) {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(capability))
		_, _ = h.Write(b[:])
	}
	for _, value := range []int64{limits.MaxFrameBytes, limits.MaxRecordBytes, limits.MaxUDPBytes, limits.MaxStreams} {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(value))
		_, _ = h.Write(b[:])
	}
	return h.Sum(nil)
}

func VerifyChallenge(token, nonce, mac []byte, agentID string, capabilities []Capability, limits Limits) bool {
	return hmac.Equal(SignChallenge(token, nonce, agentID, capabilities, limits), mac)
}

func ValidateCapabilities(capabilities []Capability) error {
	seen := make(map[Capability]struct{}, len(capabilities))
	var previous Capability
	for index, capability := range capabilities {
		if capability == CapabilityUnspecified || !knownCapability(capability) {
			return fmt.Errorf("unsupported capability %d", capability)
		}
		if index > 0 && capability <= previous {
			return errors.New("capabilities must be sorted and unique")
		}
		if _, ok := seen[capability]; ok {
			return fmt.Errorf("duplicate capability %d", capability)
		}
		seen[capability] = struct{}{}
		previous = capability
	}
	return nil
}

func normalizedCapabilities(capabilities []Capability) []Capability {
	result := append([]Capability(nil), capabilities...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func SupportsCapability(capabilities []Capability, wanted Capability) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func ValidateLimits(limits Limits) error {
	if limits.MaxFrameBytes < 1024 || limits.MaxRecordBytes < 1024 || limits.MaxUDPBytes < 512 || limits.MaxStreams < 1 {
		return errors.New("negotiated limits are too small")
	}
	if limits.MaxFrameBytes > DefaultMaxFrame || limits.MaxRecordBytes > 64<<20 || limits.MaxUDPBytes > 64<<10 || limits.MaxStreams > 65536 {
		return errors.New("negotiated limits exceed protocol bounds")
	}
	return nil
}

func NegotiateCapabilities(offered, supported []Capability) ([]Capability, error) {
	if err := ValidateCapabilities(offered); err != nil {
		return nil, err
	}
	if err := ValidateCapabilities(supported); err != nil {
		return nil, err
	}
	if !SupportsCapability(offered, CapabilityErrorsV1) || !SupportsCapability(offered, CapabilityLimitsV1) {
		return nil, errors.New("required v4 capabilities are missing")
	}
	result := make([]Capability, 0, len(offered))
	for _, capability := range normalizedCapabilities(offered) {
		if SupportsCapability(supported, capability) {
			result = append(result, capability)
		}
	}
	if !SupportsCapability(result, CapabilityErrorsV1) || !SupportsCapability(result, CapabilityLimitsV1) {
		return nil, errors.New("required v4 capabilities are unsupported")
	}
	return result, nil
}

func NegotiateLimits(offered, supported Limits) (Limits, error) {
	if err := ValidateLimits(offered); err != nil {
		return Limits{}, err
	}
	if err := ValidateLimits(supported); err != nil {
		return Limits{}, err
	}
	result := Limits{
		MaxFrameBytes:  minPositive(offered.MaxFrameBytes, supported.MaxFrameBytes),
		MaxRecordBytes: minPositive(offered.MaxRecordBytes, supported.MaxRecordBytes),
		MaxUDPBytes:    minPositive(offered.MaxUDPBytes, supported.MaxUDPBytes),
		MaxStreams:     minPositive(offered.MaxStreams, supported.MaxStreams),
	}
	if err := ValidateLimits(result); err != nil {
		return Limits{}, err
	}
	return result, nil
}

func ValidateNegotiation(offeredCaps, selectedCaps []Capability, offeredLimits, selectedLimits Limits) error {
	if err := ValidateCapabilities(offeredCaps); err != nil {
		return err
	}
	if err := ValidateCapabilities(selectedCaps); err != nil {
		return err
	}
	for _, selected := range selectedCaps {
		if !SupportsCapability(offeredCaps, selected) {
			return fmt.Errorf("peer selected an unoffered capability %d", selected)
		}
	}
	if err := ValidateLimits(offeredLimits); err != nil {
		return err
	}
	if err := ValidateLimits(selectedLimits); err != nil {
		return err
	}
	if selectedLimits.MaxFrameBytes > offeredLimits.MaxFrameBytes || selectedLimits.MaxRecordBytes > offeredLimits.MaxRecordBytes || selectedLimits.MaxUDPBytes > offeredLimits.MaxUDPBytes || selectedLimits.MaxStreams > offeredLimits.MaxStreams {
		return errors.New("peer selected limits above the offer")
	}
	if !SupportsCapability(selectedCaps, CapabilityErrorsV1) || !SupportsCapability(selectedCaps, CapabilityLimitsV1) {
		return errors.New("required v4 capabilities were not selected")
	}
	return nil
}

func minPositive(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func writeTranscriptBytes(w io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = w.Write(length[:])
	_, _ = w.Write(value)
}

func knownCapability(capability Capability) bool {
	switch capability {
	case CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP, CapabilityReverseUDP, CapabilityEgressProxy, CapabilityRelayBalanced:
		return true
	default:
		return false
	}
}

func errorCodeName(code ErrorCode) string {
	switch code {
	case ErrorInvalidFrame:
		return "invalid frame"
	case ErrorUnsupportedVersion:
		return "unsupported protocol version"
	case ErrorCapabilityMismatch:
		return "capability mismatch"
	case ErrorLimitMismatch:
		return "limit mismatch"
	case ErrorAuthFailed:
		return "authentication failed"
	case ErrorAgentLimit:
		return "agent limit reached"
	case ErrorResourceExhausted:
		return "resource exhausted"
	case ErrorPolicyDenied:
		return "policy denied"
	case ErrorMappingRejected:
		return "mapping rejected"
	case ErrorInternal:
		return "internal protocol error"
	default:
		return "protocol error"
	}
}

func knownErrorCode(code ErrorCode) bool {
	return code >= ErrorInvalidFrame && code <= ErrorInternal
}

func marshalPayload(typ byte, value any) ([]byte, error) {
	var message proto.Message
	switch typ {
	case TypeHello:
		v, ok := asHello(value)
		if !ok {
			return nil, errors.New("hello payload type mismatch")
		}
		message = &wirev4.Hello{AgentId: v.AgentID, Capabilities: capabilityValues(v.Capabilities), Limits: limitsValue(v.Limits)}
	case TypeChallenge:
		v, ok := asChallenge(value)
		if !ok {
			return nil, errors.New("challenge payload type mismatch")
		}
		message = &wirev4.Challenge{Nonce: append([]byte(nil), v.Nonce...), Capabilities: capabilityValues(v.Capabilities), Limits: limitsValue(v.Limits)}
	case TypeAuth:
		v, ok := asAuth(value)
		if !ok {
			return nil, errors.New("auth payload type mismatch")
		}
		message = &wirev4.Auth{Mac: append([]byte(nil), v.MAC...)}
	case TypeAuthOK:
		v, ok := asAuthResult(value)
		if !ok {
			return nil, errors.New("auth result payload type mismatch")
		}
		message = &wirev4.AuthResult{Error: protocolErrorValue(v.Error)}
	case TypeRegister:
		v, ok := asRegister(value)
		if !ok {
			return nil, errors.New("register payload type mismatch")
		}
		message = &wirev4.Register{Mappings: registrationValues(v.Mappings)}
	case TypeRegisterResult:
		v, ok := asRegisterResult(value)
		if !ok {
			return nil, errors.New("register result payload type mismatch")
		}
		message = &wirev4.RegisterResult{Mappings: registrationValues(v.Mappings), Error: protocolErrorValue(v.Error)}
	case TypeOpenProxy:
		v, ok := asOpenProxy(value)
		if !ok {
			return nil, errors.New("open proxy payload type mismatch")
		}
		message = &wirev4.OpenProxy{Network: v.Network, Address: v.Address, Port: uint32(v.Port), Profile: v.Profile}
	case TypeOpenReverse:
		v, ok := asOpenReverse(value)
		if !ok {
			return nil, errors.New("open reverse payload type mismatch")
		}
		message = &wirev4.OpenReverse{Name: v.Name, Protocol: v.Protocol, Profile: v.Profile}
	case TypeOpenOK, TypeOpenError:
		v, ok := asOpenResult(value)
		if !ok {
			return nil, errors.New("open result payload type mismatch")
		}
		message = &wirev4.OpenResult{Error: protocolErrorValue(v.Error)}
	case TypeData:
		v, ok := asData(value)
		if !ok {
			return nil, errors.New("data payload type mismatch")
		}
		message = &wirev4.Data{Payload: append([]byte(nil), v.Payload...), Padding: append([]byte(nil), v.Padding...)}
	case TypePing, TypePong:
		if value != nil {
			if _, ok := value.(*struct{}); !ok {
				return nil, errors.New("empty payload type mismatch")
			}
		}
		message = &wirev4.Empty{}
	case TypeError:
		v, ok := asProtocolError(value)
		if !ok {
			return nil, errors.New("protocol error payload type mismatch")
		}
		message = &wirev4.Error{Error: protocolErrorValue(v)}
	default:
		return nil, fmt.Errorf("unsupported frame type %d", typ)
	}
	return proto.Marshal(message)
}

func unmarshalPayload(typ byte, payload []byte, target any) error {
	if !validType(typ) {
		return fmt.Errorf("unsupported frame type %d", typ)
	}
	if isEmptyType(typ) {
		if len(payload) != 0 {
			var empty wirev4.Empty
			if err := proto.Unmarshal(payload, &empty); err != nil {
				return fmt.Errorf("decode empty payload: %w", err)
			}
		}
		if target != nil {
			if _, ok := target.(*struct{}); !ok {
				return errors.New("empty payload type mismatch")
			}
		}
		return nil
	}
	if target == nil {
		return errors.New("payload target is required")
	}
	switch typ {
	case TypeHello:
		var value wirev4.Hello
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*Hello)
		if !ok {
			return errors.New("hello payload type mismatch")
		}
		*v = Hello{AgentID: value.AgentId, Capabilities: capabilitiesFromValues(value.Capabilities), Limits: limitsFromValue(value.Limits)}
	case TypeChallenge:
		var value wirev4.Challenge
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*Challenge)
		if !ok {
			return errors.New("challenge payload type mismatch")
		}
		*v = Challenge{Nonce: append([]byte(nil), value.Nonce...), Capabilities: capabilitiesFromValues(value.Capabilities), Limits: limitsFromValue(value.Limits)}
	case TypeAuth:
		var value wirev4.Auth
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*Auth)
		if !ok {
			return errors.New("auth payload type mismatch")
		}
		v.MAC = append([]byte(nil), value.Mac...)
	case TypeAuthOK:
		var value wirev4.AuthResult
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*AuthResult)
		if !ok {
			return errors.New("auth result payload type mismatch")
		}
		v.Error = protocolErrorFromValue(value.Error)
	case TypeRegister:
		var value wirev4.Register
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*Register)
		if !ok {
			return errors.New("register payload type mismatch")
		}
		mappings, err := registrationsFromValues(value.Mappings)
		if err != nil {
			return err
		}
		v.Mappings = mappings
	case TypeRegisterResult:
		var value wirev4.RegisterResult
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*RegisterResult)
		if !ok {
			return errors.New("register result payload type mismatch")
		}
		mappings, err := registrationsFromValues(value.Mappings)
		if err != nil {
			return err
		}
		v.Mappings = mappings
		v.Error = protocolErrorFromValue(value.Error)
	case TypeOpenProxy:
		var value wirev4.OpenProxy
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*OpenProxy)
		if !ok {
			return errors.New("open proxy payload type mismatch")
		}
		if value.Port > 65535 {
			return errors.New("open proxy port is out of range")
		}
		v.Network, v.Address, v.Port, v.Profile = value.Network, value.Address, uint16(value.Port), value.Profile
	case TypeOpenReverse:
		var value wirev4.OpenReverse
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*OpenReverse)
		if !ok {
			return errors.New("open reverse payload type mismatch")
		}
		v.Name, v.Protocol, v.Profile = value.Name, value.Protocol, value.Profile
	case TypeOpenOK, TypeOpenError:
		var value wirev4.OpenResult
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*OpenResult)
		if !ok {
			return errors.New("open result payload type mismatch")
		}
		v.Error = protocolErrorFromValue(value.Error)
	case TypeData:
		var value wirev4.Data
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*Data)
		if !ok {
			return errors.New("data payload type mismatch")
		}
		v.Payload = append([]byte(nil), value.Payload...)
		v.Padding = append([]byte(nil), value.Padding...)
	case TypeError:
		var value wirev4.Error
		if err := proto.Unmarshal(payload, &value); err != nil {
			return err
		}
		v, ok := target.(*ProtocolError)
		if !ok {
			return errors.New("protocol error payload type mismatch")
		}
		converted := protocolErrorFromValue(value.Error)
		if converted == nil {
			return errors.New("protocol error payload is missing")
		}
		*v = *converted
	}
	return nil
}

func asHello(v any) (Hello, bool) {
	switch x := v.(type) {
	case Hello:
		return x, true
	case *Hello:
		if x != nil {
			return *x, true
		}
	}
	return Hello{}, false
}
func asChallenge(v any) (Challenge, bool) {
	switch x := v.(type) {
	case Challenge:
		return x, true
	case *Challenge:
		if x != nil {
			return *x, true
		}
	}
	return Challenge{}, false
}
func asAuth(v any) (Auth, bool) {
	switch x := v.(type) {
	case Auth:
		return x, true
	case *Auth:
		if x != nil {
			return *x, true
		}
	}
	return Auth{}, false
}
func asAuthResult(v any) (AuthResult, bool) {
	switch x := v.(type) {
	case AuthResult:
		return x, true
	case *AuthResult:
		if x != nil {
			return *x, true
		}
	}
	return AuthResult{}, false
}
func asRegister(v any) (Register, bool) {
	switch x := v.(type) {
	case Register:
		return x, true
	case *Register:
		if x != nil {
			return *x, true
		}
	}
	return Register{}, false
}
func asRegisterResult(v any) (RegisterResult, bool) {
	switch x := v.(type) {
	case RegisterResult:
		return x, true
	case *RegisterResult:
		if x != nil {
			return *x, true
		}
	}
	return RegisterResult{}, false
}
func asOpenProxy(v any) (OpenProxy, bool) {
	switch x := v.(type) {
	case OpenProxy:
		return x, true
	case *OpenProxy:
		if x != nil {
			return *x, true
		}
	}
	return OpenProxy{}, false
}
func asOpenReverse(v any) (OpenReverse, bool) {
	switch x := v.(type) {
	case OpenReverse:
		return x, true
	case *OpenReverse:
		if x != nil {
			return *x, true
		}
	}
	return OpenReverse{}, false
}
func asOpenResult(v any) (OpenResult, bool) {
	switch x := v.(type) {
	case OpenResult:
		return x, true
	case *OpenResult:
		if x != nil {
			return *x, true
		}
	}
	return OpenResult{}, false
}
func asData(v any) (Data, bool) {
	switch x := v.(type) {
	case Data:
		return x, true
	case *Data:
		if x != nil {
			return *x, true
		}
	}
	return Data{}, false
}
func asProtocolError(v any) (*ProtocolError, bool) {
	switch x := v.(type) {
	case ProtocolError:
		return &x, true
	case *ProtocolError:
		return x, x != nil
	}
	return nil, false
}

func capabilityValues(values []Capability) []wirev4.Capability {
	result := make([]wirev4.Capability, 0, len(values))
	for _, value := range values {
		result = append(result, wirev4.Capability(value))
	}
	return result
}

func capabilitiesFromValues(values []wirev4.Capability) []Capability {
	result := make([]Capability, 0, len(values))
	for _, value := range values {
		result = append(result, Capability(value))
	}
	return result
}

func limitsValue(value Limits) *wirev4.Limits {
	return &wirev4.Limits{MaxFrameBytes: uint64(value.MaxFrameBytes), MaxRecordBytes: uint64(value.MaxRecordBytes), MaxUdpBytes: uint64(value.MaxUDPBytes), MaxStreams: uint64(value.MaxStreams)}
}

func limitsFromValue(value *wirev4.Limits) Limits {
	if value == nil {
		return Limits{}
	}
	return Limits{MaxFrameBytes: int64(value.MaxFrameBytes), MaxRecordBytes: int64(value.MaxRecordBytes), MaxUDPBytes: int64(value.MaxUdpBytes), MaxStreams: int64(value.MaxStreams)}
}

func registrationValues(values []TunnelRegistration) []*wirev4.TunnelRegistration {
	result := make([]*wirev4.TunnelRegistration, 0, len(values))
	for _, value := range values {
		result = append(result, &wirev4.TunnelRegistration{Name: value.Name, Protocol: value.Protocol, GatewayPort: uint32(value.GatewayPort), Profile: value.Profile})
	}
	return result
}

func registrationsFromValues(values []*wirev4.TunnelRegistration) ([]TunnelRegistration, error) {
	result := make([]TunnelRegistration, 0, len(values))
	for _, value := range values {
		if value == nil {
			return nil, errors.New("nil tunnel registration")
		}
		if value.GatewayPort > 65535 {
			return nil, errors.New("tunnel gateway port is out of range")
		}
		result = append(result, TunnelRegistration{Name: value.Name, Protocol: value.Protocol, GatewayPort: uint16(value.GatewayPort), Profile: value.Profile})
	}
	return result, nil
}

func protocolErrorValue(value *ProtocolError) *wirev4.ProtocolError {
	if value == nil {
		return nil
	}
	code := value.Code
	if !knownErrorCode(code) {
		code = ErrorInternal
	}
	detail := value.Detail
	if len(detail) > 128 {
		detail = detail[:128]
	}
	return &wirev4.ProtocolError{Code: wirev4.ErrorCode(code), Detail: detail, Retryable: value.Retryable}
}

func protocolErrorFromValue(value *wirev4.ProtocolError) *ProtocolError {
	if value == nil {
		return nil
	}
	detail := value.Detail
	if len(detail) > 128 {
		detail = detail[:128]
	}
	code := ErrorCode(value.Code)
	if !knownErrorCode(code) {
		code = ErrorInternal
	}
	return &ProtocolError{Code: code, Detail: detail, Retryable: value.Retryable}
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
