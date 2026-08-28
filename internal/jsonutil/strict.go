// Package jsonutil contains the shared JSON decoding rules used by the
// Controller and node boundaries.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ErrTrailingJSON indicates that more than one JSON value was supplied.
// Keeping this sentinel allows callers to preserve their boundary-specific
// error wording while sharing the strict parser implementation.
var ErrTrailingJSON = errors.New("trailing JSON")

// DecodeStrict decodes exactly one JSON value, rejects unknown object fields,
// and rejects any non-whitespace data after that value.
func DecodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ErrTrailingJSON
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
