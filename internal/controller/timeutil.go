package controller

import (
	"fmt"
	"time"
)

func parseStoredTime(field, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid stored %s: %w", ErrStorageFailure, field, err)
	}
	return parsed, nil
}
