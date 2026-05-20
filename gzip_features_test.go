package tar

import (
	"bytes"
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