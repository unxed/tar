package tar

// FileIndex is the method set both index backends implement: the
// SQLite-backed Index (sqlite_enabled.go/sqlite_disabled.go) and the
// FlatBuffers-backed ArcidxIndex (arcidx_index.go, unxed/tar#9). TarFS.Index
// (fs.go) is declared as this interface, not as a concrete *Index, so a
// single TarFS value can hold either backend - chosen explicitly via
// WithFSIndexBackend, or left to the tarindex_simple build tag as before -
// without needing two different builds of the tar package at once.
//
// Every other caller of OpenIndex (writer.go, indexer_enabled.go) is
// untouched: they keep using the tag-bound Index/OpenIndex names directly,
// exactly as before this file was added.
type FileIndex interface {
	Close() error
	InitMetadata() error
	Insert(nodes []FileNode) error
	Lookup(p string) (*FileNode, error)
	List(p string) ([]FileNode, error)
	RecursiveSize(p string) (int64, error)
	InsertBlockOffsets(table string, offsets []BlockOffset) error
	GetClosestBlockOffset(table string, targetDataOffset int64) (*BlockOffset, error)
	GetGzipIndex() ([]byte, error)
	SaveGzipIndex(data []byte) error
}

// IndexBackend explicitly selects which FileIndex implementation NewFS opens
// for a given TarFS, independent of the tarindex_simple build tag. Pass one
// via WithFSIndexBackend.
type IndexBackend int

const (
	// IndexBackendAuto (the default) keeps the pre-existing, tag-bound
	// behavior: NewFS opens whatever OpenIndex/Index currently name for this
	// build - the SQLite backend, unless the binary was built with
	// -tags tarindex_simple, in which case ArcidxIndex.
	IndexBackendAuto IndexBackend = iota
	// IndexBackendSQLite always opens the SQLite-backed Index, regardless of
	// the tarindex_simple tag. Only functional in builds that actually
	// compiled the SQLite backend in: on a tarindex_simple build it returns
	// ErrSQLiteBackendUnavailable (see tarindex_simple.go), and on the BSD/
	// Solaris/illumos OSes without cgo-free SQLite it returns the same
	// errNoSqlite the tag-bound OpenIndex already returns there
	// (sqlite_disabled.go) - not a new limitation, just reachable
	// explicitly now.
	IndexBackendSQLite
	// IndexBackendArcidx always opens the FlatBuffers-backed ArcidxIndex.
	// arcidx_index.go has no build tag of its own, so this works in every
	// build, including ones that also have the SQLite backend compiled in.
	IndexBackendArcidx
)

// openIndexForBackend is NewFS's single entry point into whichever backend
// was requested (fs.go's prepareIndex calls this instead of calling
// OpenIndex directly).
func openIndexForBackend(backend IndexBackend, dsn string) (FileIndex, error) {
	switch backend {
	case IndexBackendArcidx:
		return OpenArcidxIndex(dsn)
	case IndexBackendSQLite:
		return openSQLiteIndex(dsn)
	default:
		return OpenIndex(dsn)
	}
}
