// Package atomicfile contains the shared file-write primitives used by the
// Controller and node bootstrap/cache code.
package atomicfile

import (
	"errors"
	"os"
	"path/filepath"

	"asterferry/internal/wireio"
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
// On platforms that cannot rename over an existing file, it moves the old
// target to a same-directory backup while publishing the new file. If the
// second rename fails, the old target is restored before returning, so a
// Windows fallback can never leave both the destination and temporary file
// absent.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	tmpPath, err := WriteTemp(path, tempPrefix, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if firstErr := os.Rename(tmpPath, path); firstErr == nil {
		return nil
	} else {
		if _, statErr := os.Stat(path); statErr != nil {
			return errors.Join(firstErr, statErr)
		}
		backupPath, reserveErr := reservePath(path, ".asterferry-backup-*")
		if reserveErr != nil {
			return errors.Join(firstErr, reserveErr)
		}
		if backupErr := os.Rename(path, backupPath); backupErr != nil {
			_ = os.Remove(backupPath)
			return errors.Join(firstErr, backupErr)
		}
		if publishErr := os.Rename(tmpPath, path); publishErr != nil {
			restoreErr := os.Rename(backupPath, path)
			return errors.Join(firstErr, publishErr, restoreErr)
		}
		// Publishing succeeded. A failure to remove the backup is harmless to
		// the new target and is intentionally left for an operator/cleanup job;
		// returning it would make callers retry an already-published write.
		_ = os.Remove(backupPath)
		return nil
	}
}

func reservePath(path, pattern string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return "", err
	}
	reserved := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(reserved)
		return "", err
	}
	if err := os.Remove(reserved); err != nil {
		return "", err
	}
	return reserved, nil
}

// WriteTemp writes and syncs data to a temporary file in path's directory,
// returning the temporary pathname for a caller that needs a custom publish
// or fallback strategy. The temporary file is removed when writing fails.
func WriteTemp(path, pattern string, data []byte, mode os.FileMode) (string, error) {
	if err := ensureParent(path); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	closed := false
	keep := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode.Perm()); err != nil {
		return "", err
	}
	if err := wireio.WriteFull(tmp, data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	closed = true
	keep = true
	return tmpPath, nil
}

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
