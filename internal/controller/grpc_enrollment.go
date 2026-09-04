package controller

import (
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"context"
	"errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"log/slog"
	"time"
)

func (s *ControlServer) Enroll(ctx context.Context, request *v1.EnrollRequest) (response *v1.EnrollResponse, returnErr error) {
	defer func() {
		if s.metrics != nil {
			code := codes.OK.String()
			if returnErr != nil {
				code = status.Code(returnErr).String()
			}
			s.metrics.observeGRPC("Enroll", code)
		}
	}()
	if request == nil || request.GetToken() == "" || request.GetNodeId() == "" || len(request.GetCsrDer()) == 0 || len(request.GetCsrDer()) > 128<<10 {
		return nil, status.Error(codes.InvalidArgument, "token, node_id and csr_der are required")
	}
	if allowed, retry := s.enrollLimiter.allow(peerAddressKey(ctx)); !allowed {
		return nil, status.Errorf(codes.ResourceExhausted, "enrollment rate limit exceeded; retry after %s", retry.Round(time.Second))
	}
	select {
	case s.enrollSlots <- struct{}{}:
		defer func() { <-s.enrollSlots }()
	default:
		return nil, status.Error(codes.ResourceExhausted, "enrollment capacity is temporarily exhausted")
	}
	certificate, err := s.store.IssueNodeCertificate(ctx, s.config, request.GetToken(), request.GetNodeId(), request.GetCsrDer())
	if err != nil {
		if errors.Is(err, ErrInvalidEnrollmentRequest) {
			return nil, status.Error(codes.InvalidArgument, "enrollment request is invalid")
		}
		if isCredentialError(err) {
			return nil, status.Error(codes.PermissionDenied, "enrollment credentials are invalid")
		}
		slog.Default().Error("node enrollment failed", "node_id", request.GetNodeId(), "error", err)
		return nil, status.Error(codes.Unavailable, "enrollment service is temporarily unavailable")
	}
	return &v1.EnrollResponse{SchemaVersion: domain.SchemaVersion, Certificate: &v1.CertificateBundle{CertificateDer: certificateDER(certificate.CertificatePEM), CaCertificateDer: certificateDER(certificate.CAPEM), Serial: certificate.Serial, NotBefore: timestamppb.New(certificate.NotBefore), NotAfter: timestamppb.New(certificate.NotAfter)}}, nil
}
