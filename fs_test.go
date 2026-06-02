//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos
package tar

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"encoding/binary"
	"runtime"
	"strings"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

func TestTarFS(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "folder"), 0755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello random access"), 0644)
	os.WriteFile(filepath.Join(srcDir, "folder", "sub.txt"), []byte("ratarmount is awesome"), 0644)

	tarPath := filepath.Join(tmpDir, "test.tar.zst")
	err := Compress(srcDir, tarPath)
	if err != nil {
		t.Fatal(err)
	}

	// Mount it
	idxPath := filepath.Join(tmpDir, "test.tar.zst.index.sqlite")
	tfs, err := NewFS(tarPath, idxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// Test Random Access / ReadFile
	b, err := fs.ReadFile(tfs, "src/file.txt")
	if err != nil || string(b) != "hello random access" {
		t.Errorf("ReadFile file.txt failed: %v, %s", err, string(b))
	}

	b, err = fs.ReadFile(tfs, "src/folder/sub.txt")
	if err != nil || string(b) != "ratarmount is awesome" {
		t.Errorf("ReadFile folder/sub.txt failed: %v, %s", err, string(b))
	}

	// Test WalkDir
	var paths []string
	err = fs.WalkDir(tfs, ".", func(path string, d fs.DirEntry, err error) error {
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, p := range paths {
		if p == "src/folder/sub.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("WalkDir didn't find folder/sub.txt, got %v", paths)
	}

	// Test RecursiveSize calculation on the index
	size, err := tfs.RecursiveSize("src")
	if err != nil {
		t.Fatalf("RecursiveSize failed: %v", err)
	}
	expectedSize := int64(len("hello random access") + len("ratarmount is awesome"))
	if size != expectedSize {
		t.Errorf("Expected recursive size of 'src' to be %d, got %d", expectedSize, size)
	}
}
func TestTarFS_DefaultCache(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "cache_test.tar")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "test.txt", Size: 4})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	tfs, err := NewFS(archivePath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()
	defer os.Remove(tfs.IndexPath)

	if tfs.IndexPath == "" {
		t.Error("Expected automatic IndexPath to be non-empty")
	}

	if _, err := os.Stat(tfs.IndexPath); os.IsNotExist(err) {
		t.Errorf("Standard index file not created at %s", tfs.IndexPath)
	}
}
func TestGetStandardIndexPath_SidecarAndFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Test Writable Directory (Should use Sidecar)
	writableArchive := filepath.Join(tmpDir, "writable_archive.tar")
	idxPath, err := GetStandardIndexPath(writableArchive)
	if err != nil {
		t.Fatalf("Failed to get index path: %v", err)
	}
	expectedSidecar := writableArchive + ".index.sqlite"
	if idxPath != expectedSidecar {
		t.Errorf("Expected sidecar path %q, got %q", expectedSidecar, idxPath)
	}
	os.Remove(idxPath)

	// 2. Test Read-Only Directory (Should Fallback to Cache)
	if runtime.GOOS != "windows" {
		readOnlyDir := filepath.Join(tmpDir, "readonly")
		if err := os.MkdirAll(readOnlyDir, 0555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(readOnlyDir, 0755)

		readOnlyArchive := filepath.Join(readOnlyDir, "readonly_archive.tar")
		idxPathFallback, err := GetStandardIndexPath(readOnlyArchive)
		if err != nil {
			t.Fatalf("Failed to get fallback index path: %v", err)
		}
		if idxPathFallback == readOnlyArchive+".index.sqlite" {
			t.Error("Expected index path to fall back to cache directory, but got sidecar path in read-only directory")
		}
		if !strings.Contains(idxPathFallback, "ratarmount") && !strings.Contains(idxPathFallback, ".cache") {
			t.Errorf("Expected fallback path to be in cache directory, got %q", idxPathFallback)
		}
	}
}

