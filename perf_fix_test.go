package tar

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestArchiver_IntegrityRaceCondition verifies that concurrent reading of small files
// does not cause data corruption when using multi-threaded compression.
// This is a regression test for a bug where pooled buffers were returned to the pool
// while ZSTD was still compressing them in the background.
func TestArchiver_IntegrityRaceCondition(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)

	// Create 100 files with unique but highly compressible content.
	// 1MB each is enough to trigger the concurrency and pooling logic.
	fileCount := 100
	fileSize := 1024 * 1024
	files := make(map[string]os.FileInfo)

	for i := 0; i < fileCount; i++ {
		path := filepath.Join(srcDir, fmt.Sprintf("file_%d.txt", i))
		content := bytes.Repeat([]byte{byte(i)}, fileSize)
		os.WriteFile(path, content, 0644)
		fi, _ := os.Stat(path)
		files[path] = fi
	}

	archivePath := filepath.Join(tmpDir, "integrity.tar.zst")

	// 1. Archive
	a, err := NewArchiver(archivePath, tmpDir, WithArchiverMethod(ZSTD))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatalf("Archiving failed: %v", err)
	}
	a.Close()

	// 2. Extract and Verify
	dstDir := filepath.Join(tmpDir, "dst")
	e, err := NewExtractor(archivePath, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Extract(context.Background()); err != nil {
		t.Fatalf("Extraction failed: %v", err)
	}
	e.Close()

	for i := 0; i < fileCount; i++ {
		path := filepath.Join(dstDir, "src", fmt.Sprintf("file_%d.txt", i))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("Failed to read extracted file %d: %v", i, err)
			continue
		}
		expected := byte(i)
		for j, b := range data {
			if b != expected {
				t.Fatalf("Data corruption at file %d, byte %d: expected 0x%02x, got 0x%02x", i, j, expected, b)
			}
		}
	}
}