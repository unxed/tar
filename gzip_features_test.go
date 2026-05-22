package tar

import (
	"bytes"
	"context"
	"compress/gzip"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestGzipIndexImporter_InterfaceAndFastPath verifies that gzipFormat
// implements the corrected GzipIndexImporter interface.
func TestGzipIndexImporter_InterfaceAndFastPath(t *testing.T) {
	var di any = gzipFormat{}
	_, ok := di.(GzipIndexImporter)
	if !ok {
		t.Fatal("gzipFormat does not implement the GzipIndexImporter interface")
	}
}

// TestGzipOnTheFlyIndexGeneration verifies that IndexArchive generates
// a valid GZIDX index when indexing GZIP archives on-the-fly.
func TestGzipOnTheFlyIndexGeneration(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "onthefly.tar.gz")
	indexPath := filepath.Join(tmpDir, "onthefly.sqlite")

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := NewWriter(gw)

	// Create 2 files, each 1MB, to exceed the default 1MB index spacing
	file1Data := bytes.Repeat([]byte("X"), 1024*1024)
	if err := tw.WriteHeader(&Header{Name: "file1.txt", Size: int64(len(file1Data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(file1Data)

	file2Data := bytes.Repeat([]byte("Y"), 1024*1024)
	if err := tw.WriteHeader(&Header{Name: "file2.txt", Size: int64(len(file2Data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(file2Data)

	tw.Close()
	gw.Close()
	f.Close()

	// Run on-the-fly index generator
	err = IndexArchive(tarPath, indexPath)
	if err != nil {
		t.Fatalf("IndexArchive failed: %v", err)
	}

	idx, err := OpenIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	indexBytes, err := idx.GetGzipIndex()
	if err != nil {
		t.Fatalf("Failed to fetch gzip index from database: %v", err)
	}
	if len(indexBytes) == 0 {
		t.Fatal("GZIP index was not saved in SQLite database")
	}

	// Decompress and parse the GZIDX index header
	gr, err := gzip.NewReader(bytes.NewReader(indexBytes))
	if err != nil {
		t.Fatalf("Failed to decompress GZIDX blob: %v", err)
	}
	defer gr.Close()

	dflidx, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("Failed to read GZIDX content: %v", err)
	}

	if len(dflidx) < 35 || string(dflidx[:5]) != "GZIDX" {
		t.Fatalf("Invalid GZIDX signature or length: %d", len(dflidx))
	}

	// Check correct offset for numPoints [31:35]
	numPoints := binary.LittleEndian.Uint32(dflidx[31:35])
	if numPoints < 2 {
		t.Errorf("Expected at least 2 checkpoints (0 offset + 1MB), got %d", numPoints)
	}

	// Verify TarFS actually opens and reads file2.txt correctly using the generated index
	tfs, err := NewFS(tarPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	data, err := fs.ReadFile(tfs, "file2.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(data, file2Data) {
		t.Error("Read data mismatch for file2.txt")
	}
}

// TestGzipResumeWithNoDataPoint verifies that seeking to a checkpoint
// with hasData = 0 (like offset 0 with GZIP header) is processed correctly.
func TestGzipResumeWithNoDataPoint(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte("hello seekable world"))
	gw.Close()

	gzipData := buf.Bytes()

	// Construct GZIDX index bytes containing only Point 0
	gzidxBuf := new(bytes.Buffer)
	gzidxBuf.Write([]byte("GZIDX"))
	gzidxBuf.WriteByte(1) // version
	gzidxBuf.WriteByte(0) // flags
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(len(gzipData)))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(20))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(1024*1024))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(32768))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(1)) // 1 point

	// Point 0 (offset 0, hasData = 0)
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(0))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(0))
	gzidxBuf.WriteByte(0) // bits
	gzidxBuf.WriteByte(0) // hasData = 0

	var cmpBuf bytes.Buffer
	gwIndex := gzip.NewWriter(&cmpBuf)
	gwIndex.Write(gzidxBuf.Bytes())
	gwIndex.Close()

	var di any = gzipFormat{}
	importer := di.(GzipIndexImporter)

	r, uncompOffset, err := importer.ResumeFromGzipIndex(bytes.NewReader(gzipData), cmpBuf.Bytes(), 0)
	if err != nil {
		t.Fatalf("ResumeFromGzipIndex failed on point with hasData = 0: %v", err)
	}
	defer r.Close()

	if uncompOffset != 0 {
		t.Errorf("Expected uncompOffset 0, got %d", uncompOffset)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("Failed to read GZIP data: %v", err)
	}
	if string(data) != "hello seekable world" {
		t.Errorf("Expected 'hello seekable world', got %q", string(data))
	}
}

