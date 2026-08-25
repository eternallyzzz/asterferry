// Package atomicfile contains the shared file-write primitives used by
// configuration and bundle generation code.
package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

const tempPrefix = ".asterferry-atomic-*"

// Write creates the parent directory, writes data to path, and applies the
// requested permission bits after the write. It is intended for generated
// files whose callers do not need a replace-in-place transaction.
func Write(path string, data []byte, mode os.FileMode) error {
	if err := ensureParent(path); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode.Perm()); err != nil {
		return err
	}
	return os.Chmod(path, mode.Perm())
}

// AtomicWrite writes data through a synced temporary file and replaces path.
// On platforms that cannot rename over an existing file, it retries after
// removing the old target. The temporary file is removed on every failure
// path.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := ensureParent(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPrefix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	permissions := mode.Perm()
	if err := tmp.Chmod(permissions); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := writeAll(tmp, data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	} else {
		if removeErr := os.Remove(path); removeErr != nil {
			return errors.Join(err, removeErr)
		}
		if renameErr := os.Rename(tmpPath, path); renameErr != nil {
			return renameErr
		}
	}
	return nil
}

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
