//go:build (freebsd || openbsd || netbsd || dragonfly || solaris || illumos) && !tarindex_simple

package tar

// errNoSqlite is declared in indexer_disabled.go (which, unlike this file,
// has no tarindex_simple exclusion - see its comment for why) and reused
// here.

type Index struct{}

// openSQLiteIndex backs IndexBackendSQLite (index_backend.go) on this OS set:
// SQLite was never wired up here (see errNoSqlite's comment above), so it
// returns the same error OpenIndex already does - explicit selection doesn't
// unlock anything OpenIndex couldn't already do on these platforms.
func openSQLiteIndex(dsn string) (FileIndex, error) { return OpenIndex(dsn) }

func OpenIndex(dsn string) (*Index, error)                                      { return nil, errNoSqlite }
func (idx *Index) Close() error                                                 { return nil }
func (idx *Index) InitMetadata() error                                          { return nil }
func (idx *Index) Insert(nodes []FileNode) error                                { return errNoSqlite }
func (idx *Index) Lookup(p string) (*FileNode, error)                           { return nil, errNoSqlite }
func (idx *Index) List(p string) ([]FileNode, error)                            { return nil, errNoSqlite }
func (idx *Index) RecursiveSize(p string) (int64, error)                        { return 0, errNoSqlite }
func (idx *Index) InsertBlockOffsets(table string, offsets []BlockOffset) error { return errNoSqlite }
func (idx *Index) GetClosestBlockOffset(table string, targetDataOffset int64) (*BlockOffset, error) {
	return nil, errNoSqlite
}
func (idx *Index) GetGzipIndex() ([]byte, error)   { return nil, errNoSqlite }
func (idx *Index) SaveGzipIndex(data []byte) error { return errNoSqlite }
