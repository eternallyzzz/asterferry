package afdp

// This file contains the transport adapter for AFDP/2. It deliberately deals
// only in a small QUIC connection interface and AssignmentView; the
// Controller, SQLite and node bootstrap packages are not visible here.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"asterferry/internal/domain"
	"asterferry/internal/wireio"
	"github.com/quic-go/quic-go"
)

const maxSessionStreams = 1 << 20

// Conn is the subset of quic.Conn used by the AFDP session. Keeping this
// interface small makes handshake and authorization tests independent of a
// live UDP socket while still accepting *quic.Conn in production.
type Conn interface {
	OpenStreamSync(context.Context) (*quic.Stream, error)
	AcceptStream(context.Context) (*quic.Stream, error)
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
	CloseWithError(quic.ApplicationErrorCode, string) error
}

type SessionOptions struct {
	MaxFrame       int
	MaxDatagram    int
	MaxStreams     int
	ReassemblyFlow int
	ReassemblyByte int
	ReassemblyTTL  time.Duration
	Capabilities   []string
	// ExpectedPeerID is the Controller-issued node identity expected on the
	// remote QUIC certificate. The Gateway uses the assignment's AgentID on
	// the server side; an Agent sets this to the assignment's GatewayID so a
	// certificate signed by the same CA cannot impersonate another Gateway.
	ExpectedPeerID string
}

func (o SessionOptions) limits() (int, int, int, int, int, time.Duration) {
	maxFrame, maxDatagram, maxStreams := o.MaxFrame, o.MaxDatagram, o.MaxStreams
	if maxFrame <= 0 {
		maxFrame = maxSessionFrame
	}
	if maxFrame > maxSessionFrame {
		maxFrame = maxSessionFrame
	}
	if maxDatagram <= datagramHeaderBytes {
		maxDatagram = 1200
	}
	if maxDatagram > maxDatagramFrame {
		maxDatagram = maxDatagramFrame
	}
	if maxStreams <= 0 {
		maxStreams = defaultMaxStreams
	}
	if maxStreams > maxSessionStreams {
		maxStreams = maxSessionStreams
	}
	flows, bytes := o.ReassemblyFlow, o.ReassemblyByte
	if flows <= 0 {
		flows = 256
	}
	if bytes <= 0 {
		bytes = 8 << 20
	}
	timeout := o.ReassemblyTTL
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return maxFrame, maxDatagram, maxStreams, flows, bytes, timeout
}

// Session is an authenticated AFDP/2 QUIC session. The first bidirectional
// stream is retained as the reliable handshake/control stream; all subsequent
// streams carry one bounded OpenMetadata message followed by raw bytes.
type Session struct {
	conn         Conn
	assignment   AssignmentView
	maxFrame     int
	maxDatagram  int
	maxStreams   int
	opened       atomic.Int64
	closed       atomic.Bool
	control      *quic.Stream
	capabilities []string
}

func (s *Session) Assignment() AssignmentView {
	if s == nil {
		return AssignmentView{}
	}
	return s.assignment.Clone()
}
func (s *Session) ActiveStreams() int64 { return s.opened.Load() }

func (s *Session) Capabilities() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.capabilities...)
}

