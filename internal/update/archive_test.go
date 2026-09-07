package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractBinaryFromZip(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "release.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("bin/asterferry.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("windows binary")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(root, "staged.exe")
	if err := ExtractBinary(archivePath, destination, "asterferry.exe"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "windows binary" {
		t.Fatalf("extracted zip binary = %q", data)
	}
}

func TestExtractBinaryFromTarGz(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "release.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	data := []byte("linux binary")
	if err := writer.WriteHeader(&tar.Header{Name: "asterferry", Mode: 0o755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(root, "staged")
	if err := ExtractBinary(archivePath, destination, "asterferry"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("extracted tar binary = %q", got)
	}
}

func TestExtractBinaryRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "unsafe.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("../../asterferry")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("unsafe"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractBinary(archivePath, filepath.Join(root, "staged"), "asterferry"); err == nil {
		t.Fatal("archive traversal was accepted")
	}
}
