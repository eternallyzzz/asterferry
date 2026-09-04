package controller

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"asterferry/internal/domain"
	moderncsqlite "modernc.org/sqlite"
)

func TestListResourceProjectionsUseJoinedQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "agent-a", Name: "agent-a", Enabled: true},
		{ID: "agent-b", Name: "agent-b", Enabled: true},
		{ID: "gateway-a", Name: "gateway-a", Enabled: true},
		{ID: "gateway-b", Name: "gateway-b", Enabled: true},
		{ID: "unconfigured", Name: "unconfigured", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	for _, nodeID := range []string{"agent-a", "agent-b"} {
		if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: nodeID}, WriteOptions{}); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	for _, nodeID := range []string{"gateway-a", "gateway-b"} {
		if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: nodeID, PublicEndpoints: []string{nodeID + ".example:4433"}}, WriteOptions{}); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var selects atomic.Int64
	db := sql.OpenDB(countingSQLiteConnector{
		driver: &countingSQLiteDriver{inner: &moderncsqlite.Driver{}, selects: &selects},
		name:   sqliteDSN(path),
	})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	listStore := &ResourceRepository{databaseHandle: &databaseHandle{db: db, dialect: sqliteDialect{}}, changes: newChangeBus()}

	nodes, err := listStore.ListNodes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 5 || nodes[0].ID != "agent-a" || nodes[0].SpecKind != domain.NodeSpecAgent || nodes[4].SpecKind != "" {
		t.Fatalf("all nodes = %#v", nodes)
	}
	if got := selects.Load(); got > 2 {
		t.Fatalf("ListNodes all-node SELECT count = %d, want a bounded set query count", got)
	}

	selects.Store(0)
	nodes, err = listStore.ListNodes(ctx, string(domain.NodeSpecGateway))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != "gateway-a" || nodes[1].ID != "gateway-b" {
		t.Fatalf("gateway nodes = %#v", nodes)
	}
	if got := selects.Load(); got > 2 {
		t.Fatalf("ListNodes filtered SELECT count = %d, want a bounded set query count", got)
	}

	selects.Store(0)
	gateways, err := listStore.ListGatewayViews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gateways) != 2 || gateways[0].Node.ID != "gateway-a" || gateways[1].Spec == nil || gateways[1].Spec.NodeID != "gateway-b" {
		t.Fatalf("gateway views = %#v", gateways)
	}
	if got := selects.Load(); got > 10 {
		t.Fatalf("ListGatewayViews SELECT count = %d, want a bounded set query count", got)
	}

	selects.Store(0)
	agents, err := listStore.ListAgentViews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].Node.ID != "agent-a" || agents[1].Spec == nil || agents[1].Spec.NodeID != "agent-b" {
		t.Fatalf("agent views = %#v", agents)
	}
	if got := selects.Load(); got > 10 {
		t.Fatalf("ListAgentViews SELECT count = %d, want a bounded set query count", got)
	}

	selects.Store(0)
	specs, err := listStore.ListNodeSpecs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 4 || specs[0].NodeID != "agent-a" || specs[2].NodeID != "gateway-a" {
		t.Fatalf("node specs = %#v", specs)
	}
	if got := selects.Load(); got > 14 {
		t.Fatalf("ListNodeSpecs SELECT count = %d, want bounded aggregate loads", got)
	}
}

type countingSQLiteDriver struct {
	inner   *moderncsqlite.Driver
	selects *atomic.Int64
}

func (d *countingSQLiteDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingSQLiteConn{Conn: conn, selects: d.selects}, nil
}

type countingSQLiteConn struct {
	driver.Conn
	selects *atomic.Int64
}

func (c *countingSQLiteConn) Prepare(query string) (driver.Stmt, error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT") {
		c.selects.Add(1)
	}
	return c.Conn.Prepare(query)
}

type countingSQLiteConnector struct {
	driver *countingSQLiteDriver
	name   string
}

func (c countingSQLiteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.name)
}

func (c countingSQLiteConnector) Driver() driver.Driver { return c.driver }
