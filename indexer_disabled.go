//go:build freebsd || openbsd || netbsd || dragonfly || solaris || illumos

package tar

import "errors"

// errNoSqlite is declared here rather than in sqlite_disabled.go, even
// though both files share the OS gate above: sqlite_disabled.go additionally
// excludes !tarindex_simple (it defines the sqlite-flavoured Index/OpenIndex
// stubs, which tarindex_simple.go replaces), while this file's IndexArchive
// stub applies unconditionally on these OSes regardless of tarindex_simple -
// indexer_enabled.go, the on-the-fly scanning loop this file substitutes
// for, is intentionally left untouched by the tarindex_simple wiring (see
// tarindex_simple.go's doc comment) and keeps excluding these OSes either
// way. Declaring errNoSqlite only in sqlite_disabled.go would make this file
// fail to compile for freebsd || ... combined with -tags tarindex_simple.
var errNoSqlite = errors.New("tar: indexing is not supported on this platform (SQLite requires CGO or missing libc support)")

func IndexArchive(archivePath, indexPath string) error {
	return errNoSqlite
}

// IndexArchiveWithBackend mirrors IndexArchive on this OS set: the scanning
// loop itself (indexer_enabled.go) is unavailable here regardless of which
// IndexBackend was requested, exactly as openSQLiteIndex (sqlite_disabled.go)
// already treats IndexBackendSQLite the same as IndexBackendAuto on these
// OSes - see index_backend.go's IndexBackendSQLite doc comment.
func IndexArchiveWithBackend(archivePath, indexPath string, backend IndexBackend) error {
	return errNoSqlite
}
