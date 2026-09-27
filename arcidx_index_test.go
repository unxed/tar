package tar

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustTempArcidxPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.arcidx")
}

// TestArcidxCreateInsertLookup covers the basic write-then-read cycle: create
// a fresh index, insert a handful of files and directories, and look them up
// while the index is still open (materialized, not-yet-flushed state).
func TestArcidxCreateInsertLookup(t *testing.T) {
	path := mustTempArcidxPath(t)

	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}
	if err := idx.InitMetadata(); err != nil {
		t.Fatalf("InitMetadata: %v", err)
	}

	now := time.Now()
	nodes := []FileNode{
		{Path: "/", Name: "dir", OffsetHeader: 0, Offset: 512, Size: 0, Mode: 0755 | 040000, ModTime: now, Type: TypeDir},
		{Path: "/dir", Name: "file.txt", OffsetHeader: 512, Offset: 1024, Size: 42, Mode: 0644, ModTime: now, Type: TypeReg, Uid: 1000, Gid: 1000},
	}
	if err := idx.Insert(nodes); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := idx.Lookup("/dir/file.txt")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Size != 42 || got.Uid != 1000 || got.OffsetHeader != 512 {
		t.Errorf("Lookup returned unexpected node: %+v", got)
	}

	root, err := idx.Lookup("/")
	if err != nil {
		t.Fatalf("Lookup(/): %v", err)
	}
	if !root.IsGenerated || root.Type != TypeDir {
		t.Errorf("Lookup(/) returned unexpected synthetic root: %+v", root)
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestArcidxCloseReopenLookup verifies that an index survives a Close and a
// fresh OpenArcidxIndex in a different *ArcidxIndex value (simulating a
// different process), and that Lookup/List/RecursiveSize on the reopened,
// zero-copy index return the same data as before Close.
func TestArcidxCloseReopenLookup(t *testing.T) {
	path := mustTempArcidxPath(t)

	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}
	now := time.Unix(1700000000, 123456789)
	nodes := []FileNode{
		{Path: "/", Name: "a", OffsetHeader: 0, Size: 0, Mode: 0755 | 040000, Type: TypeDir},
		{Path: "/a", Name: "b", OffsetHeader: 0, Size: 0, Mode: 0755 | 040000, Type: TypeDir},
		{Path: "/a/b", Name: "f1.txt", OffsetHeader: 100, Offset: 200, Size: 7, Mode: 0644, ModTime: now, Type: TypeReg,
			Xattrs: map[string][]byte{"user.note": []byte("hello")}, Acl: []byte{1, 2, 3}, LinkName: ""},
		{Path: "/a/b", Name: "f2.txt", OffsetHeader: 300, Offset: 400, Size: 13, Mode: 0644, ModTime: now, Type: TypeReg},
	}
	if err := idx.Insert(nodes); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	defer reopened.Close()

	node, err := reopened.Lookup("/a/b/f1.txt")
	if err != nil {
		t.Fatalf("Lookup after reopen: %v", err)
	}
	if node.Size != 7 || string(node.Xattrs["user.note"]) != "hello" || len(node.Acl) != 3 {
		t.Errorf("reopened Lookup mismatch: %+v", node)
	}
	if !node.ModTime.Equal(now) {
		t.Errorf("mtime mismatch after reopen: got %v want %v", node.ModTime, now)
	}

	kids, err := reopened.List("/a/b")
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(kids) != 2 {
		t.Fatalf("List after reopen: got %d entries, want 2: %+v", len(kids), kids)
	}

	size, err := reopened.RecursiveSize("/a")
	if err != nil {
		t.Fatalf("RecursiveSize after reopen: %v", err)
	}
	if size != 20 {
		t.Errorf("RecursiveSize after reopen = %d, want 20", size)
	}
}

