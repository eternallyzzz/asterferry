package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/control"
	v1 "asterferry/internal/control/v1"
	"asterferry/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ControlServer struct {
	v1.UnimplementedControlServer
	store    *Store
	config   Config
	streams  map[string]*controlStream // node id -> active stream
	streamMu sync.Mutex
}

type controlStream struct {
	cancel context.CancelFunc
	// send is installed before the stream is published in streams. It lets
	// security-sensitive Controller operations (notably revocation) deliver a
	// reconnect action synchronously before cancelling the control RPC, rather
	// than relying on a best-effort buffered subscription racing the cancel.
	send func(*v1.ControllerMessage) error
}

func NewControlServer(config Config, store *Store) (*ControlServer, error) {
	if store == nil {
		return nil, errors.New("controller store is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ControlServer{store: store, config: config, streams: make(map[string]*controlStream)}, nil
}

func (s *ControlServer) Enroll(ctx context.Context, request *v1.EnrollRequest) (*v1.EnrollResponse, error) {
	if request == nil || request.GetToken() == "" || request.GetNodeId() == "" || len(request.GetCsrDer()) == 0 || len(request.GetCsrDer()) > 128<<10 {
		return nil, status.Error(codes.InvalidArgument, "token, node_id and csr_der are required")
	}
	role, err := protoRole(request.GetRole())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	certificate, err := s.store.IssueNodeCertificate(ctx, s.config, request.GetToken(), role, request.GetNodeId(), request.GetCsrDer())
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &v1.EnrollResponse{SchemaVersion: domain.SchemaVersion, Certificate: &v1.CertificateBundle{CertificateDer: certificateDER(certificate.CertificatePEM), CaCertificateDer: certificateDER(certificate.CAPEM), Serial: certificate.Serial, NotBefore: timestamppb.New(certificate.NotBefore), NotAfter: timestamppb.New(certificate.NotAfter)}}, nil
}

func (s *ControlServer) Connect(stream v1.Control_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first == nil {
		return status.Error(codes.InvalidArgument, "first node message must be Hello")
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first node message must be Hello")
	}
	if hello.GetSchemaVersion() != domain.SchemaVersion {
		return status.Error(codes.InvalidArgument, "unknown control schema version")
	}
	if err := domain.ValidateID(hello.GetNodeId(), "node_id"); err != nil || len(hello.GetCapabilities()) > 64 {
		return status.Error(codes.InvalidArgument, "hello identity or capabilities are invalid")
	}
	seenCapabilities := make(map[string]struct{}, len(hello.GetCapabilities()))
	if hello.GetAppliedGeneration() == 0 && hello.GetAppliedChecksum() != "" {
		return status.Error(codes.InvalidArgument, "hello checksum requires an applied generation")
	}
	if hello.GetAppliedGeneration() > 0 && hello.GetAppliedChecksum() == "" {
		return status.Error(codes.InvalidArgument, "hello applied generation requires a checksum")
	}
	if hello.GetAppliedChecksum() != "" {
		if len(hello.GetAppliedChecksum()) != 64 {
			return status.Error(codes.InvalidArgument, "hello checksum is invalid")
		}
		if _, decodeErr := hex.DecodeString(hello.GetAppliedChecksum()); decodeErr != nil {
			return status.Error(codes.InvalidArgument, "hello checksum is invalid")
		}
	}
	for _, capability := range hello.GetCapabilities() {
		if strings.TrimSpace(capability) == "" || len(capability) > 64 || strings.ContainsAny(capability, "\x00\r\n") {
			return status.Error(codes.InvalidArgument, "hello capability is invalid")
		}
		if _, exists := seenCapabilities[capability]; exists {
			return status.Error(codes.InvalidArgument, "hello capabilities contain duplicates")
		}
		seenCapabilities[capability] = struct{}{}
	}
	role, err := protoRole(hello.GetRole())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	node, err := s.store.GetNode(stream.Context(), hello.GetNodeId())
	if err != nil {
		return status.Error(codes.PermissionDenied, "node is not enrolled")
	}
	if node.Role != role || !node.Enabled || node.CertificateState != domain.CertificateActive {
		return status.Error(codes.PermissionDenied, "node is disabled or role does not match")
	}
	if err := verifyPeerIdentity(stream.Context(), hello.GetNodeId()); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	if certificate, certErr := peerCertificate(stream.Context()); certErr != nil || node.CertificateSerial == "" || !strings.EqualFold(certificate.SerialNumber.Text(16), node.CertificateSerial) {
		// A rotated certificate keeps the same node identity but invalidates the
		// previous serial immediately. This closes a stream opened with an old
		// certificate even before its natural expiry.
		return status.Error(codes.PermissionDenied, "certificate serial is not current")
	}
	// Resource writes are intentionally independent from the long-lived node
	// stream. Materialize the latest node-scoped document just before sending
	// it so a reconnect always observes API changes, even if the writer was
	// offline when the resource was edited.
	if _, snapshotErr := s.store.EnsureDesiredSnapshot(stream.Context(), hello.GetNodeId()); snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
		return status.Error(codes.Internal, "build desired snapshot failed")
	}
	snapshotRecord, snapshotErr := s.store.LoadSnapshot(stream.Context(), hello.GetNodeId())
	if snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
		return status.Error(codes.Internal, "load desired snapshot failed")
	}
	if snapshotErr == nil && hello.GetAppliedGeneration() > snapshotRecord.Generation {
		return status.Error(codes.FailedPrecondition, "node reports an unapplied future generation")
	}
	if errors.Is(snapshotErr, sql.ErrNoRows) && hello.GetAppliedGeneration() != 0 {
		return status.Error(codes.FailedPrecondition, "node reports a generation without a desired snapshot")
	}
	connectionCtx, cancel := context.WithCancel(stream.Context())
	var sendMu sync.Mutex
	send := func(message *v1.ControllerMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(message)
	}
	entry := &controlStream{cancel: cancel, send: send}
	s.streamMu.Lock()
	if previous := s.streams[hello.GetNodeId()]; previous != nil {
		previous.cancel()
	}
	s.streams[hello.GetNodeId()] = entry
	s.streamMu.Unlock()
	defer func() {
		s.streamMu.Lock()
		// Do not remove a newer stream that replaced this one while it was
		// unwinding after cancellation.
		if current := s.streams[hello.GetNodeId()]; current == entry {
			delete(s.streams, hello.GetNodeId())
		}
		s.streamMu.Unlock()
		cancel()
	}()
	actionCh, unsubscribeActions := s.store.SubscribeActions(hello.GetNodeId())
	defer unsubscribeActions()
	// A node's Hello is sent before the Controller can authenticate the
	// bidirectional RPC.  Send an explicit readiness marker only after all
	// certificate, role and current-serial checks above have succeeded; the
	// node uses it to lift a reconnect/revocation drain safely.
	if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Name: "session_ready"}}}); err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-connectionCtx.Done():
				return
			case action, ok := <-actionCh:
				if !ok {
					return
				}
				if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Id: action.ID, Name: action.Name, PayloadJson: append([]byte(nil), action.Payload...)}}}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var lastSent atomic.Uint64
	lastSent.Store(hello.GetAppliedGeneration())
	if snapshotErr == nil {
		// A node's Hello carries both its applied generation and checksum. Send
		// a newer generation, or repair a same-generation cache divergence.
		if snapshotRecord.Generation > hello.GetAppliedGeneration() || (snapshotRecord.Generation == hello.GetAppliedGeneration() && !strings.EqualFold(snapshotRecord.Checksum, hello.GetAppliedChecksum())) {
			message := &v1.ControllerMessage{Body: &v1.ControllerMessage_DesiredSnapshot{DesiredSnapshot: &v1.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeId: snapshotRecord.NodeID, Generation: snapshotRecord.Generation, Checksum: snapshotRecord.Checksum, DocumentJson: snapshotRecord.Document}}}
			if err := send(message); err != nil {
				return err
			}
			lastSent.Store(snapshotRecord.Generation)
		}
	}
	go s.pushSnapshots(connectionCtx, hello.GetNodeId(), send, &lastSent)
	for {
		message, err := recvWithContext(connectionCtx, stream)
		if err != nil {
			return err
		}
		if message == nil {
			return status.Error(codes.InvalidArgument, "node message is empty")
		}
		switch {
		case message.GetHeartbeat() != nil:
			heartbeat := message.GetHeartbeat()
			currentNode, currentNodeErr := s.store.GetNode(stream.Context(), hello.GetNodeId())
			currentPeer, currentPeerErr := peerCertificate(stream.Context())
			if currentNodeErr != nil || currentPeerErr != nil || currentNode.CertificateState != domain.CertificateActive || currentNode.CertificateSerial == "" || !strings.EqualFold(currentPeer.SerialNumber.Text(16), currentNode.CertificateSerial) {
				return status.Error(codes.PermissionDenied, "certificate serial is not current")
			}
			if heartbeat.GetAppliedGeneration() > 0 {
				latest, latestErr := s.store.LoadSnapshot(stream.Context(), hello.GetNodeId())
				if latestErr == nil && heartbeat.GetAppliedGeneration() > latest.Generation {
					return status.Error(codes.InvalidArgument, "heartbeat reports a future generation")
				}
				if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
					return status.Error(codes.Internal, "load desired snapshot failed")
				}
				if errors.Is(latestErr, sql.ErrNoRows) {
					return status.Error(codes.InvalidArgument, "heartbeat has no desired generation")
				}
			}
			observed := domain.ObservedState{SchemaVersion: domain.SchemaVersion, NodeID: hello.GetNodeId(), AppliedGeneration: heartbeat.GetAppliedGeneration(), Healthy: heartbeat.GetHealthy(), Degraded: !heartbeat.GetHealthy(), ObservedAt: time.Now().UTC()}
			if heartbeat.GetSentAt() != nil {
				if err := heartbeat.GetSentAt().CheckValid(); err != nil {
					return status.Error(codes.InvalidArgument, "heartbeat timestamp is invalid")
				}
				observed.ObservedAt = heartbeat.GetSentAt().AsTime().UTC()
			}
			if observed.ObservedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
				return status.Error(codes.InvalidArgument, "heartbeat timestamp is too far in the future")
			}
			if previous, previousErr := s.store.GetObserved(stream.Context(), hello.GetNodeId()); previousErr == nil {
				observed.Sessions = previous.Sessions
				observed.Listeners = previous.Listeners
				observed.Metrics = previous.Metrics
				if !observed.Healthy {
					observed.LastError = previous.LastError
				}
			} else if !errors.Is(previousErr, sql.ErrNoRows) {
				return status.Error(codes.Internal, "load observed state failed")
			}
			document, marshalErr := jsonMarshalObserved(observed)
			if marshalErr != nil {
				return status.Error(codes.Internal, "encode heartbeat state failed")
			}
			if err := s.store.SaveObserved(stream.Context(), ObservedRecord{NodeID: hello.GetNodeId(), Generation: observed.AppliedGeneration, Document: document, UpdatedAt: observed.ObservedAt}); err != nil {
				return status.Error(codes.Internal, "save heartbeat state failed")
			}
		case message.GetObservedState() != nil:
			observed, decodeErr := control.ObservedFromProto(message.GetObservedState())
			if decodeErr != nil {
				return status.Error(codes.InvalidArgument, decodeErr.Error())
			}
			if observed.NodeID != hello.GetNodeId() {
				return status.Error(codes.InvalidArgument, "observed state node identity does not match hello")
			}
			latest, latestErr := s.store.LoadSnapshot(stream.Context(), hello.GetNodeId())
			if latestErr == nil {
				if observed.AppliedGeneration > latest.Generation {
					return status.Error(codes.InvalidArgument, "observed state reports a future generation")
				}
			} else if !errors.Is(latestErr, sql.ErrNoRows) {
				return status.Error(codes.Internal, "load desired snapshot failed")
			} else if observed.AppliedGeneration != 0 {
				return status.Error(codes.InvalidArgument, "observed state has no desired generation")
			}
			document, _ := jsonMarshalObserved(observed)
			if err := s.store.SaveObserved(stream.Context(), ObservedRecord{NodeID: hello.GetNodeId(), Generation: observed.AppliedGeneration, Document: document, UpdatedAt: time.Now().UTC()}); err != nil {
				return status.Error(codes.Internal, "save observed state failed")
			}
		case message.GetApplyResult() != nil:
			result := message.GetApplyResult()
			latest := snapshotRecord
			if current, currentErr := s.store.LoadSnapshot(stream.Context(), hello.GetNodeId()); currentErr == nil {
				latest = current
			} else if !errors.Is(currentErr, sql.ErrNoRows) {
				return status.Error(codes.Internal, "load desired snapshot failed")
			}
			if err := validateApplyResult(result, latest); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			attributes := map[string]string{"generation": fmt.Sprint(result.GetGeneration()), "status": result.GetStatus().String()}
			if result.GetChecksum() != "" {
				attributes["checksum"] = result.GetChecksum()
			}
			if result.GetError() != nil {
				attributes["error_code"] = result.GetError().GetCode()
				attributes["error_path"] = result.GetError().GetFieldPath()
			}
			if err := s.store.RecordEvent(stream.Context(), hello.GetNodeId(), "", "apply_result", result.GetStatus().String(), hello.GetNodeId(), attributes); err != nil {
				return status.Error(codes.Internal, "record apply result failed")
			}
			if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_APPLIED || result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_REJECTED {
				errorCode := ""
				if result.GetError() != nil {
					errorCode = result.GetError().GetCode()
				}
				changed, stateErr := s.store.applyNodeResultWithError(stream.Context(), hello.GetNodeId(), result.GetGeneration(), result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_APPLIED, errorCode, hello.GetNodeId())
				if stateErr != nil {
					return status.Error(codes.Internal, "update assignment state failed")
				}
				// Assignment state is part of the desired document. Refresh both
				// ends only after the state transaction commits, so a node that
				// successfully applied a pending assignment receives a stable
				// follow-up generation with state=applied rather than a partially
				// updated peer view.
				for _, assignment := range changed {
					for _, participant := range []string{assignment.GatewayID, assignment.AgentID} {
						if _, snapshotErr := s.store.EnsureDesiredSnapshot(stream.Context(), participant); snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
							return status.Error(codes.Internal, "refresh assignment snapshot failed")
						}
					}
				}
			}
		case message.GetEventBatch() != nil:
			batch := message.GetEventBatch()
			if len(batch.GetEvents()) > 256 {
				return status.Error(codes.InvalidArgument, "event batch is too large")
			}
			for _, event := range batch.GetEvents() {
				if event == nil || strings.TrimSpace(event.GetType()) == "" || len(event.GetType()) > 128 || len(event.GetMessage()) > 4096 {
					return status.Error(codes.InvalidArgument, "event fields are invalid")
				}
				attributes := map[string]string{}
				if len(event.GetAttributesJson()) > 128<<10 {
					return status.Error(codes.InvalidArgument, "event attributes are too large")
				}
				if len(event.GetAttributesJson()) > 0 {
					if err := json.Unmarshal(event.GetAttributesJson(), &attributes); err != nil {
						return status.Error(codes.InvalidArgument, "event attributes are invalid")
					}
				}
				if err := s.store.RecordEvent(stream.Context(), hello.GetNodeId(), event.GetId(), event.GetType(), event.GetMessage(), hello.GetNodeId(), attributes); err != nil {
					return status.Error(codes.InvalidArgument, err.Error())
				}
			}
		case message.GetRenewCertificate() != nil:
			if len(message.GetRenewCertificate().GetCsrDer()) == 0 || len(message.GetRenewCertificate().GetCsrDer()) > 128<<10 {
				return status.Error(codes.InvalidArgument, "renewal CSR is missing or too large")
			}
			// A stream remains open while the node is waiting for its rotated
			// certificate. Re-check the peer serial before issuing one so an old
			// stream cannot renew itself after an administrator has rotated or
			// revoked the node in the meantime.
			currentNode, currentNodeErr := s.store.GetNode(stream.Context(), hello.GetNodeId())
			currentPeer, currentPeerErr := peerCertificate(stream.Context())
			if currentNodeErr != nil || currentPeerErr != nil || currentNode.CertificateState != domain.CertificateActive || currentNode.CertificateSerial == "" || !strings.EqualFold(currentPeer.SerialNumber.Text(16), currentNode.CertificateSerial) {
				return status.Error(codes.PermissionDenied, "certificate serial is not current")
			}
			certificate, renewErr := s.store.RenewNodeCertificate(stream.Context(), s.config, hello.GetNodeId(), message.GetRenewCertificate().GetCsrDer())
			if renewErr != nil {
				return status.Error(codes.PermissionDenied, renewErr.Error())
			}
			if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_CertificateBundle{CertificateBundle: &v1.CertificateBundle{CertificateDer: certificateDER(certificate.CertificatePEM), CaCertificateDer: certificateDER(certificate.CAPEM), Serial: certificate.Serial, NotBefore: timestamppb.New(certificate.NotBefore), NotAfter: timestamppb.New(certificate.NotAfter)}}}); err != nil {
				return err
			}
			// Force the node to establish the next stream with the newly issued
			// certificate. Existing data/control streams are not silently trusted
			// after a serial rotation.
			cancel()
			return nil
		default:
			// A protobuf message with no recognized oneof body is not a
			// forward-compatible hint: accepting it would let a malformed node
			// stream stay authenticated indefinitely without any observable
			// state transition. Fail closed at the control boundary.
			return status.Error(codes.InvalidArgument, "node message body is required")
		}
	}
}

