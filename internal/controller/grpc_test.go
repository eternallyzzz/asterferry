package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"asterferry/internal/domain"
	"asterferry/internal/node"
)

type closeProbeGRPC struct {
	stopped  bool
	graceful bool
}

func (p *closeProbeGRPC) Stop()         { p.stopped = true }
func (p *closeProbeGRPC) GracefulStop() { p.graceful = true }

func TestControllerCloseForcesGRPCStop(t *testing.T) {
	probe := &closeProbeGRPC{}
	repositories, err := openTestRepositories(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{Repositories: repositories, grpcServer: probe}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if !probe.stopped {
		t.Fatal("controller close did not stop the gRPC server")
	}
	if probe.graceful {
		t.Fatal("controller close unexpectedly waited for a graceful gRPC stop")
	}
}

func TestEnrollmentOverGRPC(t *testing.T) {
	root := t.TempDir()
	result, err := Init(context.Background(), InitOptions{Dir: root, GRPCAdvertise: "127.0.0.1:9443", Password: "a-very-long-admin-password"})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := LoadOrCreateMasterKey(result.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := OpenControllerRepositoriesWithConfig(result.Config, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	store := repositories.Resources
	if err := store.CreateNode(context.Background(), domain.Node{ID: "agent-grpc", Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateEnrollmentToken(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	config := result.Config
	config.GRPCListen = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, grpcServer, err := StartGRPC(ctx, config, repositories)
	if err != nil {
		t.Fatal(err)
	}
	defer grpcServer.Stop()
	defer listener.Close()
	bootstrap, err := node.Enroll(context.Background(), node.EnrollOptions{ControllerAddress: listener.Addr().String(), Token: token, NodeID: "agent-grpc", CAPath: filepath.Join(root, "ca", "ca.crt"), CachePath: filepath.Join(root, "agent.cache")})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.NodeID != "agent-grpc" || bootstrap.CertificatePEM == "" {
		t.Fatalf("unexpected bootstrap: %#v", bootstrap)
	}
}
