package controller

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"asterferry/internal/domain"
	nodepkg "asterferry/internal/node"
)

func TestDecommissionNodeKeepsBusinessResourcesAndBlocksScheduling(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "old-serial"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "service", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 28080, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	decommissioned, err := store.DecommissionNode(ctx, "agent", WriteOptions{IfMatch: 1, Actor: "admin", IdempotencyKey: "decommission-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if decommissioned.Enabled || decommissioned.CertificateState != domain.CertificateDecommissioned {
		t.Fatalf("decommissioned node = %#v", decommissioned)
	}
	if _, err := store.GetService(ctx, "service"); err != nil {
		t.Fatalf("service was removed with its node: %v", err)
	}
	if _, err := store.GetAgentSpec(ctx, "agent"); err != nil {
		t.Fatalf("node behavior spec was removed with its node: %v", err)
	}
	if nodes, err := store.ListNodes(ctx, string(domain.NodeSpecAgent)); err != nil || len(nodes) != 1 || nodes[0].CertificateState != domain.CertificateDecommissioned {
		t.Fatalf("decommissioned node projection = %#v, err=%v", nodes, err)
	}
	if _, _, err := store.CreateNodeEnrollmentToken(ctx, "agent", EnrollmentTTL); err != nil {
		t.Fatalf("replacement token was not allowed: %v", err)
	}
}

func TestDecommissionedNodeCanBeReEnrolledWithOneTimeToken(t *testing.T) {
	root := t.TempDir()
	initResult, err := Init(context.Background(), InitOptions{Dir: root, GRPCAdvertise: "controller.example.com:9443", Password: "a-very-long-admin-password"})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := LoadOrCreateMasterKey(initResult.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := OpenControllerRepositoriesWithConfig(initResult.Config, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	store := repositories.Resources
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "replacement", Name: "replacement", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "old-serial"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecommissionNode(ctx, "replacement", WriteOptions{IfMatch: 1}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateNodeEnrollmentToken(ctx, "replacement", EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := nodepkg.GenerateCSR("replacement")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := store.IssueNodeCertificate(ctx, initResult.Config, token, "replacement", csr)
	if err != nil {
		t.Fatal(err)
	}
	if certificate.Serial == "" {
		t.Fatal("replacement enrollment returned an empty certificate serial")
	}
	updated, err := store.GetNode(ctx, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.CertificateState != domain.CertificateActive || updated.CertificateSerial != certificate.Serial || updated.CertificateSerial == "old-serial" {
		t.Fatalf("replacement node state = %#v", updated)
	}
	if _, err := store.IssueNodeCertificate(ctx, initResult.Config, token, "replacement", csr); !errors.Is(err, ErrEnrollmentTokenUsed) {
		t.Fatalf("replayed replacement token error = %v, want ErrEnrollmentTokenUsed", err)
	}
}

func TestDecommissionedNodeCannotBeReenabledByNodeUpdate(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "terminal", Name: "terminal", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "serial"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecommissionNode(ctx, "terminal", WriteOptions{IfMatch: 1}); err != nil {
		t.Fatal(err)
	}
	node, err := store.GetNode(ctx, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	node.Enabled = true
	if err := store.UpdateNode(ctx, node, WriteOptions{IfMatch: node.Revision}); err == nil || !strings.Contains(err.Error(), "replacement enrollment") {
		t.Fatalf("manual decommissioned-node re-enable error = %v", err)
	}
}

func TestPurgeNodeRequiresDecommissionAndRemovesNodeOwnedState(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{
		ID: "purge-me", Name: "purge me", Labels: map[string]string{"env": "test"},
		Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "serial",
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "purge-me", PublicEndpoints: []string{"gateway.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := store.PurgeNode(ctx, "purge-me", WriteOptions{IfMatch: 1}); err == nil || !strings.Contains(err.Error(), "must be decommissioned") {
		t.Fatalf("active node purge error = %v, want decommission precondition", err)
	}
	if _, err := store.GetNode(ctx, "purge-me"); err != nil {
		t.Fatalf("active node disappeared after rejected purge: %v", err)
	}

	node, err := store.GetNode(ctx, "purge-me")
	if err != nil {
		t.Fatal(err)
	}
	decommissioned, err := store.DecommissionNode(ctx, "purge-me", WriteOptions{IfMatch: node.Revision, Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PurgeNode(ctx, "purge-me", WriteOptions{IfMatch: decommissioned.Revision, Actor: "admin"}); err != nil {
		t.Fatalf("purge decommissioned node: %v", err)
	}
	if _, err := store.GetNode(ctx, "purge-me"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("purged node lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.GetGatewaySpec(ctx, "purge-me"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("purged node spec lookup error = %v, want sql.ErrNoRows", err)
	}
	audit, err := store.ListAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundPurgeAudit := false
	for _, record := range audit {
		if record.Action == "purge" && record.Resource == "node" && record.ResourceID == "purge-me" {
			foundPurgeAudit = true
			break
		}
	}
	if !foundPurgeAudit {
		t.Fatalf("purge audit event missing: %#v", audit)
	}
}

func TestDeleteNodeAPIDecommissionsWithoutDeletingService(t *testing.T) {
	root := t.TempDir()
	initResult, err := Init(context.Background(), InitOptions{
		Dir: root, Password: "a-very-long-admin-password",
		GRPCAdvertise: "controller.example.com:9443",
	})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := LoadOrCreateMasterKey(initResult.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := OpenControllerRepositoriesWithConfig(initResult.Config, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	store := repositories.Resources
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Name: "gateway", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "gateway-serial"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "agent-serial"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gateway", PublicEndpoints: []string{"gateway.example:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 28080, Max: 28080}}}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent", GatewayID: "gateway"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "service", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 28080, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAssignment(ctx, domain.Assignment{ID: "agent-gateway", GatewayID: "gateway", AgentID: "agent", ServiceIDs: []string{"service"}, Bindings: []domain.Binding{{ServiceID: "service", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 28080}}, Generation: 1, State: domain.AssignmentPending}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := store.CreateAPIToken(ctx, initResult.Admin.ID, "node-lifecycle-api-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(initResult.Config, repositories)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	node, err := store.GetNode(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/agent", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("If-Match", strconv.FormatInt(node.Revision, 10))
	request.Header.Set("Idempotency-Key", "decommission-agent-api")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("node DELETE response = %d, body=%s", response.Code, response.Body.String())
	}
	decommissioned, err := store.GetNode(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if decommissioned.Enabled || decommissioned.CertificateState != domain.CertificateDecommissioned {
		t.Fatalf("node DELETE state = %#v", decommissioned)
	}
	if _, err := store.GetService(ctx, "service"); err != nil {
		t.Fatalf("node DELETE removed service: %v", err)
	}
	assignment, err := store.GetAssignment(ctx, "agent-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.State != domain.AssignmentDegraded || len(assignment.ServiceIDs) != 1 || len(assignment.Bindings) != 0 {
		t.Fatalf("node DELETE assignment quarantine = %#v", assignment)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/nodes/agent/actions/purge", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("If-Match", strconv.FormatInt(decommissioned.Revision, 10))
	request.Header.Set("Idempotency-Key", "purge-agent-api")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("dependent node purge response = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := store.GetNode(ctx, "agent"); err != nil {
		t.Fatalf("dependent node was purged despite conflict: %v", err)
	}
}

func TestReplacementEnrollmentRepairsDependentAgentAssignments(t *testing.T) {
	root := t.TempDir()
	initResult, err := Init(context.Background(), InitOptions{
		Dir: root, Password: "a-very-long-admin-password",
		GRPCAdvertise: "controller.example.com:9443",
	})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := LoadOrCreateMasterKey(initResult.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := OpenControllerRepositoriesWithConfig(initResult.Config, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	store := repositories.Resources
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Name: "gateway", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "gateway-old"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true, CertificateState: domain.CertificateActive, CertificateSerial: "agent-serial"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gateway", PublicEndpoints: []string{"gateway.example:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 28080, Max: 28080}}}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent", GatewayID: "gateway"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "service", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 28080, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if assignments, err := scheduler.ScheduleAgent(ctx, "agent", WriteOptions{}); err != nil || len(assignments) != 1 {
		t.Fatalf("initial assignment = %#v, err=%v", assignments, err)
	}
	if _, err := store.DecommissionNode(ctx, "gateway", WriteOptions{IfMatch: 1}); err != nil {
		t.Fatal(err)
	}
	degraded, err := store.GetAssignment(ctx, "agent-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if degraded.State != domain.AssignmentDegraded {
		t.Fatalf("decommissioned gateway assignment state = %#v", degraded)
	}

	changes, unsubscribe := store.ChangeBus().SubscribeResourceChanges()
	defer unsubscribe()
	token, _, err := store.CreateNodeEnrollmentToken(ctx, "gateway", EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := nodepkg.GenerateCSR("gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueNodeCertificate(ctx, initResult.Config, token, "gateway", csr); err != nil {
		t.Fatal(err)
	}
	var change ResourceChange
	select {
	case change = <-changes:
	case <-time.After(time.Second):
		t.Fatal("replacement enrollment did not notify dependent nodes")
	}
	seen := map[string]bool{}
	for _, nodeID := range change.NodeIDs {
		seen[nodeID] = true
	}
	if !seen["gateway"] || !seen["agent"] {
		t.Fatalf("replacement enrollment change = %#v, want gateway and agent", change)
	}
	if _, err := scheduler.ReconcileAssignmentsForAgents(ctx, change.NodeIDs...); err != nil {
		t.Fatal(err)
	}
	repaired, err := store.GetAssignment(ctx, "agent-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if repaired.State == domain.AssignmentDegraded || repaired.GatewayID != "gateway" || len(repaired.Bindings) != 1 {
		t.Fatalf("repaired assignment = %#v", repaired)
	}
}
