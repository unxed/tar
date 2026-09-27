//go:build (freebsd || openbsd || netbsd || dragonfly || solaris || illumos) && !tarindex_simple

package tar

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestNewFSIndexBackendSQLiteExplicitUnsupported mirrors
// index_backend_sqlite_test.go for the OS set where SQLite was never wired
// up (sqlite_disabled.go): explicit IndexBackendSQLite selection returns the
// same errNoSqlite OpenIndex has always returned here, rather than either
// silently substituting ArcidxIndex or getting a compile error over the
// missing *Index.db field it doesn't have on this build anyway.
func TestNewFSIndexBackendSQLiteExplicitUnsupported(t *testing.T) {
	archivePath := buildIndexBackendTestArchive(t)
	indexPath := filepath.Join(t.TempDir(), "explicit.sqlite")

	_, err := NewFS(archivePath, indexPath, WithFSIndexBackend(IndexBackendSQLite))
	if !errors.Is(err, errNoSqlite) {
		t.Fatalf("NewFS with IndexBackendSQLite: got err=%v, want errNoSqlite", err)
	}
}
