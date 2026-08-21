// Package lifecycle contains role-neutral helpers for shutdown and error
// reporting. Keeping error classification here prevents Agent and Gateway
// from slowly developing different operational vocabularies.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func ErrorKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	t := fmt.Sprintf("%T", err)
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return strings.Trim(t, "*")
}
