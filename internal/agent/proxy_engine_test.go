package agent

import (
	"context"
	"net"
	"testing"

	"asterferry/internal/config"
)

func TestProxyEngineRequiresHandler(t *testing.T) {
	if _, err := NewProxyEngine(ProxyEngineOptions{}); err == nil {
		t.Fatal("expected missing handler error")
	}
}

func TestProxyEngineStartRollbackAndClose(t *testing.T) {
	engine, err := NewProxyEngine(ProxyEngineOptions{
		Inbounds: []config.Inbound{
			{Tag: "first", Protocol: "http", Listen: "127.0.0.1:0"},
			{Tag: "invalid", Protocol: "http", Listen: "127.0.0.1:not-a-port"},
		},
		Handler: func(net.Conn, config.Inbound) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); err == nil {
		t.Fatal("expected partial-start failure")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal("close must be idempotent: ", err)
	}
	if err := engine.Start(context.Background()); err == nil {
		t.Fatal("closed engine must not restart")
	}
}
