package controller

import "google.golang.org/grpc"

// grpcServer is the lifecycle surface Controller needs from the gRPC
// implementation. Keeping this as a named port makes shutdown semantics
// explicit without forcing the controller to depend on the full server API.
type grpcServer interface {
	Stop()
	GracefulStop()
}

var _ grpcServer = (*grpc.Server)(nil)
