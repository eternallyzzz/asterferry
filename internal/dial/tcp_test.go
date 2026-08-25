package dial

import (
	"context"
	"net"
	"testing"
	"time"
)

func closedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func TestTCPHappyEyeballsUsesFirstSuccessfulAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	conn, err := TCP(context.Background(), []string{closedTCPAddress(t), listener.Addr().String()}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case peer := <-accepted:
		_ = peer.Close()
	case <-time.After(time.Second):
		t.Fatal("listener did not observe the winning connection")
	}
}

func TestTCPReportsFailuresAndCancellation(t *testing.T) {
	if _, err := TCP(context.Background(), nil, time.Second); err == nil {
		t.Fatal("empty address list should fail")
	}
	if _, err := TCP(context.Background(), []string{closedTCPAddress(t), closedTCPAddress(t)}, 50*time.Millisecond); err == nil {
		t.Fatal("all failed addresses should return an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TCP(ctx, []string{closedTCPAddress(t)}, time.Second); err != context.Canceled {
		t.Fatalf("canceled dial error = %v", err)
	}
}
