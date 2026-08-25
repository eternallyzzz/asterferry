package transport

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"

	"asterferry/internal/protocol"
)

const (
	Version             byte = protocol.Version
	DefaultMaxFrame          = 16 << 20
	HandshakeMaxFrame        = 64 << 10
	MaxCapabilities          = 16
	MaxAgentIDBytes          = 128
	MaxMappingNameBytes      = 128
	MaxEndpointBytes         = 2048
	MaxMappings              = 256
	MaxProxyCandidates       = 16

	frameHeaderBytes = 16

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

// ErrorCode is a stable, machine-readable v6 protocol failure category.
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

// Capability identifies an optional v6 application feature. The first two
// capabilities are mandatory for every v6 connection.
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

// Limits are the bounded values negotiated during the v6 handshake.
type Limits struct {
	MaxFrameBytes      int64
	MaxRecordBytes     int64
	MaxWriteBatchBytes int64
	MaxUDPBytes        int64
	MaxStreams         int64
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

// Frame is the v6 control envelope. Payload is the deterministic binary
// encoding of the typed message selected by Type.
type Frame struct {
	Version   byte
	Type      byte
	RequestID uint64
	Payload   []byte
}

// WriteFrame writes a fixed 16-byte v6 header followed by its payload. max is
// the total frame limit, including the header.
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
	if max < frameHeaderBytes || int64(len(f.Payload)) > max-frameHeaderBytes {
		return errors.New("frame exceeds configured limit")
	}
	if uint64(len(f.Payload)) > uint64(^uint32(0)) {
		return errors.New("frame exceeds wire length limit")
	}
	var header [frameHeaderBytes]byte
	header[0] = f.Version
	header[1] = f.Type
	// Bytes 2..3 are reserved flags and must remain zero.
	binary.BigEndian.PutUint32(header[4:8], uint32(len(f.Payload)))
	binary.BigEndian.PutUint64(header[8:16], f.RequestID)
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, f.Payload)
}