// TestArcidxDeduplication inserts many files that share the same directory,
// metadata (mode/uid/gid/type/flags) and xattr key, and checks that the
// on-disk index actually stores one copy of each, not one per file.
func TestArcidxDeduplication(t *testing.T) {
	path := mustTempArcidxPath(t)

	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}

	const numFiles = 50
	nodes := make([]FileNode, 0, numFiles+1)
	nodes = append(nodes, FileNode{Path: "/", Name: "dir", OffsetHeader: 0, Mode: 0755 | 040000, Type: TypeDir})
	for i := 0; i < numFiles; i++ {
		nodes = append(nodes, FileNode{
			Path: "/dir", Name: fmt.Sprintf("file_%d.txt", i),
			OffsetHeader: int64(1000 + i), Offset: int64(2000 + i), Size: 10,
			Mode: 0644, Uid: 1000, Gid: 1000, Type: TypeReg,
			Xattrs: map[string][]byte{"user.same": []byte("same-value")},
		})
	}
	if err := idx.Insert(nodes); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	defer reopened.Close()

	root := reopened.root
	if root == nil {
		t.Fatal("expected a zero-copy root on reopen, got nil (index was materialized eagerly?)")
	}

	// One path for "/", one for "/dir" -> 2, regardless of numFiles.
	if got := root.PathsLength(); got != 2 {
		t.Errorf("PathsLength = %d, want 2 (dedup failed)", got)
	}
	// One xattr key ("user.same") shared by all numFiles files.
	if got := root.XattrKeysLength(); got != 1 {
		t.Errorf("XattrKeysLength = %d, want 1 (dedup failed)", got)
	}
	// All numFiles regular files share one MetadataTuple (mode/uid/gid/type
	// are identical), plus one more for the synthetic directory: 2 total.
	if got := root.MetadataTuplesLength(); got != 2 {
		t.Errorf("MetadataTuplesLength = %d, want 2 (dedup failed)", got)
	}
	// But every individual file is still present.
	if got := root.FilesLength(); got != numFiles+1 {
		t.Errorf("FilesLength = %d, want %d (files themselves must not be deduplicated)", got, numFiles+1)
	}
}

// TestArcidxPrefixLookupReadDir checks the ReadDir-like prefix lookup (List)
// against a small directory tree, including that only the *latest* version
// of an overwritten file is returned.
func TestArcidxPrefixLookupReadDir(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}

	nodes := []FileNode{
		{Path: "/", Name: "dir", OffsetHeader: 0, Mode: 0755 | 040000, Type: TypeDir},
		{Path: "/dir", Name: "a.txt", OffsetHeader: 10, Size: 1, Mode: 0644, Type: TypeReg},
		{Path: "/dir", Name: "b.txt", OffsetHeader: 20, Size: 2, Mode: 0644, Type: TypeReg},
		// b.txt appended a second time later in the archive: this later
		// occurrence (higher OffsetHeader) must win.
		{Path: "/dir", Name: "b.txt", OffsetHeader: 30, Size: 99, Mode: 0644, Type: TypeReg},
		{Path: "/dir", Name: "sub", OffsetHeader: 40, Mode: 0755 | 040000, Type: TypeDir},
		{Path: "/dir/sub", Name: "c.txt", OffsetHeader: 50, Size: 3, Mode: 0644, Type: TypeReg},
	}
	if err := idx.Insert(nodes); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Exercise List while still materialized (not yet closed).
	kids, err := idx.List("/dir")
	if err != nil {
		t.Fatalf("List (materialized): %v", err)
	}
	assertListDirContents(t, kids)

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	defer reopened.Close()

	kids, err = reopened.List("/dir")
	if err != nil {
		t.Fatalf("List (zero-copy): %v", err)
	}
	assertListDirContents(t, kids)
}

func assertListDirContents(t *testing.T, kids []FileNode) {
	t.Helper()
	if len(kids) != 3 {
		t.Fatalf("List(/dir) returned %d entries, want 3: %+v", len(kids), kids)
	}
	byName := make(map[string]FileNode, len(kids))
	for _, k := range kids {
		byName[k.Name] = k
	}
	if b, ok := byName["b.txt"]; !ok || b.Size != 99 || b.OffsetHeader != 30 {
		t.Errorf("b.txt should be the latest (offset 30, size 99) version, got %+v", b)
	}
	if a, ok := byName["a.txt"]; !ok || a.Size != 1 {
		t.Errorf("a.txt mismatch: %+v", a)
	}
	if s, ok := byName["sub"]; !ok || s.Type != TypeDir {
		t.Errorf("sub directory mismatch: %+v", s)
	}
}

