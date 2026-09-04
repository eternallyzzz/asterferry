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
	// Revocation is an immediate data-plane security event as well as a
	// durable identity update. Ask an online node to tear down authenticated
	// AFDP sessions before closing its control stream. The direct send avoids a
	// race where cancelling the RPC wins before the asynchronous action broker
	// can flush its buffered message; an offline node is rejected on its next
	// mTLS control connection.
	var send func(*v1.ControllerMessage) error
	s.streamMu.Lock()
	if current := s.streams[nodeID]; current != nil {
		send = current.send
	}
	s.streamMu.Unlock()
	if send != nil {
		if err := send(&v1.ControllerMessage{Body: &v1.ControllerMessage_Action{Action: &v1.Action{Name: "reconnect"}}}); err != nil {
			slog.Default().Error("failed to deliver node revocation action", "node_id", nodeID, "error", err)
			if eventErr := s.resources.RecordEvent(context.Background(), "system", "", "action_delivery_failed", "reconnect action could not be delivered", nodeID, map[string]string{"action": "reconnect"}); eventErr != nil {
				slog.Default().Error("failed to record node revocation delivery event", "node_id", nodeID, "error", eventErr)
			}
		}
	} else {
		if eventErr := s.resources.RecordEvent(context.Background(), "system", "", "action_not_delivered", "reconnect action queued for next connection", nodeID, map[string]string{"action": "reconnect"}); eventErr != nil {
			slog.Default().Error("failed to record node revocation queued event", "node_id", nodeID, "error", eventErr)
		}
	}
	// The stream lookup above is only a snapshot. A node may complete a new
	// Connect handshake between that lookup and the action send. Re-read the
	// map while holding the same mutex used by Connect and cancel whichever
	// stream is current; otherwise revocation can leave the replacement stream
	// alive until its next heartbeat.
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
