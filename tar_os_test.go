package tar

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExtractor_ChownErrorHandling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific chown handling on Windows")
	}

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "test.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	// Create an archive with strict permissions and specific UID/GID
	tw, err := CreateWriter(archivePath, Store)
	if err != nil {
		t.Fatalf("Failed to create writer: %v", err)
	}
	hdr := &Header{
		Name:  "secret.txt",
		Mode:  0700,
		Uid:   9999,
		Gid:   9999,
		Uname: "fakeuser",
		Size:  6,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("secret"))
	tw.Close()

	chownCalled := false
	handler := func(name string, err error) error {
		chownCalled = true
		return nil // Ignore chown error (simulate unprivileged user extraction)
	}

	e, err := NewExtractor(archivePath, dstDir, WithExtractorChownErrorHandler(handler))
	if err != nil {
		t.Fatalf("Failed to create extractor: %v", err)
	}

	err = e.Extract(context.Background())
	if err != nil {
		t.Fatalf("Extraction failed despite error handler: %v", err)
	}

	if os.Getuid() != 0 && !chownCalled {
		t.Log("Chown error handler wasn't triggered. (Normal if running as root, otherwise suspicious).")
	}

	info, err := os.Stat(filepath.Join(dstDir, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Errorf("Permissions lost! Expected 0700, got %o", info.Mode().Perm())
	}
}