// TestArcidxRecursiveSize checks the recursive-subtree-size operation,
// including the whole-archive ("/") case and an overwritten file, which
// must be counted only once (at its latest size).
func TestArcidxRecursiveSize(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}

	nodes := []FileNode{
		{Path: "/", Name: "dir", OffsetHeader: 0, Mode: 0755 | 040000, Type: TypeDir},
		{Path: "/dir", Name: "a.txt", OffsetHeader: 10, Size: 5, Mode: 0644, Type: TypeReg},
		{Path: "/dir", Name: "sub", OffsetHeader: 20, Mode: 0755 | 040000, Type: TypeDir},
		{Path: "/dir/sub", Name: "b.txt", OffsetHeader: 30, Size: 7, Mode: 0644, Type: TypeReg},
		// Overwritten later with a different size; only 11 should count.
		{Path: "/dir/sub", Name: "b.txt", OffsetHeader: 40, Size: 11, Mode: 0644, Type: TypeReg},
		{Path: "/", Name: "other.txt", OffsetHeader: 50, Size: 1000, Mode: 0644, Type: TypeReg},
	}
	if err := idx.Insert(nodes); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	checkSizes := func(label string, i *ArcidxIndex) {
		if got, err := i.RecursiveSize("/dir"); err != nil || got != 16 {
			t.Errorf("%s: RecursiveSize(/dir) = %d, err=%v; want 16", label, got, err)
		}
		if got, err := i.RecursiveSize("/dir/sub"); err != nil || got != 11 {
			t.Errorf("%s: RecursiveSize(/dir/sub) = %d, err=%v; want 11", label, got, err)
		}
		if got, err := i.RecursiveSize("/"); err != nil || got != 1016 {
			t.Errorf("%s: RecursiveSize(/) = %d, err=%v; want 1016", label, got, err)
		}
	}

	checkSizes("materialized", idx)

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	defer reopened.Close()
	checkSizes("zero-copy", reopened)
}

// TestArcidxCloseReopenAppend verifies that an index opened for reading can
// still be appended to (materializing lazily) and rewritten on Close,
// without losing previously-flushed entries.
func TestArcidxCloseReopenAppend(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}
	if err := idx.Insert([]FileNode{{Path: "/", Name: "old.txt", OffsetHeader: 1, Size: 1, Mode: 0644, Type: TypeReg}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	if err := reopened.Insert([]FileNode{{Path: "/", Name: "new.txt", OffsetHeader: 2, Size: 2, Mode: 0644, Type: TypeReg}}); err != nil {
		t.Fatalf("Insert on reopened index: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	final, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex (final): %v", err)
	}
	defer final.Close()
	if _, err := final.Lookup("/old.txt"); err != nil {
		t.Errorf("old.txt lost after append+close: %v", err)
	}
	if _, err := final.Lookup("/new.txt"); err != nil {
		t.Errorf("new.txt missing after append+close: %v", err)
	}
}

// TestArcidxBlockOffsetsAndGzipIndex exercises InsertBlockOffsets /
// GetClosestBlockOffset and SaveGzipIndex / GetGzipIndex, both immediately
// and after a Close+reopen round trip.
func TestArcidxBlockOffsetsAndGzipIndex(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}

	offsets := []BlockOffset{
		{BlockOffset: 0, DataOffset: 0},
		{BlockOffset: 100, DataOffset: 1000},
		{BlockOffset: 200, DataOffset: 2000},
	}
	if err := idx.InsertBlockOffsets("zstdblocks", offsets); err != nil {
		t.Fatalf("InsertBlockOffsets: %v", err)
	}
	bo, err := idx.GetClosestBlockOffset("zstdblocks", 1500)
	if err != nil {
		t.Fatalf("GetClosestBlockOffset: %v", err)
	}
	if bo.BlockOffset != 100 || bo.DataOffset != 1000 {
		t.Errorf("GetClosestBlockOffset(1500) = %+v, want {100 1000}", bo)
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	bo, err = reopened.GetClosestBlockOffset("zstdblocks", 2500)
	if err != nil {
		t.Fatalf("GetClosestBlockOffset after reopen: %v", err)
	}
	if bo.BlockOffset != 200 || bo.DataOffset != 2000 {
		t.Errorf("GetClosestBlockOffset(2500) after reopen = %+v, want {200 2000}", bo)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// SaveGzipIndex/GetGzipIndex on a separate, fresh index.
	gzPath := mustTempArcidxPath(t)
	gzIdx, err := NewArcidxIndex(gzPath)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}
	blob := []byte("fake gzip random-access index data")
	if err := gzIdx.SaveGzipIndex(blob); err != nil {
		t.Fatalf("SaveGzipIndex: %v", err)
	}
	got, err := gzIdx.GetGzipIndex()
	if err != nil || string(got) != string(blob) {
		t.Errorf("GetGzipIndex = %q, err=%v; want %q", got, err, blob)
	}
	if err := gzIdx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	gzReopened, err := OpenArcidxIndex(gzPath)
	if err != nil {
		t.Fatalf("OpenArcidxIndex: %v", err)
	}
	defer gzReopened.Close()
	got, err = gzReopened.GetGzipIndex()
	if err != nil || string(got) != string(blob) {
		t.Errorf("GetGzipIndex after reopen = %q, err=%v; want %q", got, err, blob)
	}
}

// TestArcidxOpenNonexistent verifies that opening a path that does not
// exist yet behaves like creating a fresh, empty index (mirrors OpenIndex's
// CREATE TABLE IF NOT EXISTS semantics), not an error.
func TestArcidxOpenNonexistent(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex on nonexistent path: %v", err)
	}
	defer idx.Close()

	if _, err := idx.Lookup("/nope"); err != ErrArcidxNotFound {
		t.Errorf("Lookup on fresh empty index = %v, want ErrArcidxNotFound", err)
	}
	kids, err := idx.List("/")
	if err != nil || len(kids) != 0 {
		t.Errorf("List(/) on fresh empty index = %v, %v; want empty, nil", kids, err)
	}
}

