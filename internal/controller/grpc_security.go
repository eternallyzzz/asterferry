package controller

import (
	controlwire "asterferry/internal/controlwire"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"log/slog"
	"os"
	"strings"
)

func (s *ControlServer) RevokeNode(ctx context.Context, nodeID string) error {
	if s.leadership != nil {
		if err := s.leadership.RequireLeader(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(nodeID) == "" {
		return errors.New("node id is required")
	}
	node, err := s.resources.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNodeNotEnrolled
		}
		return storageFailure("load node for revocation", err)
	}
	node.CertificateState = domain.CertificateRevoked
	if err := s.resources.UpdateNode(ctx, node, WriteOptions{IfMatch: node.Revision, Actor: "system"}); err != nil {
		return err
	}
	return s.deliverNodeAction(nodeID, "reconnect", "system")
}

// deliverNodeAction sends a security-sensitive action directly to the current
// stream. Runtime actions normally travel through the ChangeBus, but
// revocation and decommissioning must not wait behind a buffered subscription.
// The durable node state is authoritative if the node is offline or the send
// races a stream close; the next connection is rejected by Connect.
func (s *ControlServer) deliverNodeAction(nodeID, action, actor string) error {
	var send func(*v1.ControllerMessage) error
	s.streamMu.Lock()
	if current := s.streams[nodeID]; current != nil {
		send = current.send
	}
	s.streamMu.Unlock()
	if send != nil {
		if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Name: action}}}); err != nil {
			slog.Default().Error("failed to deliver node security action", "node_id", nodeID, "action", action, "error", err)
			if eventErr := s.resources.RecordEvent(context.Background(), actor, "", "action_delivery_failed", "node security action could not be delivered", nodeID, map[string]string{"action": action}); eventErr != nil {
				slog.Default().Error("failed to record node security action delivery event", "node_id", nodeID, "action", action, "error", eventErr)
			}
		}
	} else {
		if eventErr := s.resources.RecordEvent(context.Background(), actor, "", "action_not_delivered", "node security action will be enforced on the next connection", nodeID, map[string]string{"action": action}); eventErr != nil {
			slog.Default().Error("failed to record node security action queued event", "node_id", nodeID, "action", action, "error", eventErr)
		}
	}
	// Closing the stream is a fallback for older Nodes that do not know the
	// action. New Nodes exit through the action itself and observe the durable
	// state on any subsequent connection. Re-read the map after the send: a
	// node may have completed a new Connect handshake between the first lookup
	// and this point, and that replacement stream must be closed too.
	s.streamMu.Lock()
	if current := s.streams[nodeID]; current != nil {
		current.cancel()
	}
	s.streamMu.Unlock()
	return nil
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
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven, NextProtos: []string{"h2", controlwire.ControlALPN}}, nil
}

func verifyPeerIdentity(ctx context.Context, nodeID string) error {
	certificate, err := peerCertificate(ctx)
	if err != nil {
		return err
	}
	expected := domain.NodeIdentityURI(nodeID)
	for _, uri := range certificate.URIs {
		// Node certificates use the exact SPIFFE path
		// spiffe://asterferry/node/<node-id>. Do not use a generic suffix check:
		// /node/evil-<node-id> must not authenticate as <node-id>.
		if uri != nil && uri.Scheme == expected.Scheme && uri.Host == expected.Host && strings.TrimSuffix(uri.Path, "/") == expected.Path {
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
