package controller

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"google.golang.org/grpc"
)

func TestIsExpectedServerStopError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "http", err: http.ErrServerClosed, want: true},
		{name: "network", err: net.ErrClosed, want: true},
		{name: "grpc", err: grpc.ErrServerStopped, want: true},
		{name: "wrapped grpc", err: fmt.Errorf("server stopped: %w", grpc.ErrServerStopped), want: true},
		{name: "unexpected", err: errors.New("accept failed"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isExpectedServerStopError(test.err); got != test.want {
				t.Fatalf("isExpectedServerStopError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
