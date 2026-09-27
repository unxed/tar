//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos && !tarindex_simple

package tar

import (
	"path/filepath"
	"testing"
)

// TestNewFSIndexBackendSQLiteExplicit exercises WithFSIndexBackend(IndexBackendSQLite)
// on the OS set where the SQLite backend actually works (sqlite_enabled.go's
// own build tag, minus tarindex_simple - see
// index_backend_sqlite_disabled_test.go for the BSD/Solaris/illumos set, and
// index_backend_tarindex_simple_test.go for tarindex_simple builds). It
// should open a *Index-backed TarFS exactly like IndexBackendAuto already
// does today.
func TestNewFSIndexBackendSQLiteExplicit(t *testing.T) {
	archivePath := buildIndexBackendTestArchive(t)
	indexPath := filepath.Join(t.TempDir(), "explicit.sqlite")

	tfs, err := NewFS(archivePath, indexPath, WithFSIndexBackend(IndexBackendSQLite))
	if err != nil {
		t.Fatalf("NewFS with IndexBackendSQLite failed: %v", err)
	}
	defer tfs.Close()

	if _, ok := tfs.Index.(*Index); !ok {
		t.Fatalf("TarFS.Index is %T, want *Index", tfs.Index)
	}

	data, err := readAllFromFS(tfs, "src/hello.txt")
	if err != nil {
		t.Fatalf("reading src/hello.txt via the explicit sqlite backend failed: %v", err)
	}
	if string(data) != "hello, index backend" {
		t.Fatalf("unexpected content: %q", data)
	}
}