// TestArcidxOpenEmptyFile checks that an existing but zero-byte file (e.g.
// created but never written) is treated the same way as a missing file,
// not as corruption.
func TestArcidxOpenEmptyFile(t *testing.T) {
	path := mustTempArcidxPath(t)
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	idx, err := OpenArcidxIndex(path)
	if err != nil {
		t.Fatalf("OpenArcidxIndex on empty file: %v", err)
	}
	defer idx.Close()
	if _, err := idx.Lookup("/nope"); err != ErrArcidxNotFound {
		t.Errorf("Lookup on empty-file index = %v, want ErrArcidxNotFound", err)
	}
}

// TestArcidxOpenCorrupted checks that garbage bytes and a truncated-but
// previously-valid file are both reported as errors, not panics, and not
// silently treated as an empty index.
func TestArcidxOpenCorrupted(t *testing.T) {
	t.Run("random garbage", func(t *testing.T) {
		path := mustTempArcidxPath(t)
		garbage := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
		if err := os.WriteFile(path, garbage, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := OpenArcidxIndex(path); err == nil {
			t.Error("OpenArcidxIndex on random garbage: expected an error, got nil")
		}
	})

	t.Run("too short", func(t *testing.T) {
		path := mustTempArcidxPath(t)
		if err := os.WriteFile(path, []byte{0x01, 0x02, 0x03}, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := OpenArcidxIndex(path); err == nil {
			t.Error("OpenArcidxIndex on a too-short buffer: expected an error, got nil")
		}
	})

	t.Run("truncated valid index", func(t *testing.T) {
		path := mustTempArcidxPath(t)
		idx, err := NewArcidxIndex(path)
		if err != nil {
			t.Fatalf("NewArcidxIndex: %v", err)
		}
		if err := idx.Insert([]FileNode{
			{Path: "/", Name: "file.txt", OffsetHeader: 1, Size: 1, Mode: 0644, Type: TypeReg,
				Xattrs: map[string][]byte{"user.a": []byte("b")}},
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := idx.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		truncated := data[:len(data)/2]
		if err := os.WriteFile(path, truncated, 0644); err != nil {
			t.Fatalf("WriteFile (truncated): %v", err)
		}

		if _, err := OpenArcidxIndex(path); err == nil {
			t.Error("OpenArcidxIndex on a truncated valid index: expected an error, got nil")
		}
	})
}

// TestArcidxCloseTwiceIsIdempotent mirrors sql.DB.Close's idempotency.
func TestArcidxCloseTwiceIsIdempotent(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestArcidxUseAfterClose checks that methods fail predictably (not panic)
// once the index has been closed.
func TestArcidxUseAfterClose(t *testing.T) {
	path := mustTempArcidxPath(t)
	idx, err := NewArcidxIndex(path)
	if err != nil {
		t.Fatalf("NewArcidxIndex: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := idx.Insert(nil); err != errArcidxClosed {
		t.Errorf("Insert after Close = %v, want errArcidxClosed", err)
	}
	if _, err := idx.Lookup("/x"); err != errArcidxClosed {
		t.Errorf("Lookup after Close = %v, want errArcidxClosed", err)
	}
}
