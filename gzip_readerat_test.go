package tar

import (
	"bytes"
	"compress/gzip"
	"io"
	"math/rand"
	"testing"
)

// compressible returns n bytes of text-like data that deflate compresses
// several times over but not to nothing: words drawn from a small vocabulary
// with numbers between them, from a fixed seed.
func compressible(n int) []byte {
	rng := rand.New(rand.NewSource(42))
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	var b bytes.Buffer
	b.Grow(n + 64)
	for b.Len() < n {
		b.WriteString(words[rng.Intn(len(words))])
		b.WriteByte(' ')
		b.WriteString(string(rune('0' + rng.Intn(10))))
		b.WriteString(string(rune('a' + rng.Intn(26))))
		b.WriteByte('\n')
	}
	return b.Bytes()[:n]
}

func gzipOf(t *testing.T, level int, parts ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, part := range parts {
		zw, err := gzip.NewWriterLevel(&buf, level)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(part); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// The f4#1678 case: a 50 MB GZIP served as a ReaderAt with no index and no
// path. Reads at arbitrary places return the right bytes, and once the scan
// has passed a place, reading it again does not decompress from the start.
func TestGzipReaderAtRandomReads50MB(t *testing.T) {
	data := compressible(50 << 20)
	packed := gzipOf(t, gzip.BestSpeed, data)
	const spacing = 1 << 20
	g, err := NewGzipReaderAt(bytes.NewReader(packed), int64(len(packed)), spacing)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	check := func(off, n int) {
		t.Helper()
		p := make([]byte, n)
		got, err := g.ReadAt(p, int64(off))
		if got != n || err != nil {
			t.Fatalf("ReadAt(%d, %d) = %d, %v", off, n, got, err)
		}
		if !bytes.Equal(p, data[off:off+n]) {
			t.Fatalf("ReadAt(%d, %d): wrong bytes", off, n)
		}
	}

	// Cold: one far read is one scan up to it.
	check(45<<20, 4096)
	if d := g.Decoded(); d < 45<<20 || d > 46<<20 {
		t.Fatalf("decoded %d bytes for the cold far read, want about %d", d, 45<<20)
	}

	// Warm: reads anywhere behind the scan cost at most a checkpoint interval.
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 60; i++ {
		off := rng.Intn(45 << 20)
		n := 1 + rng.Intn(100<<10)
		before := g.Decoded()
		check(off, n)
		if cost := g.Decoded() - before; cost > int64(2*spacing+n) {
			t.Fatalf("read at %d decoded %d bytes, more than 2 checkpoint intervals and the read", off, cost)
		}
	}

	// Reads straddling the scan frontier, and the very start.
	check(0, 10)
	check(45<<20-100, 5000)
}

func TestGzipReaderAtEndAndSize(t *testing.T) {
	data := compressible(300 << 10)
	packed := gzipOf(t, gzip.DefaultCompression, data)
	g, err := NewGzipReaderAt(bytes.NewReader(packed), int64(len(packed)), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if g.Size() != -1 {
		t.Fatalf("Size before the end = %d, want -1", g.Size())
	}

	p := make([]byte, 1000)
	n, err := g.ReadAt(p, int64(len(data))-400)
	if n != 400 || err != io.EOF || !bytes.Equal(p[:n], data[len(data)-400:]) {
		t.Fatalf("read across the end = %d, %v", n, err)
	}
	if g.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", g.Size(), len(data))
	}
	if n, err := g.ReadAt(p, int64(len(data))); n != 0 || err != io.EOF {
		t.Fatalf("read at the end = %d, %v", n, err)
	}
	// After the end is known, a read behind it still works from a checkpoint.
	n, err = g.ReadAt(p, 100_000)
	if n != len(p) || err != nil || !bytes.Equal(p, data[100_000:101_000]) {
		t.Fatalf("read behind the end = %d, %v", n, err)
	}
	if n, err := g.ReadAt(nil, 5); n != 0 || err != nil {
		t.Fatalf("empty read = %d, %v", n, err)
	}
	if _, err := g.ReadAt(p, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ReadAt(p, int64(len(data))+10); err != io.EOF {
		t.Fatalf("read after close past the end = %v, want EOF", err)
	}
}

func TestGzipReaderAtConcatenatedMembersAndStoredBlocks(t *testing.T) {
	a, b := compressible(200<<10), compressible(150<<10)
	b[0] ^= 0xff
	for _, level := range []int{gzip.NoCompression, gzip.BestCompression} {
		packed := gzipOf(t, level, a, b)
		want := append(append([]byte{}, a...), b...)
		g, err := NewGzipReaderAt(bytes.NewReader(packed), int64(len(packed)), 32<<10)
		if err != nil {
			t.Fatal(err)
		}
		p := make([]byte, len(want))
		if n, err := g.ReadAt(p, 0); n != len(want) || err != nil || !bytes.Equal(p, want) {
			t.Fatalf("level %d: whole read = %d, %v", level, n, err)
		}
		// Back into the first member, across the seam, and into the second.
		for _, off := range []int{10, 200<<10 - 50, 200<<10 + 5, 340 << 10} {
			q := make([]byte, 100)
			if n, err := g.ReadAt(q, int64(off)); n != 100 || err != nil || !bytes.Equal(q, want[off:off+100]) {
				t.Fatalf("level %d: read at %d = %d, %v", level, off, n, err)
			}
		}
		_ = g.Close()
	}
}

func TestGzipReaderAtRejectsBadInput(t *testing.T) {
	if _, err := NewGzipReaderAt(bytes.NewReader([]byte("not gzip at all")), 15, 0); err == nil {
		t.Fatal("non-gzip data accepted")
	}
	if _, err := NewGzipReaderAt(bytes.NewReader(nil), -1, 0); err == nil {
		t.Fatal("unknown size accepted")
	}

	// A file cut in the middle of the deflate data reports an error, and the
	// failure sticks instead of being retried.
	data := compressible(400 << 10)
	packed := gzipOf(t, gzip.DefaultCompression, data)
	cut := packed[:len(packed)/2]
	g, err := NewGzipReaderAt(bytes.NewReader(cut), int64(len(cut)), 0)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 10)
	_, first := g.ReadAt(p, int64(len(data))-10)
	if first == nil {
		t.Fatal("a truncated file read past its data without an error")
	}
	if _, second := g.ReadAt(p, int64(len(data))-10); second == nil {
		t.Fatal("a failed scan was retried into success")
	}
}
