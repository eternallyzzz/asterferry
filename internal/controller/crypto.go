package controller

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"asterferry/internal/atomicfile"
	"golang.org/x/crypto/argon2"
)

const (
	masterKeyBytes   = 32
	argonMemory      = 64 * 1024
	argonTime        = 3
	argonThreads     = 2
	argonSaltBytes   = 16
	argonHashBytes   = 32
	maxPasswordBytes = 4096
)

// LoadOrCreateMasterKey reads a 32-byte key and creates it atomically when it
// does not exist. The caller may use the key only for encrypting controller
// secrets; node credentials are never derived from it.
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, errors.New("master key path is required")
	}
	if key, err := os.ReadFile(path); err == nil {
		if len(key) != masterKeyBytes {
			return nil, errors.New("master key must contain exactly 32 bytes")
		}
		return append([]byte(nil), key...), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	key := make([]byte, masterKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	tmpName, err := atomicfile.WriteTemp(path, ".master-key-*", key, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmpName) }()
	if err := os.Rename(tmpName, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && len(existing) == masterKeyBytes {
			return existing, nil
		}
		return nil, fmt.Errorf("publish master key: %w", err)
	}
	return key, nil
}

func EncryptSecret(key, plaintext []byte) ([]byte, error) {
	if len(key) != masterKeyBytes {
		return nil, errors.New("master key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	result := gcm.Seal(nil, nonce, plaintext, nil)
	return append(nonce, result...), nil
}

func DecryptSecret(key, ciphertext []byte) ([]byte, error) {
	if len(key) != masterKeyBytes {
		return nil, errors.New("master key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("ciphertext is truncated")
	}
	nonce, data := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, errors.New("ciphertext authentication failed")
	}
	return plaintext, nil
}

func EncryptSecretString(key []byte, plaintext string) (string, error) {
	b, err := EncryptSecret(key, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func DecryptSecretString(key []byte, value string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", errors.New("secret is not valid base64")
	}
	plaintext, err := DecryptSecret(key, b)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must contain at least 12 characters")
	}
	if len(password) > maxPasswordBytes {
		return "", errors.New("password is too long")
	}
	salt := make([]byte, argonSaltBytes)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonHashBytes)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

// VerifyPassword accepts only the canonical parameters emitted by
// HashPassword. The encoded hash is database-controlled data; allowing it to
// select a larger Argon2 cost would turn database tampering into a login DoS.
func VerifyPassword(encoded, password string) bool {
	if len(password) < 12 || len(password) > maxPasswordBytes {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	params := make(map[string]uint32, 3)
	for _, part := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(part, "=", 2)
		if len(pair) != 2 {
			return false
		}
		if pair[0] != "m" && pair[0] != "t" && pair[0] != "p" {
			return false
		}
		if _, exists := params[pair[0]]; exists {
			return false
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return false
		}
		params[pair[0]] = uint32(value)
	}
	memory, okM := params["m"]
	timeCost, okT := params["t"]
	threads, okP := params["p"]
	if len(params) != 3 || !okM || !okT || !okP || memory != argonMemory || timeCost != argonTime || threads != argonThreads {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, uint32(memory), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func NewAPIToken() (plain, digest string, err error) {
	token := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return "", "", err
	}
	plain = "af_" + base64.RawURLEncoding.EncodeToString(token)
	digest = HashToken(plain)
	return plain, digest, nil
}

func HashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func TokenEqual(digest, token string) bool {
	left, err := hex.DecodeString(strings.TrimSpace(digest))
	if err != nil || len(left) != sha256.Size {
		return false
	}
	right := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(left, right[:]) == 1
}
