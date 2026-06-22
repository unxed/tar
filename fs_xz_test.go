//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos
package tar

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unxed/xz"
)

func TestTarFS_XZRandomAccess(t *testing.T) {
	// 1. Create a raw TAR archive in memory
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)

	fileContents := []string{
		strings.Repeat("A", 2048), // file1.txt
		strings.Repeat("B", 2048), // file2.txt
		strings.Repeat("C", 2048), // file3.txt
	}

	for i, content := range fileContents {
		name := fmt.Sprintf("file%d.txt", i+1)
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()

	// 2. Compress the TAR using XZ with a small BlockSize to force multiple blocks
	var xzBuf bytes.Buffer
	xzCfg := xz.WriterConfig{
		BlockSize: 512, // Force small blocks
	}
	xw, err := xzCfg.NewWriter(&xzBuf)
	if err != nil {
		t.Fatal(err)
	}
	xw.Write(tarBuf.Bytes())
	xw.Close()

	// 3. Write to a temporary file
	tmpDir := t.TempDir()
	arcPath := filepath.Join(tmpDir, "test.tar.xz")
	if err := os.WriteFile(arcPath, xzBuf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
    
	// 4. Open with TarFS
	tfs, err := NewFS(arcPath, "")
	if err != nil {
		t.Fatalf("NewFS failed: %v", err)
	}
	defer tfs.Close()

	// 5. Verify blocks were parsed natively using the new unxed/xz API
	if len(tfs.xzBlocks) <= 1 {
		t.Fatalf("expected multiple XZ blocks due to small BlockSize, got %d", len(tfs.xzBlocks))
	}

	// 6. Read "file3.txt" directly (should trigger random access fast-path)
	f, err := tfs.Open("file3.txt")
	if err != nil {
		t.Fatalf("failed to open file3.txt: %v", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("failed to read file3.txt: %v", err)
	}

	if !bytes.Equal(data, []byte(fileContents[2])) {
		t.Errorf("content mismatch: expected %d bytes of 'C', got different data", len(fileContents[2]))
	}

	// 7. Verify fs.ReadFile works on earlier blocks too
	data2, err := fs.ReadFile(tfs, "file1.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data2, []byte(fileContents[0])) {
		t.Error("content mismatch for file1.txt")
	}
}
