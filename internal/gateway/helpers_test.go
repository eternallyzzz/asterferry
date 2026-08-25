package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"asterferry/internal/cluster"
	"asterferry/internal/config"
	"asterferry/internal/transport"
)

func configuredGatewayForHelpers() *Gateway {
	g := testGatewayRuntime()
	g.nodeID = "gw-a"
	g.cfg = &config.GatewayOptions{
		Management: config.ManagementOptions{Listen: "127.0.0.1:0"},
		Shutdown:   config.ShutdownOptions{GracePeriod: time.Second},
		Transport:  config.TransportConfig{HandshakeTimeoutSec: 1},
		Limits: config.Limits{
			MaxAgents:              4,
			MaxConnectionsPerAgent: 2,
			MaxStreamsPerAgent:     4,
			MaxFrameBytes:          4096,
			MaxRecordBytes:         1024,
			MaxUDPBytes:            512,
			UDPIdleTimeoutSec:      1,
		},
		Obfuscation: config.ObfuscationConfig{ProxyProfile: config.ProfileStandard, MaxPaddingBytes: 64},
		Gateway:     config.GatewayConfig{Listen: "127.0.0.1:4433"},
		StreamLimit: 4,
	}
	g.owners = cluster.NewLocalOwnerStore()
	g.sessions = newSessionRegistry(g.nodeID, g.owners)
	g.mappings = newMappingManager(g)
	g.acl = map[string]*credential{
		"edge": {
			token: []byte("01234567890123456789012345678901"),
			tcp:   []config.PortRange{{Min: 1, Max: 65535}},
			udp:   []config.PortRange{{Min: 1, Max: 65535}},
		},
	}
	return g
}

func gatewayHelperSession(g *Gateway, capabilities ...transport.Capability) *Session {
	ctx, cancel := context.WithCancel(g.ctx)
	return &Session{
		gateway:   g,
		agentID:   "edge",
		sessionID: "session-a",
		caps:      capabilities,
		limits:    transport.Limits{MaxFrameBytes: 4096, MaxRecordBytes: 1024, MaxUDPBytes: 512, MaxStreams: 4},
		ctx:       ctx,
		cancel:    cancel,
		streamSem: make(chan struct{}, 1),
		connSem:   make(chan struct{}, 1),
	}
}

func TestGatewayHelpersAndSessionAdmission(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil gateway config should fail")
	}
	if _, err := New(&config.GatewayOptions{Cluster: config.ClusterOptions{NodeID: "bad/node"}}); err == nil {
		t.Fatal("invalid node id should fail")
	}
	g := configuredGatewayForHelpers()
	state, ok := g.Status().(status)
	if !ok || state.Mode != config.RoleGateway || state.NodeID != "gw-a" || state.Ready {
		t.Fatalf("gateway status = %#v", g.Status())
	}
	if !allowedPort("tcp", 443, g.acl["edge"]) || !allowedPort("udp", 53, g.acl["edge"]) || allowedPort("tcp", 0, g.acl["edge"]) {
		t.Fatal("gateway ACL port checks are incorrect")
	}
	if mappingKey("tcp", config.DefaultReverseGatewayBind, 443) != "tcp:127.0.0.1:443" {
		t.Fatal("mapping key format changed")
	}
	if errorKind(context.Canceled) != "canceled" || errorKind(errors.New("boom")) == "" {
		t.Fatal("gateway error kind classification failed")
	}

	sess := gatewayHelperSession(g, transport.CapabilityErrorsV1, transport.CapabilityLimitsV1)
	if !sess.acquireStream() || sess.acquireStream() {
		t.Fatal("stream admission did not enforce its limit")
	}
	sess.releaseStream()
	release, ok := sess.acquireConnection()
	if !ok {
		t.Fatal("connection admission should succeed")
	}
	if _, ok := sess.acquireConnection(); ok {
		t.Fatal("connection admission overbooked")
	}
	release()
	if !sess.hasCapability(transport.CapabilityErrorsV1) || sess.hasCapability(transport.CapabilityReverseTCP) {
		t.Fatal("capability lookup is incorrect")
	}
	if got := sess.maxFrame(); got != 4096 || sess.maxRecord() != 1024 || sess.maxUDP() != 512 || sess.maxPadding() != 64 {
		t.Fatalf("session limits = %d/%d/%d/%d", got, sess.maxRecord(), sess.maxUDP(), sess.maxPadding())
	}
	if _, err := sess.relayProfile("unsupported"); err == nil {
		t.Fatal("unsupported relay profile should fail")
	}
	if owner := sess.owner(g.nodeID); owner.AgentID != "edge" || owner.SessionID != "session-a" || owner.NodeID != "gw-a" {
		t.Fatalf("session owner = %#v", owner)
	}
	sess.cancel()

	var frame bytes.Buffer
	sendOpenError(&frame, 3, transport.ErrorPolicyDenied, "denied", false, 4096)
	decoded, err := transport.ReadFrame(&frame, 4096)
	if err != nil || decoded.Type != transport.TypeOpenError {
		t.Fatalf("open error frame = %#v, err=%v", decoded, err)
	}
	frame.Reset()
	sendProtocolError(&frame, 4, transport.ErrorInternal, "internal", true, 4096)
	decoded, err = transport.ReadFrame(&frame, 4096)
	if err != nil || decoded.Type != transport.TypeError {
		t.Fatalf("protocol error frame = %#v, err=%v", decoded, err)
	}
}