func validateApplyResult(result *v1.ApplyResult, snapshot SnapshotRecord) error {
	if result == nil || result.GetGeneration() == 0 || result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_UNSPECIFIED {
		return errors.New("apply result generation and status are required")
	}
	switch result.GetStatus() {
	case v1.ApplyStatus_APPLY_STATUS_ACCEPTED, v1.ApplyStatus_APPLY_STATUS_APPLIED, v1.ApplyStatus_APPLY_STATUS_REJECTED:
	default:
		return errors.New("apply result status is invalid")
	}
	if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_REJECTED {
		if result.GetError() == nil || strings.TrimSpace(result.GetError().GetCode()) == "" || strings.TrimSpace(result.GetError().GetMessage()) == "" {
			return errors.New("rejected apply result must include an error")
		}
		if len(result.GetError().GetCode()) > 128 || len(result.GetError().GetFieldPath()) > 256 || len(result.GetError().GetMessage()) > 2048 {
			return errors.New("apply result error is too large")
		}
	} else if result.GetError() != nil {
		return errors.New("accepted or applied result must not include an error")
	}
	if snapshot.NodeID == "" {
		return errors.New("apply result has no desired snapshot")
	}
	if result.GetGeneration() != snapshot.Generation {
		return errors.New("apply result generation does not match desired snapshot")
	}
	if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_APPLIED && result.GetChecksum() == "" {
		return errors.New("applied result must include a checksum")
	}
	if result.GetChecksum() != "" && !strings.EqualFold(result.GetChecksum(), snapshot.Checksum) {
		return errors.New("apply result checksum does not match desired snapshot")
	}
	return nil
}

