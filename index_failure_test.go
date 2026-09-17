//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos

package tar

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// writeTruncatedArchive writes a .tar.gz whose stream stops in the middle, so
// indexing it reads a few entries and then fails.
func writeTruncatedArchive(t *testing.T, dir string) string {
	t.Helper()
	payload := bytes.Repeat([]byte("indexing payload\n"), 64)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := NewWriter(gz)
	for i := 0; i < 50; i++ {
		if err := tw.WriteHeader(&Header{
			Name:     fmt.Sprintf("member%02d.txt", i),
			Mode:     0o644,
			Size:     int64(len(payload)),
			Typeflag: TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	path := filepath.Join(dir, "truncated.tar.gz")
	if err := os.WriteFile(path, data[:len(data)*2/3], 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIndexArchiveRemovesAnIndexItCouldNotFinish(t *testing.T) {
	dir := t.TempDir()
	archivePath := writeTruncatedArchive(t, dir)
	indexPath := filepath.Join(dir, "truncated.index.sqlite")

	if err := IndexArchive(archivePath, indexPath); err == nil {
		t.Fatal("IndexArchive() of a truncated archive returned no error")
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("an unfinished index was left at %s (stat error %v)", indexPath, err)
	}
}

// An archive that cannot be read must keep saying so, rather than opening as
// an empty archive once a failed attempt has left an index behind.
func TestNewFSKeepsFailingOnAnUnreadableArchive(t *testing.T) {
	dir := t.TempDir()
	archivePath := writeTruncatedArchive(t, dir)
	indexPath := filepath.Join(dir, "truncated.index.sqlite")

	for attempt := 1; attempt <= 2; attempt++ {
		tfs, err := NewFS(archivePath, indexPath)
		if err != nil {
			continue
		}
		entries, readErr := fs.ReadDir(tfs, ".")
		_ = tfs.Close()
		t.Fatalf("attempt %d: opened a truncated archive with %d entries (ReadDir error %v)", attempt, len(entries), readErr)
	}
}
