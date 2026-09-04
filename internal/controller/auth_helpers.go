package controller

import (
	"asterferry/internal/domain"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Usernames are identifiers in the API but need not use the narrower
// resource-ID alphabet: operators commonly use an email address or a
// directory principal. Keep the value bounded and single-line while allowing
// those interoperable forms.
func validateUsername(username string) error {
	if username == "" || len(username) > 128 || strings.TrimSpace(username) == "" || strings.ContainsAny(username, "\x00\r\n") {
		return &domain.ApplyError{Code: "invalid_username", Path: "username", Message: "username must contain 1 to 128 single-line characters"}
	}
	return nil
}
