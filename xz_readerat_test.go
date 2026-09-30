package tar

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	"github.com/unxed/xz"
)

func xzOf(t *testing.T, blockSize int64, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := xz.WriterConfig{BlockSize: blockSize}
	w, err := cfg.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A multi-block .xz is entered at the block a read falls in: the size is known
// from the index at once, and a read decodes only what it touches.
func TestXzReaderAtMultiBlockRandomReads(t *testing.T) {
	data := compressible(12 << 20)
	const block = 1 << 20
	packed := xzOf(t, block, data)
	x, err := NewXzReaderAt(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = x.Close() }()
	if x.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", x.Size(), len(data))
	}

	check := func(off, n int) {
		t.Helper()
		p := make([]byte, n)
		got, err := x.ReadAt(p, int64(off))
		if got != n || err != nil || !bytes.Equal(p, data[off:off+n]) {
			t.Fatalf("ReadAt(%d, %d) = %d, %v", off, n, got, err)
		}
	}
	check(11<<20+5, 1000) // far, first thing: no scan
	if d := x.Decoded(); d > 2*block {
		t.Fatalf("a far first read decoded %d bytes, more than its block", d)
	}
	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 50; i++ {
		off, n := rng.Intn(11<<20), 1+rng.Intn(200<<10)
		before := x.Decoded()
		check(off, n)
		if cost := x.Decoded() - before; cost > int64(3*block+n) {
			t.Fatalf("read at %d decoded %d bytes, more than the blocks it touches", off, cost)
		}
	}
	check(block-100, 300) // across a block seam
	check(0, 10)
}

func TestXzReaderAtEndAndSingleBlock(t *testing.T) {
	data := compressible(3 << 20)
	packed := xzOf(t, 0, data) // one block
	x, err := NewXzReaderAt(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = x.Close() }()

	for off := 0; off < len(data)-4096; off += 512 << 10 { // forward chunks continue the decoder
		p := make([]byte, 4096)
		if n, err := x.ReadAt(p, int64(off)); n != len(p) || err != nil || !bytes.Equal(p, data[off:off+4096]) {
			t.Fatalf("forward read at %d = %d, %v", off, n, err)
		}
	}
	if d := x.Decoded(); d > int64(2*len(data)) {
		t.Fatalf("forward reads decoded %d bytes for a %d byte block", d, len(data))
	}
	p := make([]byte, 1000)
	n, err := x.ReadAt(p, int64(len(data))-400)
	if n != 400 || err != io.EOF || !bytes.Equal(p[:n], data[len(data)-400:]) {
		t.Fatalf("read across the end = %d, %v", n, err)
	}
	if n, err := x.ReadAt(p, int64(len(data))); n != 0 || err != io.EOF {
		t.Fatalf("read at the end = %d, %v", n, err)
	}
	if n, err := x.ReadAt(p, 777); n != len(p) || err != nil || !bytes.Equal(p, data[777:1777]) {
		t.Fatalf("read going back = %d, %v", n, err)
	}
	if n, err := x.ReadAt(nil, 3); n != 0 || err != nil {
		t.Fatalf("empty read = %d, %v", n, err)
	}
	if _, err := x.ReadAt(p, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
}

func TestXzReaderAtEmptyAndBadInput(t *testing.T) {
	empty := xzOf(t, 0, nil)
	x, err := NewXzReaderAt(bytes.NewReader(empty), int64(len(empty)))
	if err != nil {
		t.Fatal(err)
	}
	if x.Size() != 0 {
		t.Errorf("empty file size = %d", x.Size())
	}
	if n, err := x.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Errorf("read of an empty file = %d, %v", n, err)
	}

	if _, err := NewXzReaderAt(bytes.NewReader([]byte("definitely not an xz file, just text")), 36); err == nil {
		t.Error("non-xz data accepted")
	}
	if _, err := NewXzReaderAt(bytes.NewReader(nil), -1); err == nil {
		t.Error("unknown size accepted")
	}
}