func ClientSession(ctx context.Context, conn Conn, hello SessionHello, options SessionOptions) (*Session, error) {
	if conn == nil {
		return nil, errors.New("AFDP connection is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if expected := strings.TrimSpace(options.ExpectedPeerID); expected != "" {
		if err := authorizePeerIdentity(conn, expected); err != nil {
			_ = conn.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP peer identity unauthorized")
			return nil, err
		}
	}
	maxFrame, maxDatagram, maxStreams, _, _, _ := options.limits()
	control, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*Session, error) {
		_ = control.Close()
		return nil, err
	}
	if err := WriteSessionHello(control, hello, maxFrame); err != nil {
		return closeOnError(err)
	}
	accept, err := ReadSessionAccept(control, maxFrame)
	if err != nil {
		return closeOnError(err)
	}
	if accept.AssignmentID != hello.AssignmentID || accept.Generation != hello.Generation {
		return closeOnError(ErrUnauthorizedAgent)
	}
	negotiated := NegotiateCapabilities(hello.Capabilities, accept.Capabilities)
	if len(negotiated) != len(accept.Capabilities) {
		return closeOnError(fmt.Errorf("%w: session accept contains capabilities not offered by the client", ErrProtocolViolation))
	}
	services := make(map[string]struct{}, len(accept.ServiceIDs))
	for _, serviceID := range accept.ServiceIDs {
		services[serviceID] = struct{}{}
	}
	return &Session{conn: conn, assignment: AssignmentView{ID: accept.AssignmentID, AgentID: hello.AgentID, Generation: accept.Generation, ServiceIDs: services, MaxStreams: maxStreams}, maxFrame: maxFrame, maxDatagram: maxDatagram, maxStreams: maxStreams, control: control, capabilities: negotiated}, nil
}

func ServerSession(ctx context.Context, conn Conn, assignment AssignmentView, options SessionOptions) (*Session, error) {
	return serverSession(ctx, conn, func(hello SessionHello) (AssignmentView, error) {
		if err := AuthorizeSession(hello, assignment); err != nil {
			return AssignmentView{}, err
		}
		return assignment, nil
	}, options)
}

// ServerSessionWithLookup accepts the first SessionHello and resolves its
// assignment from an atomically maintained data-plane index. This is the
// Gateway-side form used when one QUIC endpoint serves multiple Agents; the
// lookup callback must return only locally applied assignments.
func ServerSessionWithLookup(ctx context.Context, conn Conn, lookup func(SessionHello) (AssignmentView, bool), options SessionOptions) (*Session, error) {
	if lookup == nil {
		return nil, errors.New("AFDP assignment lookup is required")
	}
	return serverSession(ctx, conn, func(hello SessionHello) (AssignmentView, error) {
		assignment, ok := lookup(hello)
		if !ok {
			return AssignmentView{}, ErrUnauthorizedAgent
		}
		if err := AuthorizeSession(hello, assignment); err != nil {
			return AssignmentView{}, err
		}
		return assignment, nil
	}, options)
}

