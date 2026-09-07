package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ReplacementDiskState describes the only file layouts that can be acted on
// automatically after an interrupted executable replacement.
type ReplacementDiskState string

const (
	ReplacementDiskStateTargetInstalled = "target_installed"
	ReplacementDiskStateOriginalIntact  = "original_intact"
	ReplacementDiskStatePartial         = "partial"
	ReplacementDiskStateIndeterminate   = "indeterminate"
)

type replacementFileState struct {
	digest string
	exists bool
}

func replacementDigestsFor(binaryPath, stagedPath string) (replacementDigests, error) {
	originalSHA256, err := replacementFileSHA256(binaryPath)
	if err != nil {
		return replacementDigests{}, fmt.Errorf("hash current executable: %w", err)
	}
	targetSHA256, err := replacementFileSHA256(stagedPath)
	if err != nil {
		return replacementDigests{}, fmt.Errorf("hash staged executable: %w", err)
	}
	return replacementDigests{originalSHA256: originalSHA256, targetSHA256: targetSHA256}, nil
}

// ClassifyReplacementFiles compares the recorded executable identities with
// the files on disk. A pending journal is never sufficient evidence by itself:
// the process may have died after the executable rename but before the journal
// state became durable.
func ClassifyReplacementFiles(result ReplacementResult) (ReplacementDiskState, error) {
	originalSHA256 := normalizeSHA256(result.OriginalSHA256)
	targetSHA256 := normalizeSHA256(result.TargetSHA256)
	if !isSHA256(originalSHA256) || !isSHA256(targetSHA256) {
		return ReplacementDiskStateIndeterminate, errors.New("replacement journal lacks valid executable identity metadata")
	}

	binary, err := replacementFileStateFor(result.BinaryPath)
	if err != nil {
		return ReplacementDiskStateIndeterminate, fmt.Errorf("inspect live executable: %w", err)
	}
	backup, err := replacementFileStateFor(result.BackupPath)
	if err != nil {
		return ReplacementDiskStateIndeterminate, fmt.Errorf("inspect previous executable: %w", err)
	}
	staged, err := replacementFileStateFor(result.StagedPath)
	if err != nil {
		return ReplacementDiskStateIndeterminate, fmt.Errorf("inspect staged executable: %w", err)
	}

	if binary.digest == targetSHA256 && backup.digest == originalSHA256 &&
		(!staged.exists || staged.digest == targetSHA256) {
		return ReplacementDiskStateTargetInstalled, nil
	}
	if binary.digest == originalSHA256 && !backup.exists && staged.digest == targetSHA256 {
		return ReplacementDiskStateOriginalIntact, nil
	}
	if !binary.exists && backup.digest == originalSHA256 && staged.digest == targetSHA256 {
		return ReplacementDiskStatePartial, nil
	}
	return ReplacementDiskStateIndeterminate, nil
}

func replacementFileStateFor(path string) (replacementFileState, error) {
	if strings.TrimSpace(path) == "" {
		return replacementFileState{}, nil
	}
	digest, err := replacementFileSHA256(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return replacementFileState{}, nil
		}
		return replacementFileState{}, err
	}
	return replacementFileState{digest: digest, exists: true}, nil
}

func replacementFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func normalizeSHA256(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
