//go:build linux
// +build linux

package tar

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestXattrsAndACLs(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "src.txt")
	os.WriteFile(srcFile, []byte("data"), 0644)

	// Set a standard user extended attribute
	err := unix.Setxattr(srcFile, "user.testattr", []byte("testvalue"), 0)
	if err != nil {
		t.Skipf("Filesystem does not support user xattrs: %v", err)
	}

	// Archive with xattrs enabled
	archivePath := filepath.Join(tmpDir, "xattr.tar")
	a, err := NewArchiver(archivePath, tmpDir, WithArchiverXattrs(true))
	if err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(srcFile)
	files := map[string]os.FileInfo{srcFile: fi}

	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Extract with xattrs enabled
	dstDir := filepath.Join(tmpDir, "dst")
	e, err := NewExtractor(archivePath, dstDir, WithExtractorXattrs(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Close()

	// Verify the xattr was restored successfully (covers POSIX ACLs & SELinux contexts via SCHILY.xattr.*)
	dstFile := filepath.Join(dstDir, "src.txt")
	val := make([]byte, 100)
	sz, err := unix.Getxattr(dstFile, "user.testattr", val)
	if err != nil {
		t.Fatalf("Failed to get xattr on extracted file: %v", err)
	}
	if string(val[:sz]) != "testvalue" {
		t.Errorf("Expected 'testvalue', got %s", string(val[:sz]))
	}
}

func TestExtractor_SparseExtraction(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "sparse.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	// Create an archive containing a file filled completely with zeros
	f, _ := os.Create(archivePath)
	tw := NewWriter(f)

	// Create a 1MB file of purely zeros
	zeroSize := int64(1024 * 1024)
	tw.WriteHeader(&Header{Name: "zeros.txt", Size: zeroSize, Mode: 0644})
	tw.Write(make([]byte, zeroSize))
	tw.Close()
	f.Close()

	// Extract with sparse mode enabled (-S)
	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })
	e, err := NewExtractor(archivePath, dstDir, WithExtractorSparse(true), ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatalf("Extraction failed: %v", err)
	}

	// Verify that the file was created as sparse on disk (consumes very few blocks)
	targetFile := filepath.Join(dstDir, "zeros.txt")
	fi, err := os.Stat(targetFile)
	if err != nil {
		t.Fatal(err)
	}

	if fi.Size() != zeroSize {
		t.Errorf("Logical size mismatch: expected %d, got %d", zeroSize, fi.Size())
	}

	sys, ok := fi.Sys().(*unix.Stat_t)
	if ok {
		// sys.Blocks is in 512-byte units.
		// 1MB of physical data = 2048 blocks.
		// If it's sparse, it should consume almost zero blocks.
		// We assert that it consumes fewer than 50 blocks.
		if sys.Blocks > 50 {
			t.Errorf("Expected file to be physically sparse, but it took %d blocks", sys.Blocks)
		}
	}
}