func (s *ControlServer) pushSnapshots(ctx context.Context, nodeID string, send func(*v1.ControllerMessage) error, lastSent *atomic.Uint64) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshot, err := s.store.EnsureDesiredSnapshot(ctx, nodeID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				continue
			}
			for {
				previous := lastSent.Load()
				if snapshot.Generation <= previous || !lastSent.CompareAndSwap(previous, snapshot.Generation) {
					if snapshot.Generation <= previous {
						break
					}
					continue
				}
				message := &v1.ControllerMessage{Body: &v1.ControllerMessage_DesiredSnapshot{DesiredSnapshot: &v1.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeId: snapshot.NodeID, Generation: snapshot.Generation, Checksum: snapshot.Checksum, DocumentJson: snapshot.Document}}}
				if err := send(message); err != nil {
					return
				}
				break
			}
		}
	}
}

func (s *ControlServer) RevokeNode(ctx context.Context, nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return errors.New("node id is required")
	}
	node, err := s.store.GetNode(ctx, nodeID)
	if err != nil {
		return errors.New("node not found")
	}
	node.CertificateState = domain.CertificateRevoked
	if err := s.store.UpdateNode(ctx, node, WriteOptions{IfMatch: node.Revision, Actor: "system"}); err != nil {
		return err
	}
	// Revocation is an immediate data-plane security event as well as a
	// durable identity update. Ask an online node to tear down authenticated
	// AFDP sessions before closing its control stream. The direct send avoids a
	// race where cancelling the RPC wins before the asynchronous action broker
	// can flush its buffered message; an offline node is rejected on its next
	// mTLS control connection.
	var send func(*v1.ControllerMessage) error
	var cancel context.CancelFunc
	s.streamMu.Lock()
	if entry := s.streams[nodeID]; entry != nil {
		send, cancel = entry.send, entry.cancel
	}
	s.streamMu.Unlock()
	if send != nil {
		_ = send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Name: "reconnect"}}})
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