func serverSession(ctx context.Context, conn Conn, resolve func(SessionHello) (AssignmentView, error), options SessionOptions) (*Session, error) {
	if conn == nil {
		return nil, errors.New("AFDP connection is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	maxFrame, maxDatagram, maxStreams, _, _, _ := options.limits()
	control, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	hello, err := ReadSessionHello(control, maxFrame)
	if err != nil {
		_ = control.Close()
		return nil, err
	}
	assignment, err := resolve(hello)
	if err != nil {
		_ = control.Close()
		_ = conn.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP session unauthorized")
		return nil, err
	}
	if err := authorizePeerIdentity(conn, hello.AgentID); err != nil {
		_ = control.Close()
		_ = conn.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP peer identity unauthorized")
		return nil, err
	}
	assignment = assignment.Clone()
	serviceIDs := make([]string, 0, len(assignment.ServiceIDs))
	for serviceID := range assignment.ServiceIDs {
		serviceIDs = append(serviceIDs, serviceID)
	}
	accept := SessionAccept{AssignmentID: assignment.ID, Generation: assignment.Generation, Capabilities: NegotiateCapabilities(options.Capabilities, hello.Capabilities), ServiceIDs: serviceIDs}
	if err := WriteSessionAccept(control, accept, maxFrame); err != nil {
		_ = control.Close()
		return nil, err
	}
	if assignment.MaxStreams > 0 && assignment.MaxStreams < maxStreams {
		maxStreams = assignment.MaxStreams
	}
	return &Session{conn: conn, assignment: assignment, maxFrame: maxFrame, maxDatagram: maxDatagram, maxStreams: maxStreams, control: control, capabilities: append([]string(nil), accept.Capabilities...)}, nil
}

type quicConnectionState interface {
	ConnectionState() quic.ConnectionState
}

func authorizePeerIdentity(conn Conn, agentID string) error {
	stateProvider, ok := conn.(quicConnectionState)
	if !ok {
		// In-memory protocol tests and custom transports can provide an already
		// authenticated Conn without exposing QUIC's TLS state. Production QUIC
		// connections implement the optional interface below.
		return nil
	}
	peerCertificates := stateProvider.ConnectionState().TLS.PeerCertificates
	if len(peerCertificates) == 0 {
		return errors.New("AFDP peer certificate is missing")
	}
	certificate := peerCertificates[0]
	if certificate.Subject.CommonName == agentID {
		return nil
	}
	expected := domain.NodeIdentityURI(agentID)
	for _, uri := range certificate.URIs {
		// The identity URI is issued as spiffe://asterferry/node/<agent-id>.
		// Compare the parsed path exactly; a suffix check would let an URI such
		// as /node/other-agent authenticate as /node/agent.
		if uri != nil && uri.Scheme == expected.Scheme && uri.Host == expected.Host && strings.TrimSuffix(uri.Path, "/") == expected.Path {
			return nil
		}
	}
	return errors.New("AFDP peer certificate identity does not match agent")
}

func WriteSessionHello(w io.Writer, value SessionHello, max int) error {
	return writeControlFrame(w, EncodeSessionHello, value, max)
}

func ReadSessionHello(r io.Reader, max int) (SessionHello, error) {
	frame, err := readControlFrame(r, max)
	if err != nil {
		return SessionHello{}, err
	}
	return DecodeSessionHello(frame, max)
}

func WriteSessionAccept(w io.Writer, value SessionAccept, max int) error {
	return writeControlFrame(w, EncodeSessionAccept, value, max)
}

func ReadSessionAccept(r io.Reader, max int) (SessionAccept, error) {
	frame, err := readControlFrame(r, max)
	if err != nil {
		return SessionAccept{}, err
	}
	return DecodeSessionAccept(frame, max)
}

type sessionEncoder[T any] func(T, int) ([]byte, error)

func writeControlFrame[T any](w io.Writer, encode sessionEncoder[T], value T, max int) error {
	frame, err := encode(value, max)
	if err != nil {
		return err
	}
	if len(frame) > int(^uint32(0)) {
		return ErrFrameTooLarge
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(frame)))
	if err := wireio.WriteFull(w, size[:]); err != nil {
		return err
	}
	return wireio.WriteFull(w, frame)
}

func readControlFrame(r io.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = maxSessionFrame
	}
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || uint64(length) > uint64(max)+6 || length > maxSessionFrame+6 {
		return nil, ErrFrameTooLarge
	}
	frame := make([]byte, int(length))
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// OpenStream opens an authorized AFDP stream. The stream count is reserved
// before writing metadata and released if the write fails.
func (s *Session) OpenStream(ctx context.Context, metadata OpenMetadata) (*quic.Stream, error) {
	if s == nil || s.conn == nil || s.closed.Load() {
		return nil, errors.New("AFDP session is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := AuthorizeOpen(metadata, s.assignment); err != nil {
		return nil, err
	}
	if !s.reserveStream() {
		return nil, fmt.Errorf("%w: AFDP stream limit reached", ErrTransient)
	}
	stream, err := s.conn.OpenStreamSync(ctx)
	if err != nil {
		s.releaseStream()
		return nil, err
	}
	if err := WriteOpen(stream, metadata, s.maxFrame); err != nil {
		_ = stream.Close()
		s.releaseStream()
		return nil, err
	}
	return stream, nil
}

func (s *Session) AcceptStream(ctx context.Context) (*quic.Stream, OpenMetadata, error) {
	if s == nil || s.conn == nil || s.closed.Load() {
		return nil, OpenMetadata{}, errors.New("AFDP session is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stream, err := s.conn.AcceptStream(ctx)
	if err != nil {
		return nil, OpenMetadata{}, err
	}
	// Accept the peer stream before checking the local reservation. Otherwise
	// a full session leaves the same stream queued forever and the caller can
	// either spin or incorrectly tear down the whole session. Closing the
	// accepted stream rejects only this open and lets the loop continue.
	if !s.reserveStream() {
		_ = stream.Close()
		return nil, OpenMetadata{}, fmt.Errorf("%w: AFDP stream limit reached", ErrTransient)
	}
	metadata, err := ReadOpen(stream, s.maxFrame)
	if err != nil {
		_ = stream.Close()
		s.releaseStream()
		return nil, OpenMetadata{}, fmt.Errorf("%w: %w", ErrProtocolViolation, err)
	}
	if err := AuthorizeOpen(metadata, s.assignment); err != nil {
		_ = stream.Close()
		s.releaseStream()
		return nil, OpenMetadata{}, fmt.Errorf("%w: %w", ErrUnauthorizedOpen, err)
	}
	return stream, metadata, nil
}

func (s *Session) ReleaseStream() { s.releaseStream() }

func (s *Session) reserveStream() bool {
	for {
		current := s.opened.Load()
		if current >= int64(s.maxStreams) {
			return false
		}
		if s.opened.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (s *Session) releaseStream() {
	for {
		current := s.opened.Load()
		if current <= 0 || s.opened.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (s *Session) SendDatagram(flowID uint64, sequence uint32, payload []byte, mtu int) error {
	if s == nil || s.conn == nil || s.closed.Load() {
		return errors.New("AFDP session is closed")
	}
	if mtu <= 0 || mtu > s.maxDatagram {
		mtu = s.maxDatagram
	}
	frames, err := Fragments(flowID, sequence, payload, mtu)
	if err != nil {
		return err
	}
	for _, frame := range frames {
		if err := s.conn.SendDatagram(frame); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) ReceiveDatagram(ctx context.Context, reassembler *Reassembler) ([]byte, error) {
	_, payload, err := s.ReceiveDatagramPacket(ctx, reassembler)
	return payload, err
}

// ReceiveDatagramPacket is the flow-aware form used by UDP forwarding. The
// returned flow ID and sequence come from the final fragment that completed
// the bounded reassembly set.
func (s *Session) ReceiveDatagramPacket(ctx context.Context, reassembler *Reassembler) (DatagramHeader, []byte, error) {
	if s == nil || s.conn == nil || s.closed.Load() {
		return DatagramHeader{}, nil, errors.New("AFDP session is closed")
	}
	if reassembler == nil {
		return DatagramHeader{}, nil, errors.New("AFDP datagram reassembler is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		data, err := s.conn.ReceiveDatagram(ctx)
		if err != nil {
			return DatagramHeader{}, nil, err
		}
		header, _, err := DecodeDatagram(data, s.maxDatagram-datagramHeaderBytes)
		if err != nil {
			return DatagramHeader{}, nil, err
		}
		payload, complete, err := reassembler.Add(data, timeNow())
		if err != nil {
			return DatagramHeader{}, nil, err
		}
		if complete {
			return header, payload, nil
		}
	}
}

func (s *Session) Close() error {
	if s == nil || s.closed.Swap(true) {
		return nil
	}
	if s.control != nil {
		_ = s.control.Close()
	}
	// Closing a session invalidates all outstanding stream reservations. Any
	// later defer from a forwarding goroutine can still call ReleaseStream;
	// that method is deliberately saturating at zero.
	s.opened.Store(0)
	if s.conn != nil {
		return s.conn.CloseWithError(quic.ApplicationErrorCode(0xAF00), "AFDP session closed")
	}
	return nil
}

// Isolated indirection keeps the protocol package deterministic in tests.
var timeNow = func() time.Time { return time.Now() }

var _ Conn = (*quic.Conn)(nil)
