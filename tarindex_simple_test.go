//go:build tarindex_simple

package tar

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTarindexSimpleIndexIsArcidxIndex is a compile-time-flavoured sanity
// check that, under this build tag, Index really is ArcidxIndex and not
// some other type that merely happens to satisfy the same method set: a
// *Index assigned to a *ArcidxIndex variable (and vice versa) only compiles
// at all if the two are the same type, which is exactly what the `type Index
// = ArcidxIndex` alias in tarindex_simple.go guarantees.
func TestTarindexSimpleIndexIsArcidxIndex(t *testing.T) {
	var viaIndex *Index = &ArcidxIndex{}
	var viaArcidx *ArcidxIndex = viaIndex
	if viaArcidx == nil {
		t.Fatal("unreachable: nil check only exists to use the variable")
	}
}

// TestTarindexSimpleOpenIndexRoundTrip exercises OpenIndex/Insert/Lookup/
// List/RecursiveSize/Close/re-open exactly like sqlite_test.go's
// TestIndexXattrsAndAcl and arcidx_index_test.go's
// TestArcidxCreateInsertLookup do for their own backends, so that the
// tarindex_simple build tag has its own direct coverage of the OpenIndex
// entry point (the one thing tarindex_simple.go actually adds), rather than
// relying purely on inference from sqlite_test.go (which also happens to
// compile under this tag, since it only uses the shared public API) or from
// arcidx_index_test.go (which exercises ArcidxIndex directly, not through
// the Index/OpenIndex names).
func TestTarindexSimpleOpenIndexRoundTrip(t *testing.T) {
	idxPath := filepath.Join(t.TempDir(), "tarindex_simple_roundtrip.index")

	idx, err := OpenIndex(idxPath)
	if err != nil {
		t.Fatalf("OpenIndex failed: %v", err)
	}
	if err := idx.InitMetadata(); err != nil {
		t.Fatalf("InitMetadata failed: %v", err)
	}

	now := time.Now()
	nodes := []FileNode{
		{Path: "/", Name: "dir", OffsetHeader: 0, Offset: 512, Size: 0, Mode: 0755 | 040000, ModTime: now, Type: TypeDir},
		{Path: "/dir", Name: "file.txt", OffsetHeader: 512, Offset: 1024, Size: 42, Mode: 0644, ModTime: now, Type: TypeReg, Uid: 1000, Gid: 1000,
			Xattrs: map[string][]byte{"user.test": []byte("value")},
			Acl:    []byte{0x01, 0x02, 0x03},
		},
	}
	if err := idx.Insert(nodes); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	got, err := idx.Lookup("/dir/file.txt")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if got.Size != 42 || got.Uid != 1000 || got.OffsetHeader != 512 {
		t.Errorf("Lookup returned unexpected node: %+v", got)
	}
	if string(got.Xattrs["user.test"]) != "value" {
		t.Errorf("Xattrs mismatch: got %v", got.Xattrs)
	}
	if len(got.Acl) != 3 || got.Acl[0] != 0x01 {
		t.Errorf("Acl mismatch: got %v", got.Acl)
	}

	listing, err := idx.List("/")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listing) != 1 || listing[0].Name != "dir" {
		t.Errorf("List(/) returned unexpected entries: %+v", listing)
	}

	size, err := idx.RecursiveSize("/")
	if err != nil {
		t.Fatalf("RecursiveSize failed: %v", err)
	}
	if size != 42 {
		t.Errorf("RecursiveSize(/) = %d, want 42", size)
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Re-open through OpenIndex again (not NewArcidxIndex) to confirm the
	// written file round-trips through the same entry point callers use.
	reopened, err := OpenIndex(idxPath)
	if err != nil {
		t.Fatalf("re-OpenIndex failed: %v", err)
	}
	defer reopened.Close()

	got2, err := reopened.Lookup("/dir/file.txt")
	if err != nil {
		t.Fatalf("Lookup after reopen failed: %v", err)
	}
	if got2.Size != 42 || got2.OffsetHeader != 512 {
		t.Errorf("Lookup after reopen returned unexpected node: %+v", got2)
	}

	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("expected index file to exist on disk: %v", err)
	}
}

// TestTarindexSimpleGzipAndBlockOffsets exercises the block-offset and
// gzip-index side of the API (InsertBlockOffsets/GetClosestBlockOffset/
// SaveGzipIndex/GetGzipIndex), mirroring what fs.go's random-access code
// path and IndexArchive's on-the-fly GZIP indexing rely on, so this build
// tag has coverage of that surface too, not just Insert/Lookup/List.
func TestTarindexSimpleGzipAndBlockOffsets(t *testing.T) {
	idxPath := filepath.Join(t.TempDir(), "tarindex_simple_blocks.index")

	idx, err := OpenIndex(idxPath)
	if err != nil {
		t.Fatalf("OpenIndex failed: %v", err)
	}
	defer idx.Close()

	offsets := []BlockOffset{
		{BlockOffset: 0, DataOffset: 0},
		{BlockOffset: 100, DataOffset: 1000},
		{BlockOffset: 200, DataOffset: 2000},
	}
	if err := idx.InsertBlockOffsets("zstdblocks", offsets); err != nil {
		t.Fatalf("InsertBlockOffsets failed: %v", err)
	}

	bo, err := idx.GetClosestBlockOffset("zstdblocks", 1500)
	if err != nil {
		t.Fatalf("GetClosestBlockOffset failed: %v", err)
	}
	if bo.BlockOffset != 100 || bo.DataOffset != 1000 {
		t.Errorf("GetClosestBlockOffset(1500) = %+v, want {100 1000}", bo)
	}

	gzipData := []byte("fake GZIDX payload for round-trip purposes")
	if err := idx.SaveGzipIndex(gzipData); err != nil {
		t.Fatalf("SaveGzipIndex failed: %v", err)
	}
	got, err := idx.GetGzipIndex()
	if err != nil {
		t.Fatalf("GetGzipIndex failed: %v", err)
	}
	if string(got) != string(gzipData) {
		t.Errorf("GetGzipIndex = %q, want %q", got, gzipData)
	}
}