func recvWithContext(ctx context.Context, stream v1.Control_ConnectServer) (*v1.NodeMessage, error) {
	type result struct {
		message *v1.NodeMessage
		err     error
	}
	channel := make(chan result, 1)
	go func() { message, err := stream.Recv(); channel <- result{message: message, err: err} }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-channel:
		return value.message, value.err
	}
}

func (s *ControlServer) Health(context.Context, *emptypb.Empty) (*v1.Heartbeat, error) {
	return &v1.Heartbeat{SentAt: timestamppb.New(time.Now().UTC()), Healthy: true}, nil
}

func StartGRPC(ctx context.Context, config Config, store *Store) (net.Listener, *grpc.Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	server, err := NewControlServer(config, store)
	if err != nil {
		return nil, nil, err
	}
	tlsConfig, err := loadControlTLS(config)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", config.GRPCListen)
	if err != nil {
		return nil, nil, err
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.MaxRecvMsgSize(16<<20), grpc.MaxSendMsgSize(16<<20))
	v1.RegisterControlServer(grpcServer, server)
	go func() {
		<-ctx.Done()
		// Connect is intentionally long-lived. A graceful gRPC stop waits for
		// every bidirectional stream to finish, but a node may keep retrying
		// reads until it observes the transport close. Force-stop first so
		// cancellation is deterministic; Controller.Close uses the same path.
		grpcServer.Stop()
		_ = listener.Close()
	}()
	go func() { _ = grpcServer.Serve(listener) }()
	return listener, grpcServer, nil
}

