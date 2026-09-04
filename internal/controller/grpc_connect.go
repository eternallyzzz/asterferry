package controller

import (
	controlwire "asterferry/internal/controlwire"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (s *ControlServer) Connect(stream v1.Control_ConnectServer) (returnErr error) {
	defer func() {
		if s.metrics != nil {
			code := codes.OK.String()
			if returnErr != nil {
				code = status.Code(returnErr).String()
			}
			s.metrics.observeGRPC("Connect", code)
		}
	}()
	if allowed, retry := s.connectLimiter.allow(peerAddressKey(stream.Context())); !allowed {
		return status.Errorf(codes.ResourceExhausted, "control connection rate limit exceeded; retry after %s", retry.Round(time.Second))
	}
	connectSlotHeld := false
	select {
	case s.connectSlots <- struct{}{}:
		connectSlotHeld = true
		defer func() {
			if connectSlotHeld {
				<-s.connectSlots
			}
		}()
	default:
		return status.Error(codes.ResourceExhausted, "control connection capacity is temporarily exhausted")
	}
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
	if hello.GetSchemaVersion() != domain.CurrentControlProtocolVersion {
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
	// Authenticate the certificate identity before consulting the database. This
	// keeps unknown node IDs, disabled nodes and behavior mismatches from becoming
	// an externally observable lookup oracle.
	if err := verifyPeerIdentity(stream.Context(), hello.GetNodeId()); err != nil {
		return status.Error(codes.PermissionDenied, "control stream authentication failed")
	}
	node, err := s.store.GetNode(stream.Context(), hello.GetNodeId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status.Error(codes.PermissionDenied, "control stream authentication failed")
		}
		slog.Default().Error("control stream node lookup failed", "node_id", hello.GetNodeId(), "error", err)
		return status.Error(codes.Unavailable, "controller storage is temporarily unavailable")
	}
	if !node.Enabled || node.CertificateState != domain.CertificateActive {
		return status.Error(codes.PermissionDenied, "control stream authentication failed")
	}
	if certificate, certErr := peerCertificate(stream.Context()); certErr != nil || node.CertificateSerial == "" || !strings.EqualFold(certificate.SerialNumber.Text(16), node.CertificateSerial) {
		// A rotated certificate keeps the same node identity but invalidates the
		// previous serial immediately. This closes a stream opened with an old
		// certificate even before its natural expiry.
		return status.Error(codes.PermissionDenied, "control stream authentication failed")
	}
	// The expensive pre-auth admission slot is no longer needed after the
	// certificate and current serial have been checked.
	<-s.connectSlots
	connectSlotHeld = false
	// Subscribe before materializing the initial snapshot. A resource write
	// racing this handshake is coalesced into the buffered notification instead
	// of waiting for a periodic poll.
	snapshotChanges, unsubscribeSnapshots := s.store.ChangeBus().SubscribeSnapshotChanges(hello.GetNodeId())
	defer unsubscribeSnapshots()
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
	if s.metrics != nil {
		s.metrics.streams.Inc()
		defer s.metrics.streams.Dec()
	}
	defer func() {
		wasCurrent := false
		s.streamMu.Lock()
		// Do not remove a newer stream that replaced this one while it was
		// unwinding after cancellation.
		if current := s.streams[hello.GetNodeId()]; current == entry {
			delete(s.streams, hello.GetNodeId())
			wasCurrent = true
		}
		s.streamMu.Unlock()
		cancel()
		if wasCurrent {
			markCtx, markCancel := context.WithTimeout(context.Background(), time.Second)
			if err := s.store.MarkRuntimeConnectionsUnknown(markCtx, hello.GetNodeId(), time.Now().UTC()); err != nil {
				slog.Default().Warn("failed to mark runtime connections unknown", "node_id", hello.GetNodeId(), "error", err)
			}
			markCancel()
		}
	}()
	actionCh, unsubscribeActions := s.store.ChangeBus().SubscribeActions(hello.GetNodeId())
	defer unsubscribeActions()
	// A node's Hello is sent before the Controller can authenticate the
	// bidirectional RPC.  Send an explicit readiness marker only after all
	// certificate, behavior and current-serial checks above have succeeded; the
	// node uses it to lift a reconnect/revocation drain safely.
	readyPayload := []byte(nil)
	if hasCapability(hello.GetCapabilities(), "runtime-telemetry-v1") {
		readyPayload, _ = json.Marshal(map[string]any{"capabilities": []string{"runtime-telemetry-v1", "runtime-control-v1"}})
	}
	if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Name: "session_ready", PayloadJson: readyPayload}}}); err != nil {
		return err
	}
	// Runtime controls are intentionally process-local and are never replayed
	// from desired state. If an Admin disabled the feature while this Node was
	// offline, clear any limit that survived in the Node process before it can
	// serve a newly authenticated control session.
	if hasCapability(hello.GetCapabilities(), "runtime-control-v1") {
		enabled, settingsErr := s.store.AdvancedOperationsEnabled(connectionCtx)
		if settingsErr != nil {
			slog.Default().Error("runtime operation setting lookup failed", "node_id", hello.GetNodeId(), "error", settingsErr)
			return status.Error(codes.Internal, "load runtime operation settings failed")
		}
		if !enabled {
			if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Name: "clear_runtime_controls"}}}); err != nil {
				return err
			}
		}
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
			if err := s.sendSnapshotForWire(connectionCtx, cancel, snapshotRecord, send, &lastSent); err != nil {
				if connectionCtx.Err() != nil {
					return connectionCtx.Err()
				}
				return status.Error(codes.Internal, "prepare desired snapshot for wire failed")
			}
		}
	}
	go s.pushSnapshots(connectionCtx, cancel, hello.GetNodeId(), send, &lastSent, snapshotChanges)
	type recvResult struct {
		message *v1.NodeMessage
		err     error
	}
	recvCh := make(chan recvResult, 1)
	go func() {
		defer close(recvCh)
		for {
			message, err := stream.Recv()
			select {
			case recvCh <- recvResult{message: message, err: err}:
				if err != nil {
					return
				}
			case <-connectionCtx.Done():
				return
			}
		}
	}()
	for {
		var result recvResult
		var ok bool
		select {
		case <-connectionCtx.Done():
			return connectionCtx.Err()
		case result, ok = <-recvCh:
			if !ok {
				if connectionCtx.Err() != nil {
					return connectionCtx.Err()
				}
				return errors.New("control stream receiver stopped")
			}
		}
		message, err := result.message, result.err
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
			if currentNodeErr != nil {
				if !errors.Is(currentNodeErr, sql.ErrNoRows) {
					slog.Default().Error("heartbeat node lookup failed", "node_id", hello.GetNodeId(), "error", currentNodeErr)
					return status.Error(codes.Unavailable, "controller storage is temporarily unavailable")
				}
				return status.Error(codes.PermissionDenied, "node is not enrolled")
			}
			if currentPeerErr != nil || currentNode.CertificateState != domain.CertificateActive || currentNode.CertificateSerial == "" || !strings.EqualFold(currentPeer.SerialNumber.Text(16), currentNode.CertificateSerial) {
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
			observed := domain.ObservedState{SchemaVersion: domain.CurrentControlProtocolVersion, NodeID: hello.GetNodeId(), AppliedGeneration: heartbeat.GetAppliedGeneration(), Healthy: heartbeat.GetHealthy(), Degraded: !heartbeat.GetHealthy(), ObservedAt: time.Now().UTC()}
			if heartbeat.GetSentAt() != nil {
				if err := heartbeat.GetSentAt().CheckValid(); err != nil {
					return status.Error(codes.InvalidArgument, "heartbeat timestamp is invalid")
				}
				observed.ObservedAt = heartbeat.GetSentAt().AsTime().UTC()
			}
			if observed.ObservedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
				return status.Error(codes.InvalidArgument, "heartbeat timestamp is too far in the future")
			}
			var previousObserved domain.ObservedState
			hasPreviousObserved := false
			if previous, previousErr := s.store.GetObserved(stream.Context(), hello.GetNodeId()); previousErr == nil {
				previousObserved = previous
				hasPreviousObserved = true
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
			staleGeneration := hasPreviousObserved && observed.AppliedGeneration < previousObserved.AppliedGeneration
			if err := s.store.SaveObservedHeartbeat(stream.Context(), ObservedRecord{NodeID: hello.GetNodeId(), Generation: observed.AppliedGeneration, Document: document, UpdatedAt: observed.ObservedAt}); err != nil {
				return status.Error(codes.Internal, "save heartbeat state failed")
			}
			if staleGeneration {
				if err := s.store.RecordEvent(stream.Context(), "system", "", "stale_generation", "heartbeat reported an older applied generation", hello.GetNodeId(), map[string]string{"reported_generation": fmt.Sprint(observed.AppliedGeneration), "current_generation": fmt.Sprint(previousObserved.AppliedGeneration)}); err != nil {
					return status.Error(codes.Internal, "record stale heartbeat event failed")
				}
			}
			if s.metrics != nil {
				s.metrics.observeNode(hello.GetNodeId(), string(node.SpecKind), observed)
			}
		case message.GetObservedState() != nil:
			observed, decodeErr := controlwire.ObservedFromProto(message.GetObservedState())
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
			document, marshalErr := jsonMarshalObserved(observed)
			if marshalErr != nil {
				return status.Error(codes.Internal, "encode observed state failed")
			}
			if err := s.store.SaveObserved(stream.Context(), ObservedRecord{NodeID: hello.GetNodeId(), Generation: observed.AppliedGeneration, Document: document, UpdatedAt: time.Now().UTC()}); err != nil {
				return status.Error(codes.Internal, "save observed state failed")
			}
			if s.metrics != nil {
				s.metrics.observeNode(hello.GetNodeId(), string(node.SpecKind), observed)
			}
		case message.GetApplyResult() != nil:
			result := message.GetApplyResult()
			latest := snapshotRecord
			if current, currentErr := s.store.LoadSnapshot(stream.Context(), hello.GetNodeId()); currentErr == nil {
				latest = current
			} else if !errors.Is(currentErr, sql.ErrNoRows) {
				return status.Error(codes.Internal, "load desired snapshot failed")
			}
			// A participant may finish applying generation N after the other
			// participant has acknowledged N and the Controller has already
			// published generation N+1. That result is stale, not malformed: it
			// must not tear down the control stream or cause an endless reconnect
			// loop. The node will receive and apply the newer snapshot normally.
			if latest.NodeID != "" && result.GetGeneration() < latest.Generation {
				if err := s.store.RecordEvent(stream.Context(), "system", "", "stale_apply_result", "ignored apply result for an older desired generation", hello.GetNodeId(), map[string]string{"generation": fmt.Sprint(result.GetGeneration()), "latest_generation": fmt.Sprint(latest.Generation)}); err != nil {
					return status.Error(codes.Internal, "record stale apply result failed")
				}
				continue
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
				if event == nil || strings.TrimSpace(event.GetType()) == "" || len(event.GetType()) > 128 || len(event.GetMessage()) > 4096 || len(event.GetId()) > 128 || strings.ContainsAny(event.GetId(), "\x00\r\n") {
					return status.Error(codes.InvalidArgument, "event fields are invalid")
				}
				attributes := map[string]string{}
				maxAttributes := 128 << 10
				if event.GetType() == "runtime_snapshot" || event.GetType() == "runtime_connection" || event.GetType() == "runtime_action_result" {
					maxAttributes = 4 << 20
				}
				if len(event.GetAttributesJson()) > maxAttributes {
					return status.Error(codes.InvalidArgument, "event attributes are too large")
				}
				if strings.HasPrefix(event.GetType(), "runtime_") {
					if len(event.GetAttributesJson()) == 0 {
						return status.Error(codes.InvalidArgument, "runtime event attributes are required")
					}
					createdAt := time.Now().UTC()
					if event.GetCreatedAt() != nil {
						if err := event.GetCreatedAt().CheckValid(); err != nil {
							return status.Error(codes.InvalidArgument, "runtime event timestamp is invalid")
						}
						createdAt = event.GetCreatedAt().AsTime().UTC()
					}
					if err := s.store.RecordRuntimeEvent(stream.Context(), hello.GetNodeId(), event.GetId(), event.GetType(), event.GetAttributesJson(), createdAt); err != nil {
						slog.Default().Error("failed to record runtime event", "node_id", hello.GetNodeId(), "event_type", event.GetType(), "error", err)
						return status.Error(codes.Internal, "record runtime event failed")
					}
					continue
				}
				if len(event.GetAttributesJson()) > 0 {
					if err := json.Unmarshal(event.GetAttributesJson(), &attributes); err != nil {
						return status.Error(codes.InvalidArgument, "event attributes are invalid")
					}
				}
				if len(attributes) > 62 {
					return status.Error(codes.InvalidArgument, "event attributes are too numerous")
				}
				for key, value := range attributes {
					if len(key) > 128 || len(value) > 2048 || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsAny(value, "\x00\r\n") {
						return status.Error(codes.InvalidArgument, "event attributes are invalid")
					}
				}
				if err := s.store.RecordEvent(stream.Context(), hello.GetNodeId(), event.GetId(), event.GetType(), event.GetMessage(), hello.GetNodeId(), attributes); err != nil {
					slog.Default().Error("failed to record node event", "node_id", hello.GetNodeId(), "event_type", event.GetType(), "error", err)
					return status.Error(codes.Internal, "record node event failed")
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
			if currentNodeErr != nil {
				if !errors.Is(currentNodeErr, sql.ErrNoRows) {
					slog.Default().Error("certificate renewal node lookup failed", "node_id", hello.GetNodeId(), "error", currentNodeErr)
					return status.Error(codes.Unavailable, "controller storage is temporarily unavailable")
				}
				return status.Error(codes.PermissionDenied, "node is not enrolled")
			}
			if currentPeerErr != nil || currentNode.CertificateState != domain.CertificateActive || currentNode.CertificateSerial == "" || !strings.EqualFold(currentPeer.SerialNumber.Text(16), currentNode.CertificateSerial) {
				return status.Error(codes.PermissionDenied, "certificate serial is not current")
			}
			certificate, renewErr := s.store.RenewNodeCertificate(stream.Context(), s.config, hello.GetNodeId(), message.GetRenewCertificate().GetCsrDer())
			if renewErr != nil {
				if errors.Is(renewErr, ErrInvalidEnrollmentRequest) {
					return status.Error(codes.InvalidArgument, "renewal request is invalid")
				}
				if isCredentialError(renewErr) {
					return status.Error(codes.PermissionDenied, "node certificate renewal is not authorized")
				}
				slog.Default().Error("node certificate renewal failed", "node_id", hello.GetNodeId(), "error", renewErr)
				return status.Error(codes.Unavailable, "certificate service is temporarily unavailable")
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
