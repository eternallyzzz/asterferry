package controller

import (
	v1 "asterferry/internal/controlwire/v1"
	"context"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"log/slog"
	"net"
	"sync"
	"time"
)

type ControlServer struct {
	v1.UnimplementedControlServer
	resources             *ResourceRepository
	runtime               *RuntimeRepository
	changes               *ChangeBus
	config                Config
	streams               map[string]*controlStream // node id -> active stream
	streamMu              sync.Mutex
	metrics               *ControllerMetrics
	enrollLimiter         *admissionLimiter
	connectLimiter        *admissionLimiter
	enrollSlots           chan struct{}
	connectSlots          chan struct{}
	leadership            *leadership
	unsubscribeLeadership func()
}

type controlStream struct {
	cancel context.CancelFunc
	// send is installed before the stream is published in streams. It lets
	// security-sensitive Controller operations (notably revocation) deliver a
	// reconnect action synchronously before cancelling the control RPC, rather
	// than relying on a best-effort buffered subscription racing the cancel.
	send func(*v1.ControllerMessage) error
}

func hasCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func NewControlServer(config Config, repositories *ControllerRepositories, metrics ...*ControllerMetrics) (*ControlServer, error) {
	if repositories == nil || repositories.Resources == nil || repositories.Runtime == nil || repositories.Changes == nil {
		return nil, errors.New("controller repositories are required")
	}
	resources, runtime, changes := repositories.Resources, repositories.Runtime, repositories.Changes
	if err := config.Validate(); err != nil {
		return nil, err
	}
	var controllerMetrics *ControllerMetrics
	if len(metrics) > 0 {
		controllerMetrics = metrics[0]
	}
	if controllerMetrics == nil {
		controllerMetrics = newControllerMetrics(resources.DatabaseDriver())
	}
	server := &ControlServer{
		resources: resources, runtime: runtime, changes: changes, config: config, streams: make(map[string]*controlStream), metrics: controllerMetrics,
		enrollLimiter:  newAdmissionLimiter(6, time.Minute, 4096),
		connectLimiter: newAdmissionLimiter(30, time.Minute, 4096),
		enrollSlots:    make(chan struct{}, 16),
		connectSlots:   make(chan struct{}, 256),
		leadership:     repositories.Resources.leadership,
	}
	if server.leadership != nil {
		server.unsubscribeLeadership = server.leadership.observe(func(event leadershipEvent) {
			if !event.Leader {
				server.Drain()
			}
		})
	}
	return server, nil
}

func (s *ControlServer) Health(context.Context, *emptypb.Empty) (response *v1.Heartbeat, returnErr error) {
	if s.metrics != nil {
		defer func() {
			code := codes.OK.String()
			if returnErr != nil {
				code = status.Code(returnErr).String()
			}
			s.metrics.observeGRPC("Health", code)
		}()
	}
	if s.leadership != nil {
		if err := s.leadership.RequireLeader(); err != nil {
			return nil, status.Error(codes.Unavailable, "controller is not the active leader")
		}
	}
	return &v1.Heartbeat{SentAt: timestamppb.New(time.Now().UTC()), Healthy: true}, nil
}

// Drain terminates all long-lived Node control streams. A transport close is
// intentionally used for leadership loss so Nodes treat takeover as a
// retryable outage rather than as a certificate revocation.
func (s *ControlServer) Drain() {
	if s == nil {
		return
	}
	s.streamMu.Lock()
	streams := make([]*controlStream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.streamMu.Unlock()
	for _, stream := range streams {
		if stream != nil && stream.cancel != nil {
			stream.cancel()
		}
	}
}

// Close releases the leadership observer held by the gRPC service. The
// observer otherwise keeps a stopped server reachable from the process-local
// leadership object until the Controller itself exits.
func (s *ControlServer) Close() {
	if s == nil {
		return
	}
	if s.unsubscribeLeadership != nil {
		s.unsubscribeLeadership()
		s.unsubscribeLeadership = nil
	}
	s.Drain()
}

// StartGRPC preserves the small embedding API used by tests and tools. Serve
// failures are still logged by StartGRPCWithErrors; Controller.Start uses the
// error channel directly so the CLI can propagate the failure.
func StartGRPC(ctx context.Context, config Config, repositories *ControllerRepositories, metrics ...*ControllerMetrics) (net.Listener, *grpc.Server, error) {
	listener, server, serveErr, err := StartGRPCWithErrors(ctx, config, repositories, metrics...)
	if err == nil {
		go func() {
			if serveErr != nil {
				if value, ok := <-serveErr; ok && value != nil {
					slog.Default().Error("gRPC server stopped", "error", value)
				}
			}
		}()
	}
	return listener, server, err
}

func StartGRPCWithErrors(ctx context.Context, config Config, repositories *ControllerRepositories, metrics ...*ControllerMetrics) (net.Listener, *grpc.Server, <-chan error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	server, err := NewControlServer(config, repositories, metrics...)
	if err != nil {
		return nil, nil, nil, err
	}
	tlsConfig, err := loadControlTLS(config)
	if err != nil {
		server.Close()
		return nil, nil, nil, err
	}
	listener, err := net.Listen("tcp", config.GRPCListen)
	if err != nil {
		server.Close()
		return nil, nil, nil, err
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.MaxRecvMsgSize(16<<20), grpc.MaxSendMsgSize(16<<20))
	v1.RegisterControlServer(grpcServer, server)
	serveErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		// Connect is intentionally long-lived. A graceful gRPC stop waits for
		// every bidirectional stream to finish, but a node may keep retrying
		// reads until it observes the transport close. Force-stop first so
		// cancellation is deterministic; Controller.Close uses the same path.
		grpcServer.Stop()
		_ = listener.Close()
	}()
	go func() {
		defer server.Close()
		serveErr <- grpcServer.Serve(listener)
		close(serveErr)
	}()
	return listener, grpcServer, serveErr, nil
}
