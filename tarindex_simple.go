//go:build tarindex_simple

// This file, under the tarindex_simple build tag, replaces the sqlite-backed
// Index (sqlite_enabled.go / sqlite_disabled.go) with ArcidxIndex
// (arcidx_index.go) as the concrete type behind the Index name.
//
// This is step 2 of the 11-step plan to make f4's "lite" build not depend on
// sqlite at all for .tar.gz indexing (see
// https://github.com/unxed/f4/issues/1178#issuecomment-5851433383, "Proposed
// plan (RUP steps, forks first)"). Step 1 (unxed/tar#9) added ArcidxIndex as
// a complete, tested, but unused-by-anything alternative implementation of
// the exact same method surface as Index. This file is the wiring: it makes
// Index an alias for ArcidxIndex, so every existing caller (writer.go,
// indexer_enabled.go, fs.go, and every test that only uses the public
// OpenIndex/Close/InitMetadata/Insert/Lookup/List/RecursiveSize/
// InsertBlockOffsets/GetClosestBlockOffset/GetGzipIndex/SaveGzipIndex
// surface) keeps compiling and behaving the same, just against a
// FlatBuffers-backed, sqlite-free store instead of a SQLite database.
//
// Deliberately a plain, always-opt-in build tag (tarindex_simple), not an
// OS-based one: unlike sqlite_enabled.go/sqlite_disabled.go, which pick a
// backend automatically depending on whether cgo-free sqlite is wired up for
// the current GOOS, this is a build-time choice that any platform can make
// (e.g. to avoid sqlite entirely even where it works fine). The name is
// intentionally distinct from f4's own "lite" build tag, per the plan
// comment linked above, precisely so this repository and f4 aren't coupled
// by tag name: f4's lite build is expected to pass this tag down to its
// vendored/imported copy of unxed/tar, but tar itself has no notion of an
// f4-flavoured "lite" mode of its own.
//
// Index is a type alias (type Index = ArcidxIndex), not a new named type
// wrapping ArcidxIndex. This matters: a defined type (type Index
// ArcidxIndex) would need every method re-declared as thin forwarders and
// would be a different type from ArcidxIndex as far as the type system is
// concerned, breaking anything that type-asserts or otherwise cares about
// identity. An alias makes Index and ArcidxIndex literally the same type, so
// ArcidxIndex's existing method set (already verified in step 1 to match
// Index's signatures 1:1) is Index's method set too, with no forwarding code
// and no risk of the two drifting out of sync as ArcidxIndex evolves.
package tar

import "errors"

// Index, under this build tag, is ArcidxIndex: see the package doc comment
// above for why this is a type alias rather than a new type.
type Index = ArcidxIndex

// OpenIndex, under this build tag, opens (or, per OpenArcidxIndex's own doc
// comment, transparently creates) an ArcidxIndex at dsn. The parameter is
// still named dsn to match the signature callers already depend on
// (sqlite_enabled.go's OpenIndex takes a sqlite data source name), even
// though ArcidxIndex treats it as a plain filesystem path rather than a
// SQLite DSN string.
func OpenIndex(dsn string) (*Index, error) {
	return OpenArcidxIndex(dsn)
}

// ErrSQLiteBackendUnavailable is returned by IndexBackendSQLite
// (index_backend.go) on a tarindex_simple build: sqlite_enabled.go/
// sqlite_disabled.go are excluded by this same tag (see their own build
// constraints), so there is no SQLite-backed Index type to open at all here,
// unlike on the BSD/Solaris OSes where SQLite is merely unwired
// (errNoSqlite) rather than uncompiled.
var ErrSQLiteBackendUnavailable = errors.New("tar: SQLite index backend was not compiled into this binary (built with -tags tarindex_simple)")

func openSQLiteIndex(dsn string) (FileIndex, error) {
	return nil, ErrSQLiteBackendUnavailable
}