func TestMappingManagerRegisterDrainAndClose(t *testing.T) {
	g := configuredGatewayForHelpers()
	manager := g.mappings.(*mappingManager)
	sess := gatewayHelperSession(g, transport.CapabilityErrorsV1, transport.CapabilityLimitsV1, transport.CapabilityReverseTCP, transport.CapabilityReverseUDP, transport.CapabilityRelayBalanced)
	port := freeGatewayTCPPort(t)
	result := manager.Register(sess, []transport.TunnelRegistration{{Name: "web", Protocol: "tcp", GatewayPort: uint16(port), Profile: config.ProfileStandard}})
	if result.Error != nil || manager.Count() != 1 {
		t.Fatalf("mapping registration = %#v count=%d", result, manager.Count())
	}
	item := manager.items[mappingKey("tcp", config.DefaultReverseGatewayBind, uint16(port))]
	if item == nil || item.Ownership().AgentID != "edge" {
		t.Fatal("registered mapping ownership is missing")
	}
	manager.BeginDrain()
	if result := manager.Register(sess, nil); result.Error == nil {
		t.Fatal("draining manager accepted a new registration")
	}
	manager.CloseAll()
	if manager.Count() != 0 {
		t.Fatal("CloseAll left mappings registered")
	}

	var nilManager *mappingManager
	if result := nilManager.Register(sess, nil); result.Error == nil {
		t.Fatal("nil mapping manager should fail registration")
	}
}

func TestMappingManagerRejectsInvalidRegistration(t *testing.T) {
	base := func() (*mappingManager, *Session) {
		g := configuredGatewayForHelpers()
		return g.mappings.(*mappingManager), gatewayHelperSession(g, transport.CapabilityErrorsV1, transport.CapabilityLimitsV1)
	}
	cases := []struct {
		name string
		spec []transport.TunnelRegistration
	}{
		{"empty-name", []transport.TunnelRegistration{{Protocol: "tcp", GatewayPort: 80, Profile: config.ProfileStandard}}},
		{"duplicate-name", []transport.TunnelRegistration{{Name: "same", Protocol: "tcp", GatewayPort: 80, Profile: config.ProfileStandard}, {Name: "same", Protocol: "tcp", GatewayPort: 81, Profile: config.ProfileStandard}}},
		{"unsupported-protocol", []transport.TunnelRegistration{{Name: "bad", Protocol: "icmp", GatewayPort: 80, Profile: config.ProfileStandard}}},
		{"missing-capability", []transport.TunnelRegistration{{Name: "reverse", Protocol: "tcp", GatewayPort: 80, Profile: config.ProfileStandard}}},
		{"bad-profile", []transport.TunnelRegistration{{Name: "profile", Protocol: "tcp", GatewayPort: 80, Profile: "unsupported"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, sess := base()
			result := manager.Register(sess, tc.spec)
			if result.Error == nil {
				t.Fatal("invalid mapping registration succeeded")
			}
			manager.CloseAll()
		})
	}
}

func freeGatewayTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
