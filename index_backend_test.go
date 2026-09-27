package tar

import (
	"os"
	"path/filepath"
	"testing"
)

// buildIndexBackendTestArchive creates a tiny, real tar archive (one file,
// under a directory, so both Lookup and List have something to find) for
// the WithFSIndexBackend tests below.
func buildIndexBackendTestArchive(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hello, index backend"), 0644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(tmpDir, "archive.tar")
	if err := Compress(srcDir, archivePath); err != nil {
		t.Fatalf("Compress failed: %v", err)
	}
	return archivePath
}

// TestNewFSIndexBackendArcidxExplicit verifies WithFSIndexBackend(IndexBackendArcidx)
// opens a working TarFS backed by *ArcidxIndex regardless of the
// tarindex_simple build tag - unlike Index/OpenIndex, ArcidxIndex has never
// had a build tag of its own (unxed/tar#9), so this must succeed in every
// build.
func TestNewFSIndexBackendArcidxExplicit(t *testing.T) {
	archivePath := buildIndexBackendTestArchive(t)
	indexPath := filepath.Join(t.TempDir(), "explicit.arcidx")

	tfs, err := NewFS(archivePath, indexPath, WithFSIndexBackend(IndexBackendArcidx))
	if err != nil {
		t.Fatalf("NewFS with IndexBackendArcidx failed: %v", err)
	}
	defer tfs.Close()

	if _, ok := tfs.Index.(*ArcidxIndex); !ok {
		t.Fatalf("TarFS.Index is %T, want *ArcidxIndex", tfs.Index)
	}

	data, err := readAllFromFS(tfs, "src/hello.txt")
	if err != nil {
		t.Fatalf("reading src/hello.txt via the explicit arcidx backend failed: %v", err)
	}
	if string(data) != "hello, index backend" {
		t.Fatalf("unexpected content: %q", data)
	}
}

// TestNewFSIndexBackendAutoUnchanged pins down that omitting
// WithFSIndexBackend keeps opening successfully - i.e. adding the option
// didn't change NewFS's default (IndexBackendAuto) behavior for existing
// callers that never pass it.
func TestNewFSIndexBackendAutoUnchanged(t *testing.T) {
	archivePath := buildIndexBackendTestArchive(t)
	indexPath := filepath.Join(t.TempDir(), "auto.index")

	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatalf("NewFS with the default IndexBackendAuto failed: %v", err)
	}
	defer tfs.Close()

	data, err := readAllFromFS(tfs, "src/hello.txt")
	if err != nil {
		t.Fatalf("reading src/hello.txt via the default backend failed: %v", err)
	}
	if string(data) != "hello, index backend" {
		t.Fatalf("unexpected content: %q", data)
	}
}

func readAllFromFS(tfs *TarFS, name string) ([]byte, error) {
	f, err := tfs.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 0, 64)
	tmp := make([]byte, 32)
	for {
		n, err := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}
