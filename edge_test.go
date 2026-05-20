package tar

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTarSlipSecurity verifies that the extractor prevents directory traversal attacks.
func TestTarSlipSecurity(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "evil.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	// 1. Manually craft a malicious TAR with an escaping path
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	evilPath := "../../../../evil.txt"
	hdr := &Header{
		Name: evilPath,
		Mode: 0644,
		Size: 4,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("evil"))
	tw.Close()
	f.Close()

	// 2. Attempt to extract
	extractor, err := NewExtractor(archivePath, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer extractor.Close()

	err = extractor.Extract(context.Background())
	if err == nil {
		t.Fatal("Expected TarSlip/ZipSlip error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot be extracted outside of chroot") {
		t.Fatalf("Expected chroot violation error, got: %v", err)
	}
}

// TestLongAndUnicodePaths verifies handling of paths > 100 chars (requires PAX/GNU extensions)
// and paths containing Unicode characters.
func TestLongAndUnicodePaths(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")

	// Create a deep path well over 100 bytes containing multibyte characters.
	// We keep individual component names under 255 bytes to prevent OS-level ENAMETOOLONG.
	longUnicodeName := strings.Repeat("subdir/", 15) + strings.Repeat("абвгдежз", 10) + ".txt"

	fullSrcPath := filepath.Join(srcDir, longUnicodeName)
	os.MkdirAll(filepath.Dir(fullSrcPath), 0755)
	if err := os.WriteFile(fullSrcPath, []byte("unicode and long path test"), 0644); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(tmpDir, "long.tar")
	if err := Compress(srcDir, archivePath); err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if err := Extract(archivePath, dstDir); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	fullDstPath := filepath.Join(dstDir, "src", longUnicodeName)
	content, err := os.ReadFile(fullDstPath)
	if err != nil {
		t.Fatalf("Failed to read extracted long unicode file: %v", err)
	}
	if string(content) != "unicode and long path test" {
		t.Errorf("Content mismatch: %s", string(content))
	}
}

// TestFSEdgeCases verifies the behavior of TarFS with boundary conditions.
func TestFSEdgeCases(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "empty.tar")
	indexPath := filepath.Join(tmpDir, "empty.sqlite")

	// Create a completely empty tar
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)
	tw.Close()
	f.Close()

	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// 1. Stat on root directory (.)
	info, err := fs.Stat(tfs, ".")
	if err != nil {
		t.Fatalf("Stat on root failed: %v", err)
	}
	if !info.IsDir() {
		t.Error("Root should be identified as a directory")
	}

	// 2. Open non-existent file
	_, err = tfs.Open("does-not-exist.txt")
	if err == nil {
		t.Error("Expected error opening non-existent file, got nil")
	}

	// 3. Test invalid paths (per fs.ValidPath specs)
	_, err = tfs.Open("/invalid-leading-slash")
	if err == nil {
		t.Error("Expected error for invalid path with leading slash")
	}
	_, err = tfs.Open("../escape")
	if err == nil {
		t.Error("Expected error for invalid path containing parent traversal (../)")
	}
}

func TestExtractor_ZipBomb(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "bomb.tar")
	dstDir := filepath.Join(tmpDir, "extract")

	// Create archive with large file
	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	hdr := &Header{Name: "bomb.txt", Size: 2048, Mode: 0644}
	tw.WriteHeader(hdr)
	tw.Write(make([]byte, 2048))
	tw.Close()
	f.Close()

	// Limit to 1024 bytes
	e, _ := NewExtractor(archivePath, dstDir, WithExtractorMaxFileSize(1024))
	err := e.Extract(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("expected size limit error, got: %v", err)
	}
}

// TestUpdaterEmptyArchive tests if Updater correctly handles 0-byte initial archives.
func TestUpdaterEmptyArchive(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "zero.tar")

	// Create 0 byte file
	f, _ := os.Create(archivePath)
	f.Close()

	// Open for update
	fRW, _ := os.OpenFile(archivePath, os.O_RDWR, 0644)
	updater, err := NewUpdater(fRW, APPEND_MODE_OVERWRITE)
	if err != nil {
		t.Fatalf("NewUpdater failed on 0-byte file: %v", err)
	}

	err = updater.Append("first.txt", 4, []byte("data"))
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	updater.Close()
	fRW.Close()

	// Verify file is readable and exists
	rc, err := OpenReader(archivePath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer rc.Close()

	hdr, err := rc.Next()
	if err != nil || hdr.Name != "first.txt" {
		t.Errorf("Appended file missing or corrupted")
	}
}