// TestGzipMultistreamResumption verifies that resuming decompression from a checkpoint
// with hasData = 1 and a non-zero BitOffset inside a concatenated (multistream) GZIP
// file successfully reads the remaining stream and transitions to the subsequent streams.
func TestGzipMultistreamResumption(t *testing.T) {
	var raw1 bytes.Buffer
	for i := 0; i < 35000; i++ {
		raw1.WriteString("the quick brown fox jumps over the lazy dog ")
	}
	data1 := raw1.Bytes() // ~1.54 MB to guarantee multiple internal deflate blocks
	data2 := []byte("stream 2 secondary payload content that follows stream 1")

	// 1. Compress both streams as separate GZIP members (concatenated)
	var gzipBuf bytes.Buffer
	gw1 := gzip.NewWriter(&gzipBuf)
	if _, err := gw1.Write(data1); err != nil {
		t.Fatal(err)
	}
	gw1.Close()

	stream1CompressedLen := int64(gzipBuf.Len())

	gw2 := gzip.NewWriter(&gzipBuf)
	if _, err := gw2.Write(data2); err != nil {
		t.Fatal(err)
	}
	gw2.Close()

	multistreamGzip := gzipBuf.Bytes()

	// 2. Perform sequential on-the-fly index tracking
	gtr, err := newGzipIndexTrackingReader(bytes.NewReader(multistreamGzip))
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := io.ReadAll(gtr)
	if err != nil {
		t.Fatal(err)
	}

	expectedTotal := append([]byte(nil), data1...)
	expectedTotal = append(expectedTotal, data2...)
	if !bytes.Equal(decoded, expectedTotal) {
		t.Fatal("Initial full decompression mismatch")
	}

	// 3. Find a target checkpoint inside the first stream with hasData = 1 and non-zero BitOffset
	var targetPoint *gzPoint
	for i := range gtr.points {
		p := &gtr.points[i]
		if p.hasData == 1 && p.bits > 0 && p.compOffset < uint64(stream1CompressedLen) {
			targetPoint = p
			break
		}
	}

	if targetPoint == nil {
		t.Fatal("Could not find a suitable checkpoint with hasData = 1 and non-zero BitOffset inside Stream 1")
	}
	t.Logf("Selected checkpoint: compOffset=%d, uncompOffset=%d, bits=%d",
		targetPoint.compOffset, targetPoint.uncompOffset, targetPoint.bits)

	// 4. Export index data and resume decompression from selected point
	idxBytes, err := gtr.ExportGzipIndex()
	if err != nil {
		t.Fatal(err)
	}

	var di any = gzipFormat{}
	importer := di.(GzipIndexImporter)

	resReader, uncompOffset, err := importer.ResumeFromGzipIndex(
		bytes.NewReader(multistreamGzip),
		idxBytes,
		int64(targetPoint.uncompOffset),
	)
	if err != nil {
		t.Fatalf("Failed to resume from GZIP index: %v", err)
	}
	defer resReader.Close()

	if uncompOffset != int64(targetPoint.uncompOffset) {
		t.Errorf("Resumed uncompOffset mismatch: expected %d, got %d", targetPoint.uncompOffset, uncompOffset)
	}

	resDecoded, err := io.ReadAll(resReader)
	if err != nil {
		t.Fatalf("Failed to read from resumed stream: %v", err)
	}

	// 5. Verify the correctness of returned stream suffix across stream boundaries
	expectedResumed := append([]byte(nil), data1[targetPoint.uncompOffset:]...)
	expectedResumed = append(expectedResumed, data2...)

	if !bytes.Equal(resDecoded, expectedResumed) {
		t.Fatalf("Resumed stream data mismatch: expected length %d, got %d", len(expectedResumed), len(resDecoded))
	}
}
// TestGzipHeaderMetadataParsing verifies that the manual GZIP header parser
// in gzipIndexTrackingReader successfully skips FEXTRA, FNAME, and FCOMMENT fields
// and correctly extracts the underlying deflate stream.
func TestGzipHeaderMetadataParsing(t *testing.T) {
	var gzipBuf bytes.Buffer
	gw := gzip.NewWriter(&gzipBuf)
	gw.Name = "backup_archive_2026.tar"
	gw.Comment = "this is an industrial strength backup payload"
	gw.Extra = []byte{0x41, 0x42, 0x02, 0x00, 0xde, 0xad} // ID: AB, Len: 2, Data: 0xdead

	payload := []byte("confidential target file content that sits right behind multiple gzip header fields")
	if _, err := gw.Write(payload); err != nil {
		t.Fatal(err)
	}
	gw.Close()

	gtr, err := newGzipIndexTrackingReader(bytes.NewReader(gzipBuf.Bytes()))
	if err != nil {
		t.Fatalf("Failed to initialize tracking reader with metadata headers: %v", err)
	}
	defer gtr.Close()

	decoded, err := io.ReadAll(gtr)
	if err != nil {
		t.Fatalf("Failed to decompress stream containing metadata headers: %v", err)
	}

	if !bytes.Equal(decoded, payload) {
		t.Errorf("Payload mismatch!\nExpected: %q\nGot:      %q", string(payload), string(decoded))
	}
}
// TestGzipResumeInvalidIndex verifies that ResumeFromGzipIndex returns clean
// errors when provided with malformed, corrupted, or unsupported GZIDX index data.
func TestGzipResumeInvalidIndex(t *testing.T) {
	var di any = gzipFormat{}
	importer := di.(GzipIndexImporter)
	dummyPayload := make([]byte, 100)

	// Case 1: Index data too short (less than 35 bytes)
	shortIndex := []byte("GZIDX")
	var cmpBuf1 bytes.Buffer
	gw1 := gzip.NewWriter(&cmpBuf1)
	gw1.Write(shortIndex)
	gw1.Close()

	_, _, err := importer.ResumeFromGzipIndex(bytes.NewReader(dummyPayload), cmpBuf1.Bytes(), 0)
	if err == nil || err.Error() != "tar: invalid GZIDX header" {
		t.Errorf("Expected 'tar: invalid GZIDX header' error for short index, got: %v", err)
	}

	// Case 2: Missing "GZIDX" signature magic
	badMagic := make([]byte, 40)
	copy(badMagic, "BADMX")
	var cmpBuf2 bytes.Buffer
	gw2 := gzip.NewWriter(&cmpBuf2)
	gw2.Write(badMagic)
	gw2.Close()

	_, _, err = importer.ResumeFromGzipIndex(bytes.NewReader(dummyPayload), cmpBuf2.Bytes(), 0)
	if err == nil || err.Error() != "tar: invalid GZIDX header" {
		t.Errorf("Expected 'tar: invalid GZIDX header' error for bad magic, got: %v", err)
	}

	// Case 3: Unsupported version flag (e.g. version 2)
	unsupportedVer := make([]byte, 40)
	copy(unsupportedVer[:5], "GZIDX")
	unsupportedVer[5] = 2 // Version 2
	var cmpBuf3 bytes.Buffer
	gw3 := gzip.NewWriter(&cmpBuf3)
	gw3.Write(unsupportedVer)
	gw3.Close()

	_, _, err = importer.ResumeFromGzipIndex(bytes.NewReader(dummyPayload), cmpBuf3.Bytes(), 0)
	if err == nil || err.Error() != "tar: unsupported GZIDX version: 2" {
		t.Errorf("Expected 'tar: unsupported GZIDX version: 2' error, got: %v", err)
	}
}
// TestXzRandomAccess verifies native XZ index parsing and O(1) block-level seeking.
func TestXzRandomAccess(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "test_xz.tar.xz")
	indexPath := filepath.Join(tmpDir, "test_xz.sqlite")

	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0755)
	file1Data := bytes.Repeat([]byte("A"), 1024*1024) // 1MB
	file2Data := bytes.Repeat([]byte("B"), 1024*1024) // 1MB

	os.WriteFile(filepath.Join(srcDir, "file1.txt"), file1Data, 0644)
	os.WriteFile(filepath.Join(srcDir, "sub", "file2.txt"), file2Data, 0644)

	// Explicitly compress with XZ method
	a, err := NewArchiver(tarPath, filepath.Dir(srcDir), WithArchiverMethod(XZ))
	if err != nil {
		t.Fatal(err)
	}

	files := make(map[string]os.FileInfo)
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil { return err }
		files[path] = info
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Read and verify XZ Blocks natively
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat()
	blocks, err := parseXZIndex(f, fi.Size())
	f.Close()
	if err != nil {
		t.Fatalf("Failed to parse native XZ index: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("Parsed XZ block offset list is empty")
	}

	// Mount through TarFS
	tfs, err := NewFS(tarPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// Verify we can seek and read sub/file2.txt correctly
	data, err := fs.ReadFile(tfs, "src/sub/file2.txt")
	if err != nil {
		t.Fatalf("Failed to read file2.txt: %v", err)
	}
	if !bytes.Equal(data, file2Data) {
		t.Error("Decompressed XZ random access data mismatch")
	}
}
