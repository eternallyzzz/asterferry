package controller

import (
	"errors"
	"fmt"
)

// These errors are deliberately small and stable. Handlers use errors.Is to
// choose a safe client-facing status while retaining the wrapped storage
// error for logs and diagnostics.
var (
	ErrInvalidCredentials       = errors.New("invalid credentials")
	ErrUserDisabled             = errors.New("user is disabled")
	ErrInvalidAPIToken          = errors.New("invalid API token")
	ErrAPITokenExpired          = errors.New("API token has expired")
	ErrInvalidEnrollmentToken   = errors.New("invalid enrollment token")
	ErrEnrollmentTokenUsed      = errors.New("enrollment token has already been used or revoked")
	ErrEnrollmentTokenExpired   = errors.New("enrollment token has expired")
	ErrEnrollmentRoleMismatch   = errors.New("enrollment token role does not match node role")
	ErrEnrollmentNodeMismatch   = errors.New("enrollment token is bound to a different node")
	ErrNodeNotEnrolled          = errors.New("node is not enrolled")
	ErrInvalidEnrollmentRequest = errors.New("invalid enrollment request")
	ErrNodeEnrollmentNotAllowed = errors.New("node enrollment is not allowed")
	ErrStorageFailure           = errors.New("controller storage failure")
)

func isCredentialError(err error) bool {
	return errors.Is(err, ErrInvalidCredentials) ||
		errors.Is(err, ErrUserDisabled) ||
		errors.Is(err, ErrInvalidAPIToken) ||
		errors.Is(err, ErrAPITokenExpired) ||
		errors.Is(err, ErrInvalidEnrollmentToken) ||
		errors.Is(err, ErrEnrollmentTokenUsed) ||
		errors.Is(err, ErrEnrollmentTokenExpired) ||
		errors.Is(err, ErrEnrollmentRoleMismatch) ||
		errors.Is(err, ErrEnrollmentNodeMismatch) ||
		errors.Is(err, ErrNodeNotEnrolled) ||
		errors.Is(err, ErrNodeEnrollmentNotAllowed)
}

func storageFailure(op string, err error) error {
	if err == nil {
		return nil
	}
	// Preserve both the stable classification and the underlying driver error.
	// Callers can map ErrStorageFailure to a 503 while logs and diagnostics can
	// still inspect the concrete SQLite error with errors.Is/As.
	return fmt.Errorf("%w: %s: %w", ErrStorageFailure, op, err)
}
