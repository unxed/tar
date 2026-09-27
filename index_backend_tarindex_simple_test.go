//go:build tarindex_simple

package tar

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestNewFSIndexBackendSQLiteUnavailable verifies that, on a tarindex_simple
// build, explicitly requesting IndexBackendSQLite fails loudly with
// ErrSQLiteBackendUnavailable instead of silently falling back to
// ArcidxIndex - the SQLite backend was never compiled into this binary at
// all (sqlite_enabled.go/sqlite_disabled.go both exclude this tag), so there
// is nothing for IndexBackendSQLite to open.
func TestNewFSIndexBackendSQLiteUnavailable(t *testing.T) {
	archivePath := buildIndexBackendTestArchive(t)
	indexPath := filepath.Join(t.TempDir(), "explicit.sqlite")

	_, err := NewFS(archivePath, indexPath, WithFSIndexBackend(IndexBackendSQLite))
	if !errors.Is(err, ErrSQLiteBackendUnavailable) {
		t.Fatalf("NewFS with IndexBackendSQLite on a tarindex_simple build: got err=%v, want ErrSQLiteBackendUnavailable", err)
	}
}