// ReadFrame reads and validates a v6 control envelope before allocating its
// payload. max is the total frame limit, including the header.
func ReadFrame(r io.Reader, max int64) (Frame, error) {
	if max <= 0 {
		max = DefaultMaxFrame
	}
	if max < frameHeaderBytes {
		return Frame{}, errors.New("frame limit is smaller than the header")
	}
	var header [frameHeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	if header[0] != Version {
		return Frame{}, fmt.Errorf("unsupported protocol version %d", header[0])
	}
	if binary.BigEndian.Uint16(header[2:4]) != 0 {
		return Frame{}, errors.New("unsupported frame flags")
	}
	typ := header[1]
	if !validType(typ) {
		return Frame{}, fmt.Errorf("unsupported frame type %d", typ)
	}
	length := int64(binary.BigEndian.Uint32(header[4:8]))
	if length > max-frameHeaderBytes {
		return Frame{}, fmt.Errorf("frame payload exceeds configured limit: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Version: Version, Type: typ, RequestID: binary.BigEndian.Uint64(header[8:16]), Payload: payload}, nil
}

// MessageFrame serializes one typed v6 payload into a control frame.
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

// DecodeMessage decodes a frame's deterministic binary payload into the
// matching domain type. It rejects a type/payload mismatch before the caller
// acts on any fields.
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
	GatewayBind string
	Profile     string
}

type Register struct{ Mappings []TunnelRegistration }

type RegisterResult struct {
	Error    *ProtocolError
	Mappings []TunnelRegistration
}

type OpenProxy struct {
	Network    string
	Address    string
	Port       uint16
	Profile    string
	Candidates []string
}

type OpenReverse struct {
	Name     string
	Protocol string
	Profile  string
}

type OpenResult struct{ Error *ProtocolError }

// Data is retained for reliable UDP records carried on a control-opened
// stream. TCP/proxy streams use relay.Conn's compact record format directly.
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
	base := int64(len(payload) + 16)
	for _, bucket := range []int64{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536} {
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

// SignChallenge binds authentication to the exact v6 capability and limit
// negotiation. The transcript encoding is explicit and canonical.
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
	for _, value := range []int64{limits.MaxFrameBytes, limits.MaxRecordBytes, limits.MaxWriteBatchBytes, limits.MaxUDPBytes, limits.MaxStreams} {
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
	if len(capabilities) > MaxCapabilities {
		return fmt.Errorf("too many capabilities: %d", len(capabilities))
	}
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

func ValidateHello(value Hello) error {
	if !validIdentifier(value.AgentID, MaxAgentIDBytes) {
		return errors.New("invalid agent identity")
	}
	if err := ValidateCapabilities(value.Capabilities); err != nil {
		return err
	}
	return ValidateLimits(value.Limits)
}

func ValidateChallenge(value Challenge) error {
	if len(value.Nonce) != sha256.Size {
		return errors.New("challenge nonce must be 32 bytes")
	}
	if err := ValidateCapabilities(value.Capabilities); err != nil {
		return err
	}
	return ValidateLimits(value.Limits)
}

func ValidateAuth(value Auth) error {
	if len(value.MAC) != sha256.Size {
		return errors.New("authentication MAC must be 32 bytes")
	}
	return nil
}

func ValidateRegister(value Register) error {
	if len(value.Mappings) > MaxMappings {
		return fmt.Errorf("too many mappings: %d", len(value.Mappings))
	}
	for _, mapping := range value.Mappings {
		if !validIdentifier(mapping.Name, MaxMappingNameBytes) || (mapping.Protocol != "tcp" && mapping.Protocol != "udp") || mapping.GatewayPort == 0 || !validProfile(mapping.Profile) || !validBindAddress(mapping.GatewayBind) {
			return errors.New("invalid mapping registration")
		}
	}
	return nil
}

func ValidateOpenProxy(value OpenProxy) error {
	if value.Network != "tcp" && value.Network != "udp" {
		return errors.New("invalid proxy network")
	}
	if value.Port == 0 || !validEndpointText(value.Address) || !validProfile(value.Profile) || len(value.Candidates) > MaxProxyCandidates {
		return errors.New("invalid proxy destination")
	}
	seen := make(map[string]struct{}, len(value.Candidates))
	for _, candidate := range value.Candidates {
		addr, err := netip.ParseAddr(strings.TrimSpace(candidate))
		if err != nil || !addr.IsValid() || strings.Contains(candidate, "%") {
			return errors.New("invalid proxy destination candidate")
		}
		key := addr.Unmap().String()
		if _, ok := seen[key]; ok {
			return errors.New("duplicate proxy destination candidate")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validBindAddress(value string) bool {
	if strings.TrimSpace(value) == "" {
		return true
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	return err == nil && addr.IsValid() && !strings.Contains(value, "%")
}

func ValidateOpenReverse(value OpenReverse) error {
	if !validIdentifier(value.Name, MaxMappingNameBytes) || (value.Protocol != "tcp" && value.Protocol != "udp") || !validProfile(value.Profile) {
		return errors.New("invalid reverse mapping")
	}
	return nil
}

func validEndpointText(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= MaxEndpointBytes && !strings.ContainsAny(value, "\x00\r\n")
}

func validIdentifier(value string, max int) bool {
	if value == "" || len(value) > max || !isASCIIAlphaNumeric(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !isASCIIAlphaNumeric(value[i]) && value[i] != '-' && value[i] != '_' && value[i] != '.' {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func validProfile(value string) bool { return value == "standard" || value == "balanced" }

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
	if limits.MaxWriteBatchBytes < 0 || limits.MaxWriteBatchBytes > 4<<20 || limits.MaxWriteBatchBytes > 0 && limits.MaxWriteBatchBytes < limits.MaxRecordBytes {
		return errors.New("negotiated write batch limit is out of range")
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
		return nil, errors.New("required v6 capabilities are missing")
	}
	result := make([]Capability, 0, len(offered))
	for _, capability := range normalizedCapabilities(offered) {
		if SupportsCapability(supported, capability) {
			result = append(result, capability)
		}
	}
	if !SupportsCapability(result, CapabilityErrorsV1) || !SupportsCapability(result, CapabilityLimitsV1) {
		return nil, errors.New("required v6 capabilities are unsupported")
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
		MaxFrameBytes:      minPositive(offered.MaxFrameBytes, supported.MaxFrameBytes),
		MaxRecordBytes:     minPositive(offered.MaxRecordBytes, supported.MaxRecordBytes),
		MaxWriteBatchBytes: minPositiveNonZero(offered.MaxWriteBatchBytes, supported.MaxWriteBatchBytes),
		MaxUDPBytes:        minPositive(offered.MaxUDPBytes, supported.MaxUDPBytes),
		MaxStreams:         minPositive(offered.MaxStreams, supported.MaxStreams),
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
	if selectedLimits.MaxFrameBytes > offeredLimits.MaxFrameBytes || selectedLimits.MaxRecordBytes > offeredLimits.MaxRecordBytes || (selectedLimits.MaxWriteBatchBytes > 0 && offeredLimits.MaxWriteBatchBytes > 0 && selectedLimits.MaxWriteBatchBytes > offeredLimits.MaxWriteBatchBytes) || selectedLimits.MaxUDPBytes > offeredLimits.MaxUDPBytes || selectedLimits.MaxStreams > offeredLimits.MaxStreams {
		return errors.New("peer selected limits above the offer")
	}
	if !SupportsCapability(selectedCaps, CapabilityErrorsV1) || !SupportsCapability(selectedCaps, CapabilityLimitsV1) {
		return errors.New("required v6 capabilities were not selected")
	}
	return nil
}

func minPositive(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minPositiveNonZero(a, b int64) int64 {
	if a == 0 {
		return b
	}
	if b == 0 || a < b {
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

type frameEncoder struct{ buf []byte }

func (e *frameEncoder) uvarint(value uint64) {
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(scratch[:], value)
	e.buf = append(e.buf, scratch[:n]...)
}

func (e *frameEncoder) bytes(value []byte) {
	e.uvarint(uint64(len(value)))
	e.buf = append(e.buf, value...)
}

func (e *frameEncoder) string(value string) { e.bytes([]byte(value)) }

func (e *frameEncoder) boolean(value bool) {
	if value {
		e.buf = append(e.buf, 1)
		return
	}
	e.buf = append(e.buf, 0)
}

func (e *frameEncoder) capabilities(values []Capability) {
	values = normalizedCapabilities(values)
	e.uvarint(uint64(len(values)))
	for _, value := range values {
		e.uvarint(uint64(value))
	}
}

func (e *frameEncoder) limits(value Limits) {
	e.uvarint(uint64(value.MaxFrameBytes))
	e.uvarint(uint64(value.MaxRecordBytes))
	e.uvarint(uint64(value.MaxWriteBatchBytes))
	e.uvarint(uint64(value.MaxUDPBytes))
	e.uvarint(uint64(value.MaxStreams))
}

func (e *frameEncoder) optionalError(value *ProtocolError) {
	if value == nil {
		e.uvarint(0)
		return
	}
	e.uvarint(1)
	code := value.Code
	if !knownErrorCode(code) {
		code = ErrorInternal
	}
	e.uvarint(uint64(code))
	detail := value.Detail
	if len(detail) > 128 {
		detail = detail[:128]
	}
	e.string(detail)
	e.boolean(value.Retryable)
}

type frameDecoder struct {
	buf []byte
	off int
}

func (d *frameDecoder) remaining() int { return len(d.buf) - d.off }

func (d *frameDecoder) uvarint() (uint64, error) {
	if d.off >= len(d.buf) {
		return 0, io.ErrUnexpectedEOF
	}
	value, count := binary.Uvarint(d.buf[d.off:])
	if count == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if count < 0 {
		return 0, errors.New("binary integer overflows")
	}
	d.off += count
	return value, nil
}

func (d *frameDecoder) bytes(max int) ([]byte, error) {
	length, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if length > uint64(d.remaining()) || length > uint64(max) {
		return nil, errors.New("binary field exceeds configured limit")
	}
	start := d.off
	d.off += int(length)
	return d.buf[start:d.off], nil
}

func (d *frameDecoder) string(max int) (string, error) {
	value, err := d.bytes(max)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func (d *frameDecoder) boolean() (bool, error) {
	if d.off >= len(d.buf) {
		return false, io.ErrUnexpectedEOF
	}
	value := d.buf[d.off]
	d.off++
	if value > 1 {
		return false, errors.New("invalid boolean value")
	}
	return value == 1, nil
}

func (d *frameDecoder) int64() (int64, error) {
	value, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if value > uint64(^uint64(0)>>1) {
		return 0, errors.New("integer exceeds int64")
	}
	return int64(value), nil
}

func (d *frameDecoder) capabilities() ([]Capability, error) {
	count, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if count > MaxCapabilities || count > uint64(d.remaining()) {
		return nil, errors.New("too many capabilities")
	}
	result := make([]Capability, int(count))
	for i := range result {
		value, err := d.uvarint()
		if err != nil || value > uint64(^uint32(0)) {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("capability is out of range")
		}
		result[i] = Capability(value)
	}
	return result, nil
}

func (d *frameDecoder) limits() (Limits, error) {
	frame, err := d.int64()
	if err != nil {
		return Limits{}, err
	}
	record, err := d.int64()
	if err != nil {
		return Limits{}, err
	}
	batch, err := d.int64()
	if err != nil {
		return Limits{}, err
	}
	udp, err := d.int64()
	if err != nil {
		return Limits{}, err
	}
	streams, err := d.int64()
	if err != nil {
		return Limits{}, err
	}
	return Limits{MaxFrameBytes: frame, MaxRecordBytes: record, MaxWriteBatchBytes: batch, MaxUDPBytes: udp, MaxStreams: streams}, nil
}

func (d *frameDecoder) optionalError() (*ProtocolError, error) {
	present, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, nil
	}
	if present != 1 {
		return nil, errors.New("invalid optional error marker")
	}
	code, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	detail, err := d.string(128)
	if err != nil {
		return nil, err
	}
	retryable, err := d.boolean()
	if err != nil {
		return nil, err
	}
	converted := ErrorCode(code)
	if !knownErrorCode(converted) {
		converted = ErrorInternal
	}
	return &ProtocolError{Code: converted, Detail: detail, Retryable: retryable}, nil
}

func (d *frameDecoder) done() error {
	if d.off != len(d.buf) {
		return errors.New("trailing bytes in payload")
	}
	return nil
}

func marshalPayload(typ byte, value any) ([]byte, error) {
	e := frameEncoder{buf: make([]byte, 0, 128)}
	switch typ {
	case TypeHello:
		v, ok := asHello(value)
		if !ok {
			return nil, errors.New("hello payload type mismatch")
		}
		e.string(v.AgentID)
		e.capabilities(v.Capabilities)
		e.limits(v.Limits)
	case TypeChallenge:
		v, ok := asChallenge(value)
		if !ok {
			return nil, errors.New("challenge payload type mismatch")
		}
		e.bytes(v.Nonce)
		e.capabilities(v.Capabilities)
		e.limits(v.Limits)
	case TypeAuth:
		v, ok := asAuth(value)
		if !ok {
			return nil, errors.New("auth payload type mismatch")
		}
		e.bytes(v.MAC)
	case TypeAuthOK:
		v, ok := asAuthResult(value)
		if !ok {
			return nil, errors.New("auth result payload type mismatch")
		}
		e.optionalError(v.Error)
	case TypeRegister:
		v, ok := asRegister(value)
		if !ok {
			return nil, errors.New("register payload type mismatch")
		}
		if len(v.Mappings) > MaxMappings {
			return nil, errors.New("too many mappings")
		}
		e.uvarint(uint64(len(v.Mappings)))
		for _, mapping := range v.Mappings {
			encodeRegistration(&e, mapping)
		}
	case TypeRegisterResult:
		v, ok := asRegisterResult(value)
		if !ok {
			return nil, errors.New("register result payload type mismatch")
		}
		if len(v.Mappings) > MaxMappings {
			return nil, errors.New("too many mappings")
		}
		e.uvarint(uint64(len(v.Mappings)))
		for _, mapping := range v.Mappings {
			encodeRegistration(&e, mapping)
		}
		e.optionalError(v.Error)
	case TypeOpenProxy:
		v, ok := asOpenProxy(value)
		if !ok {
			return nil, errors.New("open proxy payload type mismatch")
		}
		e.string(v.Network)
		e.string(v.Address)
		e.uvarint(uint64(v.Port))
		e.string(v.Profile)
		if len(v.Candidates) > MaxProxyCandidates {
			return nil, errors.New("too many proxy destination candidates")
		}
		e.uvarint(uint64(len(v.Candidates)))
		for _, candidate := range v.Candidates {
			e.string(candidate)
		}
	case TypeOpenReverse:
		v, ok := asOpenReverse(value)
		if !ok {
			return nil, errors.New("open reverse payload type mismatch")
		}
		e.string(v.Name)
		e.string(v.Protocol)
		e.string(v.Profile)
	case TypeOpenOK, TypeOpenError:
		v, ok := asOpenResult(value)
		if !ok {
			return nil, errors.New("open result payload type mismatch")
		}
		e.optionalError(v.Error)
	case TypeData:
		v, ok := asData(value)
		if !ok {
			return nil, errors.New("data payload type mismatch")
		}
		e.bytes(v.Payload)
		e.bytes(v.Padding)
	case TypePing, TypePong:
		if value != nil {
			if _, ok := value.(*struct{}); !ok {
				return nil, errors.New("empty payload type mismatch")
			}
		}
	case TypeError:
		v, ok := asProtocolError(value)
		if !ok || v == nil {
			return nil, errors.New("protocol error payload type mismatch")
		}
		e.optionalError(v)
	default:
		return nil, fmt.Errorf("unsupported frame type %d", typ)
	}
	return e.buf, nil
}

func unmarshalPayload(typ byte, payload []byte, target any) error {
	if !validType(typ) {
		return fmt.Errorf("unsupported frame type %d", typ)
	}
	d := frameDecoder{buf: payload}
	if isEmptyType(typ) {
		if len(payload) != 0 {
			return errors.New("empty payload must be empty")
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
		v, ok := target.(*Hello)
		if !ok || v == nil {
			return errors.New("hello payload type mismatch")
		}
		var err error
		v.AgentID, err = d.string(MaxAgentIDBytes)
		if err != nil {
			return err
		}
		v.Capabilities, err = d.capabilities()
		if err != nil {
			return err
		}
		v.Limits, err = d.limits()
		if err != nil {
			return err
		}
	case TypeChallenge:
		v, ok := target.(*Challenge)
		if !ok || v == nil {
			return errors.New("challenge payload type mismatch")
		}
		nonce, err := d.bytes(sha256.Size)
		if err != nil {
			return err
		}
		v.Nonce = append(v.Nonce[:0], nonce...)
		v.Capabilities, err = d.capabilities()
		if err != nil {
			return err
		}
		v.Limits, err = d.limits()
		if err != nil {
			return err
		}
	case TypeAuth:
		v, ok := target.(*Auth)
		if !ok || v == nil {
			return errors.New("auth payload type mismatch")
		}
		mac, err := d.bytes(sha256.Size)
		if err != nil {
			return err
		}
		v.MAC = append(v.MAC[:0], mac...)
	case TypeAuthOK:
		v, ok := target.(*AuthResult)
		if !ok || v == nil {
			return errors.New("auth result payload type mismatch")
		}
		var err error
		v.Error, err = d.optionalError()
		if err != nil {
			return err
		}
	case TypeRegister:
		v, ok := target.(*Register)
		if !ok || v == nil {
			return errors.New("register payload type mismatch")
		}
		mappings, err := decodeRegistrations(&d)
		if err != nil {
			return err
		}
		v.Mappings = mappings
	case TypeRegisterResult:
		v, ok := target.(*RegisterResult)
		if !ok || v == nil {
			return errors.New("register result payload type mismatch")
		}
		mappings, err := decodeRegistrations(&d)
		if err != nil {
			return err
		}
		v.Mappings = mappings
		v.Error, err = d.optionalError()
		if err != nil {
			return err
		}
	case TypeOpenProxy:
		v, ok := target.(*OpenProxy)
		if !ok || v == nil {
			return errors.New("open proxy payload type mismatch")
		}
		var err error
		v.Network, err = d.string(16)
		if err != nil {
			return err
		}
		v.Address, err = d.string(MaxEndpointBytes)
		if err != nil {
			return err
		}
		port, err := d.uvarint()
		if err != nil || port > 65535 {
			if err != nil {
				return err
			}
			return errors.New("open proxy port is out of range")
		}
		v.Port = uint16(port)
		v.Profile, err = d.string(16)
		if err != nil {
			return err
		}
		count, err := d.uvarint()
		if err != nil {
			return err
		}
		if count > MaxProxyCandidates || count > uint64(d.remaining()) {
			return errors.New("too many proxy destination candidates")
		}
		v.Candidates = make([]string, int(count))
		for i := range v.Candidates {
			v.Candidates[i], err = d.string(64)
			if err != nil {
				return err
			}
		}
	case TypeOpenReverse:
		v, ok := target.(*OpenReverse)
		if !ok || v == nil {
			return errors.New("open reverse payload type mismatch")
		}
		var err error
		v.Name, err = d.string(MaxMappingNameBytes)
		if err != nil {
			return err
		}
		v.Protocol, err = d.string(8)
		if err != nil {
			return err
		}
		v.Profile, err = d.string(16)
		if err != nil {
			return err
		}
	case TypeOpenOK, TypeOpenError:
		v, ok := target.(*OpenResult)
		if !ok || v == nil {
			return errors.New("open result payload type mismatch")
		}
		var err error
		v.Error, err = d.optionalError()
		if err != nil {
			return err
		}
	case TypeData:
		v, ok := target.(*Data)
		if !ok || v == nil {
			return errors.New("data payload type mismatch")
		}
		payload, err := d.bytes(DefaultMaxFrame)
		if err != nil {
			return err
		}
		padding, err := d.bytes(DefaultMaxFrame)
		if err != nil {
			return err
		}
		v.Payload = append(v.Payload[:0], payload...)
		v.Padding = append(v.Padding[:0], padding...)
	case TypeError:
		v, ok := target.(*ProtocolError)
		if !ok || v == nil {
			return errors.New("protocol error payload type mismatch")
		}
		converted, err := d.optionalError()
		if err != nil {
			return err
		}
		if converted == nil {
			return errors.New("protocol error payload is missing")
		}
		*v = *converted
	}
	return d.done()
}

func encodeRegistration(e *frameEncoder, value TunnelRegistration) {
	e.string(value.Name)
	e.string(value.Protocol)
	e.uvarint(uint64(value.GatewayPort))
	e.string(value.GatewayBind)
	e.string(value.Profile)
}

func decodeRegistrations(d *frameDecoder) ([]TunnelRegistration, error) {
	count, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if count > MaxMappings || count > uint64(d.remaining()) {
		return nil, errors.New("too many tunnel registrations")
	}
	result := make([]TunnelRegistration, int(count))
	for i := range result {
		result[i].Name, err = d.string(MaxMappingNameBytes)
		if err != nil {
			return nil, err
		}
		result[i].Protocol, err = d.string(8)
		if err != nil {
			return nil, err
		}
		port, err := d.uvarint()
		if err != nil || port > 65535 {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("tunnel gateway port is out of range")
		}
		result[i].GatewayPort = uint16(port)
		result[i].GatewayBind, err = d.string(64)
		if err != nil {
			return nil, err
		}
		result[i].Profile, err = d.string(16)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
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
