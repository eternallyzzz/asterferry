package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractBinary extracts only the named executable from a release archive.
// Archive paths are validated before any file is created so a compromised
// release cannot escape the staging directory.
func ExtractBinary(archivePath, destination, binaryName string) error {
	if strings.TrimSpace(archivePath) == "" || strings.TrimSpace(destination) == "" {
		return errors.New("archive and destination are required")
	}
	if !safeAssetName(binaryName) || filepath.Base(binaryName) != binaryName {
		return errors.New("binary name is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".asterferry-extract-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return err
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		err = extractZip(archivePath, temporary, binaryName)
	} else if strings.HasSuffix(strings.ToLower(archivePath), ".tar.gz") {
		err = extractTarGz(archivePath, temporary, binaryName)
	} else {
		err = errors.New("unsupported release archive format")
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o755); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

func extractZip(path string, destination *os.File, binaryName string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if !safeArchivePath(entry.Name) {
			return fmt.Errorf("release archive contains unsafe path %q", entry.Name)
		}
		if filepath.Base(entry.Name) != binaryName || entry.FileInfo().IsDir() {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(destination, io.LimitReader(reader, 512<<20))
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}
	return fmt.Errorf("release archive does not contain %s", binaryName)
}

func extractTarGz(path string, destination *os.File, binaryName string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !safeArchivePath(header.Name) {
			return fmt.Errorf("release archive contains unsafe path %q", header.Name)
		}
		if filepath.Base(header.Name) != binaryName || header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("release archive binary %s is not a regular file", binaryName)
		}
		if header.Size < 0 || header.Size > 512<<20 {
			return errors.New("release archive binary is too large")
		}
		if _, err := io.Copy(destination, io.LimitReader(reader, header.Size)); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("release archive does not contain %s", binaryName)
}

func safeArchivePath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
