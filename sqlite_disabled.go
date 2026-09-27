//go:build (freebsd || openbsd || netbsd || dragonfly || solaris || illumos) && !tarindex_simple

package tar

// errNoSqlite is declared in indexer_disabled.go (which, unlike this file,
// has no tarindex_simple exclusion - see its comment for why) and reused
// here.

type Index struct{}

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
