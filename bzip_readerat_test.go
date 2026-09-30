package tar

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	"github.com/dsnet/compress/bzip2"
)

func bzipOf(t *testing.T, level int, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := bzip2.NewWriter(&buf, &bzip2.WriterConfig{Level: level})
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

// A many-block bzip2 is entered at the block a read falls in: once the blocks
// before it have been sized, a read decodes only the block or two it touches.
func TestBzipReaderAtRandomReads(t *testing.T) {
	data := compressible(6 << 20)
	packed := bzipOf(t, 1, data) // 100 kB blocks
	z, err := NewBzipReaderAt(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Close() }()
	if len(z.blocks) < 20 {
		t.Fatalf("found %d blocks, want many", len(z.blocks))
	}
	if z.Size() != -1 {
		t.Fatalf("Size before sizing = %d", z.Size())
	}

	check := func(off, n int) {
		t.Helper()
		p := make([]byte, n)
		got, err := z.ReadAt(p, int64(off))
		if got != n || err != nil || !bytes.Equal(p, data[off:off+n]) {
			t.Fatalf("ReadAt(%d, %d) = %d, %v", off, n, got, err)
		}
	}
	check(5<<20+7, 1000) // far, cold: sizes the blocks before it
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 60; i++ {
		off, n := rng.Intn(5<<20), 1+rng.Intn(150<<10)
		before := z.Decoded()
		check(off, n)
		if cost := z.Decoded() - before; cost > int64(5*(110<<10)+n) {
			t.Fatalf("read at %d decoded %d bytes, more than the blocks it touches", off, cost)
		}
	}
	check(0, 10)
}

func TestBzipReaderAtEndSizeAndConcatenatedStreams(t *testing.T) {
	a, b := compressible(350<<10), compressible(220<<10)
	b[0] ^= 1
	packed := append(bzipOf(t, 1, a), bzipOf(t, 2, b)...) // two streams, different levels
	want := append(append([]byte{}, a...), b...)

	z, err := NewBzipReaderAt(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Close() }()
	p := make([]byte, 1000)
	n, err := z.ReadAt(p, int64(len(want))-400)
	if n != 400 || err != io.EOF || !bytes.Equal(p[:n], want[len(want)-400:]) {
		t.Fatalf("read across the end = %d, %v", n, err)
	}
	if z.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", z.Size(), len(want))
	}
	if n, err := z.ReadAt(p, int64(len(want))); n != 0 || err != io.EOF {
		t.Fatalf("read at the end = %d, %v", n, err)
	}
	// Back over the seam between the streams.
	q := make([]byte, 600)
	if n, err := z.ReadAt(q, int64(len(a))-300); n != 600 || err != nil || !bytes.Equal(q, want[len(a)-300:len(a)+300]) {
		t.Fatalf("read over the stream seam = %d, %v", n, err)
	}
	if n, err := z.ReadAt(nil, 3); n != 0 || err != nil {
		t.Fatalf("empty read = %d, %v", n, err)
	}
	if _, err := z.ReadAt(p, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
}

func TestBzipReaderAtEmptyAndBadInput(t *testing.T) {
	empty := bzipOf(t, 1, nil)
	z, err := NewBzipReaderAt(bytes.NewReader(empty), int64(len(empty)))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := z.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Errorf("read of an empty stream = %d, %v", n, err)
	}
	if z.Size() != 0 {
		t.Errorf("empty stream size = %d", z.Size())
	}

	if _, err := NewBzipReaderAt(bytes.NewReader([]byte("plain text that is not bzip2 at all")), 35); err == nil {
		t.Error("non-bzip2 data accepted")
	}
	packed := bzipOf(t, 1, compressible(300<<10))
	if _, err := NewBzipReaderAt(bytes.NewReader(packed[:len(packed)-20]), int64(len(packed)-20)); err == nil {
		t.Error("a stream cut before its end was accepted")
	}

	// Damage inside a block is reported when the block is read, not hidden.
	bad := append([]byte{}, packed...)
	bad[len(bad)/2] ^= 0xff
	if zb, err := NewBzipReaderAt(bytes.NewReader(bad), int64(len(bad))); err == nil {
		if _, err := zb.ReadAt(make([]byte, 10), 250<<10); err == nil {
			t.Error("a damaged block was read without an error")
		}
	}
}
