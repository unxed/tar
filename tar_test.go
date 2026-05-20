package tar

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCompressAndExtract(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0755)

	os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("hello tar"), 0644)
	os.WriteFile(filepath.Join(srcDir, "sub", "file2.txt"), []byte("nested"), 0644)

	archivePath := filepath.Join(tmpDir, "archive.tar.gz")

	// Test Compress
	if err := Compress(srcDir, archivePath); err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	// Test Extract
	if err := Extract(archivePath, dstDir); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dstDir, "src", "file1.txt"))
	if err != nil || string(content) != "hello tar" {
		t.Errorf("file1.txt extraction failed")
	}

	content, err = os.ReadFile(filepath.Join(dstDir, "src", "sub", "file2.txt"))
	if err != nil || string(content) != "nested" {
		t.Errorf("sub/file2.txt extraction failed")
	}
}

func TestUpdater(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "append.tar")

	// Create a standard uncompressed tar
	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "first.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	// Open for update
	fRW, _ := os.OpenFile(archivePath, os.O_RDWR, 0644)
	updater, err := NewUpdater(fRW, APPEND_MODE_OVERWRITE)
	if err != nil {
		t.Fatalf("NewUpdater failed: %v", err)
	}

	err = updater.Append("second.txt", 6, []byte("second"))
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	updater.Close()
	fRW.Close()

	// Verify both files exist
	rc, err := OpenReader(archivePath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer rc.Close()

	found := 0
	for {
		hdr, err := rc.Next()
		if err == io.EOF {
			break
		}
		if hdr.Name == "first.txt" || hdr.Name == "second.txt" {
			found++
		}
	}

	if found != 2 {
		t.Errorf("Expected 2 files, found %d", found)
	}
}

func TestZstdCompression(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "test.tar.zst")

	wc, err := CreateWriter(archivePath, ZSTD)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}

	wc.WriteHeader(&Header{Name: "zst.txt", Size: 4})
	wc.Write([]byte("zstd"))
	wc.Close()

	rc, err := OpenReader(archivePath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer rc.Close()

	hdr, err := rc.Next()
	if err != nil || hdr.Name != "zst.txt" {
		t.Errorf("Failed to read ZSTD compressed tar")
	}

	buf := new(bytes.Buffer)
	io.Copy(buf, rc)
	if buf.String() != "zstd" {
		t.Errorf("Data mismatch")
	}
}