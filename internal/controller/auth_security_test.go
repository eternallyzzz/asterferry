package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestPasswordDigestIsKeyed(t *testing.T) {
	var key [masterKeyBytes]byte
	copy(key[:], "01234567890123456789012345678901")
	password := "a-very-long-password"
	digest := passwordDigest(key, password)
	plainDigest := sha256.Sum256([]byte(password))
	if digest == hex.EncodeToString(plainDigest[:]) {
		t.Fatal("password digest is an unkeyed SHA-256 digest")
	}
	other := key
	other[0]++
	if digest == passwordDigest(other, password) {
		t.Fatal("password digest did not depend on the master key")
	}
}

func TestDummyPasswordHashIsValidArgon2id(t *testing.T) {
	if !VerifyPassword(dummyPasswordHash, "asterferry-dummy-password") {
		t.Fatal("fixed dummy password hash does not verify")
	}
}
