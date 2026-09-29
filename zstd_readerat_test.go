package tar

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdFrames(t *testing.T, chunks [][]byte, streaming bool) []byte {
	t.Helper()
	var out bytes.Buffer
	if streaming {
		for _, c := range chunks {
			var frame bytes.Buffer
			w, err := zstd.NewWriter(&frame, zstd.WithEncoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(c); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			out.Write(frame.Bytes())
		}
		return out.Bytes()
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		out.Write(enc.EncodeAll(c, nil))
	}
	return out.Bytes()
}

func chunked(data []byte, size int) [][]byte {
	var chunks [][]byte
	for len(data) > 0 {
		n := size
		if n > len(data) {
			n = len(data)
		}
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	return chunks
}

func skippableFrame(payload string) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.LittleEndian.PutUint32(b, 0x184D2A5B)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
	return append(b, payload...)
}

// A multi-frame zstd file is entered at the frame a read falls in: a warm read
// decodes at most the frames it touches, whatever the file's length.
func TestZstdReaderAtMultiFrameRandomReads(t *testing.T) {
	data := compressible(30 << 20)
	const chunk = 1 << 20
	for _, streaming := range []bool{false, true} {
		packed := zstdFrames(t, chunked(data, chunk), streaming)
		z, err := NewZstdReaderAt(bytes.NewReader(packed), int64(len(packed)))
		if err != nil {
			t.Fatal(err)
		}

		check := func(off, n int) {
			t.Helper()
			p := make([]byte, n)
			got, err := z.ReadAt(p, int64(off))
			if got != n || err != nil || !bytes.Equal(p, data[off:off+n]) {
				t.Fatalf("streaming=%v ReadAt(%d, %d) = %d, %v", streaming, off, n, got, err)
			}
		}
		check(29<<20+5, 1000) // cold, far
		rng := rand.New(rand.NewSource(3))
		for i := 0; i < 60; i++ {
			off, n := rng.Intn(29<<20), 1+rng.Intn(200<<10)
			before := z.Decoded()
			check(off, n)
			if cost := z.Decoded() - before; cost > int64(3*chunk+n) {
				t.Fatalf("streaming=%v: read at %d decoded %d bytes, more than the frames it touches", streaming, off, cost)
			}
		}
		check(chunk-100, 300) // across a frame seam
		check(0, 10)
		_ = z.Close()
	}
}

func TestZstdReaderAtEndSizeAndSkippableFrames(t *testing.T) {
	a, b := compressible(300<<10), compressible(200<<10)
	b[0] ^= 1
	packed := append(zstdFrames(t, [][]byte{a}, false), skippableFrame("padding")...)
	packed = append(packed, zstdFrames(t, [][]byte{b}, true)...)
	want := append(append([]byte{}, a...), b...)

	z, err := NewZstdReaderAt(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	if z.Size() != -1 {
		t.Fatalf("Size before the end = %d", z.Size())
	}
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
	// Back across the seam between the two data frames, over the skippable one.
	q := make([]byte, 600)
	if n, err := z.ReadAt(q, int64(len(a))-300); n != 600 || err != nil || !bytes.Equal(q, want[len(a)-300:len(a)+300]) {
		t.Fatalf("read over the skippable frame = %d, %v", n, err)
	}
	if n, err := z.ReadAt(nil, 3); n != 0 || err != nil {
		t.Fatalf("empty read = %d, %v", n, err)
	}
	if _, err := z.ReadAt(p, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
}

// One frame is entered only at its start: reads still come out right, forward
// reads continue the decoder, and the frame is not decoded again for each.
func TestZstdReaderAtSingleFrame(t *testing.T) {
	data := compressible(12 << 20)
	packed := zstdFrames(t, [][]byte{data}, true)
	z, err := NewZstdReaderAt(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Close() }()
	for off := 0; off < len(data)-4096; off += 1 << 20 { // forward chunks
		p := make([]byte, 4096)
		if n, err := z.ReadAt(p, int64(off)); n != len(p) || err != nil || !bytes.Equal(p, data[off:off+4096]) {
			t.Fatalf("forward read at %d = %d, %v", off, n, err)
		}
	}
	if d := z.Decoded(); d > int64(2*len(data)) {
		t.Fatalf("forward reads decoded %d bytes for a %d byte frame", d, len(data))
	}
	p := make([]byte, 100)
	if n, err := z.ReadAt(p, 1234); n != 100 || err != nil || !bytes.Equal(p, data[1234:1334]) {
		t.Fatalf("read going back = %d, %v", n, err)
	}
}

func TestZstdReaderAtRejectsBadInput(t *testing.T) {
	if _, err := NewZstdReaderAt(bytes.NewReader([]byte("plain text, no magic")), 20); err == nil {
		t.Fatal("non-zstd data accepted")
	}
	if _, err := NewZstdReaderAt(bytes.NewReader(nil), -1); err == nil {
		t.Fatal("unknown size accepted")
	}
	if _, err := NewZstdReaderAt(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("empty input accepted")
	}

	// A file cut inside its second frame reports an error rather than data.
	packed := zstdFrames(t, chunked(compressible(600<<10), 200<<10), false)
	cut := packed[:len(packed)-100]
	z, err := NewZstdReaderAt(bytes.NewReader(cut), int64(len(cut)))
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 100)
	if _, err := z.ReadAt(p, 550<<10); err == nil {
		t.Fatal("a read into a truncated frame succeeded")
	}
	// A corrupt frame magic is refused as such.
	bad := append([]byte{}, packed...)
	frames := zstdFrames(t, [][]byte{compressible(1000)}, false)
	bad = append(frames, 0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0)
	z2, err := NewZstdReaderAt(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z2.ReadAt(p, 2000); err == nil {
		t.Fatal("a garbage frame was accepted")
	}
}