// TestZstdFastPath verifies that TarFS successfully utilizes saved zstdblocks
// to perform O(1) random-access seeking in multi-frame ZSTD archives without CGO.
func TestZstdFastPath(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "fast_zstd.tar.zst")
	indexPath := filepath.Join(tmpDir, "fast_zstd.sqlite")

	// 1. Create a standard uncompressed TAR in memory
	tarBuf := new(bytes.Buffer)
	tw := NewWriter(tarBuf)

	// Write file1 (4000 bytes of 'A')
	data1 := bytes.Repeat([]byte("A"), 4000)
	if err := tw.WriteHeader(&Header{Name: "file1.txt", Size: 4000, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(data1)

	// Write file2 (1000 bytes of 'B') - this will cross the 3000 byte boundary
	data2 := bytes.Repeat([]byte("B"), 1000)
	if err := tw.WriteHeader(&Header{Name: "file2.txt", Size: 1000, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(data2)
	tw.Close()

	tarBytes := tarBuf.Bytes()
	splitOffset := int64(3000) // Split point inside file1's data blocks

	// 2. Compress the TAR into 2 independent ZSTD frames manually
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	// Frame 1 (contains file1 header and first part of data)
	z1, _ := zstd.NewWriter(f)
	z1.Write(tarBytes[:splitOffset])
	z1.Close()

	fi, _ := f.Stat()
	frame2Start := fi.Size() // Compressed offset of Frame 2

	// Frame 2 (contains the rest of file1 data, file2 header, file2 data, and EOF)
	z2, _ := zstd.NewWriter(f)
	z2.Write(tarBytes[splitOffset:])
	z2.Close()
	f.Close()

	// 3. Open the archive with TarFS (which indexes the TAR and creates empty zstdblocks)
	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// 4. Manually insert the seek point for Frame 2 into SQLite.
	// This simulates having a pre-existing index from Python ratarmount or a specialized exporter.
	_, err = tfs.Index.db.Exec(`INSERT INTO zstdblocks (blockoffset, dataoffset) VALUES (?, ?)`, frame2Start, splitOffset)
	if err != nil {
		t.Fatal(err)
	}

	// 5. Lookup and read file2.txt, which lies completely inside Frame 2
	node, err := tfs.Index.Lookup("file2.txt")
	if err != nil {
		t.Fatal(err)
	}

	if node.Offset < splitOffset {
		t.Fatalf("Expected file2.txt offset to be > %d, got %d", splitOffset, node.Offset)
	}

	// Read using the O(1) fast path.
	// Internally, tfs.Open will seek to frame2Start, start decompression from there,
	// and skip only (node.Offset - 3000) bytes!
	content, err := fs.ReadFile(tfs, "file2.txt")
	if err != nil {
		t.Fatalf("ReadFile file2.txt failed: %v", err)
	}

	if !bytes.Equal(content, data2) {
		t.Errorf("Content mismatch: expected 1000 'B's, got %s", string(content[:10])+"...")
	}
}

// TestCacheCompatibility programmatically verifies that our SQLite database schema
// matches the ratarmount/gztool specifications down to the exact table and column structures.
func TestCacheCompatibility(t *testing.T) {
	tmpDir := t.TempDir()
	indexPath := filepath.Join(tmpDir, "compat.sqlite")

	idx, err := OpenIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// 1. Verify existence of all 6 standard ratarmount tables
	expectedTables := map[string]bool{
		"files":       false,
		"metadata":    false,
		"versions":    false,
		"zstdblocks":  false,
		"bzip2blocks": false,
		"gzipindexes": false,
	}

	rows, err := idx.db.Query("SELECT name FROM sqlite_master WHERE type='table' OR type='view'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if _, ok := expectedTables[name]; ok {
			expectedTables[name] = true
		}
	}

	for name, exists := range expectedTables {
		if !exists {
			t.Errorf("Missing required ratarmount table: %q", name)
		}
	}

	// 2. Verify all 15 required columns in the "files" table
	expectedFilesColumns := map[string]bool{
		"path":           false,
		"name":           false,
		"offsetheader":   false,
		"offset":         false,
		"size":           false,
		"mtime":          false,
		"mode":           false,
		"type":           false,
		"linkname":       false,
		"uid":            false,
		"gid":            false,
		"istar":          false,
		"issparse":       false,
		"isgenerated":    false,
		"recursiondepth": false,
	}

	infoRows, err := idx.db.Query("PRAGMA table_info(files)")
	if err != nil {
		t.Fatal(err)
	}
	defer infoRows.Close()

	for infoRows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue any
		if err := infoRows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			t.Fatal(err)
		}
		if _, ok := expectedFilesColumns[name]; ok {
			expectedFilesColumns[name] = true
		}
	}

	for name, exists := range expectedFilesColumns {
		if !exists {
			t.Errorf("Missing required ratarmount column %q in 'files' table", name)
		}
	}
}
// TestGzipFastPath verifies that TarFS successfully performs O(1) random-access
// seeking in GZIP archives by index-checkpointing on pure Go without CGO.
func TestGzipFastPath(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "fast_gzip.tar.gz")
	indexPath := filepath.Join(tmpDir, "fast_gzip.sqlite")

	// 1. Create a raw TAR in memory
	tarBuf := new(bytes.Buffer)
	tw := NewWriter(tarBuf)

	// Write file1 (4000 bytes of 'A')
	data1 := bytes.Repeat([]byte("A"), 4000)
	if err := tw.WriteHeader(&Header{Name: "file1.txt", Size: 4000, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(data1)

	// Write file2 (1000 bytes of 'B')
	data2 := bytes.Repeat([]byte("B"), 1000)
	if err := tw.WriteHeader(&Header{Name: "file2.txt", Size: 1000, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(data2)
	tw.Close()

	tarBytes := tarBuf.Bytes()
	splitOffset := int64(3000)

	// 2. Compress the TAR using GZIP in 2 streams (Concatenated GZIP)
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	// Stream 1
	gw1 := gzip.NewWriter(f)
	gw1.Write(tarBytes[:splitOffset])
	gw1.Close()

	fi, _ := f.Stat()
	stream2Start := fi.Size() // Compressed offset of Stream 2

	// Stream 2
	gw2 := gzip.NewWriter(f)
	gw2.Write(tarBytes[splitOffset:])
	gw2.Close()
	f.Close()

	// 3. Open the archive with TarFS
	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// 4. Manually construct and serialize a GZIDX index (compatible with zran.c / ratarmount)
	gzidxBuf := new(bytes.Buffer)
	gzidxBuf.Write([]byte("GZIDX"))
	gzidxBuf.WriteByte(1) // version
	gzidxBuf.WriteByte(0) // flags

	// Sizes and counts
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(stream2Start+10))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(len(tarBytes)))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(1024*1024))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(32768))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(2)) // 2 seek points

	// Point 0 (at offset 0)
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(0)) // comp offset
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(0)) // uncomp offset
	gzidxBuf.WriteByte(0)                                  // bits
	gzidxBuf.WriteByte(0)                                  // hasData = 0

	// Point 1 (at offset 3000, start of Stream 2). Since it's a new GZIP stream,
	// it starts at a byte boundary (bits=0) and needs no prior window dictionary (hasData=0).
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(stream2Start)) // comp offset
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(splitOffset))  // uncomp offset
	gzidxBuf.WriteByte(0)                                             // bits
	gzidxBuf.WriteByte(0)                                             // hasData = 0

	// Compress the GZIDX index with GZIP
	var cmpBuf bytes.Buffer
	gw := gzip.NewWriter(&cmpBuf)
	gw.Write(gzidxBuf.Bytes())
	gw.Close()

	// Save to SQLite
	err = tfs.Index.SaveGzipIndex(cmpBuf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	// 5. Read file2.txt (located inside Stream 2)
	content, err := fs.ReadFile(tfs, "file2.txt")
	if err != nil {
		t.Fatalf("ReadFile file2.txt failed: %v", err)
	}

	if !bytes.Equal(content, data2) {
		t.Errorf("Content mismatch: expected 1000 'B's, got %s", string(content[:10])+"...")
	}
}