func loadControlTLS(config Config) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(config.TLSCertPath, config.TLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load controller TLS certificate: %w", err)
	}
	caPEM, err := os.ReadFile(config.CACertPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("controller CA certificate is invalid")
	}
	// Enrollment is intentionally token + CSR authenticated and therefore has
	// no client certificate yet. The Connect RPC performs an explicit mTLS
	// identity check after enrollment; VerifyClientCertIfGiven keeps both RPCs
	// on one endpoint without weakening the node stream.
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven, NextProtos: []string{"h2", control.ControlALPN}}, nil
}

func protoRole(role v1.NodeRole) (string, error) {
	switch role {
	case v1.NodeRole_NODE_ROLE_GATEWAY:
		return domain.RoleGateway, nil
	case v1.NodeRole_NODE_ROLE_AGENT:
		return domain.RoleAgent, nil
	default:
		return "", errors.New("node role is required")
	}
}

func verifyPeerIdentity(ctx context.Context, nodeID string) error {
	certificate, err := peerCertificate(ctx)
	if err != nil {
		return err
	}
	for _, uri := range certificate.URIs {
		// Node certificates use the exact SPIFFE path
		// spiffe://asterferry/node/<node-id>. Do not use a generic suffix check:
		// /node/evil-<node-id> must not authenticate as <node-id>.
		if uri != nil && uri.Scheme == "spiffe" && uri.Host == "asterferry" && strings.TrimSuffix(uri.Path, "/") == "/node/"+nodeID {
			return nil
		}
	}
	if certificate.Subject.CommonName == nodeID {
		return nil
	}
	return errors.New("certificate identity does not match node")
}

func peerCertificate(ctx context.Context) (*x509.Certificate, error) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo.AuthInfo == nil {
		return nil, errors.New("mutual TLS identity is required")
	}
	switch tlsInfo := peerInfo.AuthInfo.(type) {
	case credentials.TLSInfo:
		if len(tlsInfo.State.PeerCertificates) == 0 {
			return nil, errors.New("mutual TLS identity is required")
		}
		return tlsInfo.State.PeerCertificates[0], nil
	case *credentials.TLSInfo:
		if tlsInfo == nil || len(tlsInfo.State.PeerCertificates) == 0 {
			return nil, errors.New("mutual TLS identity is required")
		}
		return tlsInfo.State.PeerCertificates[0], nil
	default:
		return nil, errors.New("mutual TLS identity is required")
	}
}

func certificateDER(pemBytes []byte) []byte {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	return block.Bytes
}

func jsonMarshalObserved(value domain.ObservedState) ([]byte, error) {
	return json.Marshal(value)
}
