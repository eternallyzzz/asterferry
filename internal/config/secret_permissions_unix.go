//go:build !windows

package config

import (
	"errors"
	"os"
)

func ValidateSecretFilePermissions(_ string, info os.FileInfo) error {
	if info == nil {
		return errors.New("secret file metadata is missing")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("secret file must be readable only by its owner")
	}
	return nil
}
