package lifecycle

import (
	"context"
	"errors"
	"testing"
)

func TestErrorKind(t *testing.T) {
	if got := ErrorKind(context.Canceled); got != "canceled" {
		t.Fatalf("canceled kind: %q", got)
	}
	if got := ErrorKind(context.DeadlineExceeded); got != "deadline_exceeded" {
		t.Fatalf("deadline kind: %q", got)
	}
	if got := ErrorKind(errors.New("boom")); got == "" {
		t.Fatal("ordinary errors need a stable kind")
	}
}
