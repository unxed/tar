//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos
package tar

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/gzip"
)

// trackingReaderAt wraps io.ReaderAt and logs all ReadAt calls.
type trackingReaderAt struct {
	r     io.ReaderAt
	reads []readRecord
}

type readRecord struct {
	offset int64
	size   int
}

func (t *trackingReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	n, err = t.r.ReadAt(p, off)
	t.reads = append(t.reads, readRecord{offset: off, size: n})
	return n, err
}

func TestStoreRandomAccessNoFullRead(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "uncompressed.tar")
	indexPath := filepath.Join(tmpDir, "uncompressed.sqlite")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	file1Data := bytes.Repeat([]byte("A"), 10000)
	if err := tw.WriteHeader(&Header{Name: "file1.txt", Size: 10000, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(file1Data)

	file2Data := []byte("secret payload")
	if err := tw.WriteHeader(&Header{Name: "file2.txt", Size: int64(len(file2Data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(file2Data)
	tw.Close()
	f.Close()

	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	node, err := tfs.Index.Lookup("file2.txt")
	if err != nil {
		t.Fatal(err)
	}

	if node.Offset < 10000 {
		t.Fatalf("Expected file2.txt offset to be > 10000, got %d", node.Offset)
	}

	rawFile, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawFile.Close()

	tracker := &trackingReaderAt{r: rawFile}
	sr := io.NewSectionReader(tracker, node.Offset, node.Size)

	buf := make([]byte, node.Size)
	n, err := io.ReadFull(sr, buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(file2Data) || !bytes.Equal(buf, file2Data) {
		t.Fatalf("Data mismatch, got %q", string(buf))
	}

	for _, rd := range tracker.reads {
		if rd.offset < node.Offset {
			t.Errorf("Violation: read occurred at offset %d, which is before the file offset %d", rd.offset, node.Offset)
		}
	}
}

func TestZstdRandomAccessNoFullRead(t *testing.T) {
	var di any = zstdFormat{}
	importer, ok := di.(BlockOffsetImporter)
	if !ok {
		t.Fatal("zstdFormat does not implement BlockOffsetImporter")
	}

	dummyData := bytes.Repeat([]byte("ZSTD FRAME DUMMY DATA"), 1000)
	tracker := &trackingReaderAt{r: bytes.NewReader(dummyData)}

	bo := &BlockOffset{
		BlockOffset: 500,
		DataOffset:  1000,
	}

	_, _ = importer.ResumeFromBlockOffset(tracker, bo)

	for _, rd := range tracker.reads {
		if rd.offset < bo.BlockOffset {
			t.Errorf("ZSTD Violation: read occurred at offset %d, which is before block offset %d", rd.offset, bo.BlockOffset)
		}
	}
}

func TestGzipRandomAccessNoFullRead(t *testing.T) {
	var di any = gzipFormat{}
	importer, ok := di.(GzipIndexImporter)
	if !ok {
		t.Fatal("gzipFormat does not implement GzipIndexImporter")
	}

	gzidxBuf := new(bytes.Buffer)
	gzidxBuf.Write([]byte("GZIDX"))
	gzidxBuf.WriteByte(1)
	gzidxBuf.WriteByte(0)
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(2000))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(4000))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(1024*1024))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(32768))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(2))

	binary.Write(gzidxBuf, binary.LittleEndian, uint64(0))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(0))
	gzidxBuf.WriteByte(0)
	gzidxBuf.WriteByte(0)

	binary.Write(gzidxBuf, binary.LittleEndian, uint64(800))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(1500))
	gzidxBuf.WriteByte(0)
	gzidxBuf.WriteByte(0)

	var cmpBuf bytes.Buffer
	gw := gzip.NewWriter(&cmpBuf)
	gw.Write(gzidxBuf.Bytes())
	gw.Close()

	dummyData := make([]byte, 2000)
	tracker := &trackingReaderAt{r: bytes.NewReader(dummyData)}

	_, _, _ = importer.ResumeFromGzipIndex(tracker, cmpBuf.Bytes(), 1600)

	foundPoint1Read := false
	for _, rd := range tracker.reads {
		if rd.offset < 800 {
			t.Errorf("GZIP Violation: read occurred at offset %d, which is before point offset 800", rd.offset)
		} else {
			foundPoint1Read = true
		}
	}

	if !foundPoint1Read && len(tracker.reads) > 0 {
		t.Errorf("Expected some reads to occur at or after offset 800")
	}
}

func TestXzSectionReaderRandomAccess(t *testing.T) {
	header := make([]byte, 12)
	dummyFile := bytes.Repeat([]byte("XZ DUMMY BYTES"), 1000)
	tracker := &trackingReaderAt{r: bytes.NewReader(dummyFile)}

	blockOffset := int64(400)
	sr := io.NewSectionReader(tracker, blockOffset, 1<<63-1)
	mr := io.MultiReader(bytes.NewReader(header), sr)

	buf := make([]byte, 50)
	_, _ = io.ReadFull(mr, buf)

	for _, rd := range tracker.reads {
		if rd.offset < blockOffset {
			t.Errorf("XZ Violation: read occurred at offset %d, which is before block offset %d", rd.offset, blockOffset)
		}
	}
}