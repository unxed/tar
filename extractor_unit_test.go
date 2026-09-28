package tar

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestNextPowerOf2 verifies the buffer-sizing helper used by the sparse-file
// copy path: any size at or below 4096 rounds up to 4096, and everything
// else rounds up to the next power of two (or itself, if already one).
func TestNextPowerOf2(t *testing.T) {
	cases := []struct {
		in   int64
		want int64
	}{
		{0, 4096},
		{1, 4096},
		{4095, 4096},
		{4096, 4096},
		{4097, 8192},
		{5000, 8192},
		{8192, 8192},
		{8193, 16384},
		{1 << 20, 1 << 20},
		{(1 << 20) + 1, 1 << 21},
	}
	for _, tc := range cases {
		if got := nextPowerOf2(tc.in); got != tc.want {
			t.Errorf("nextPowerOf2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestIsAllZeros checks the zero-block detector used to decide whether a
// chunk of file data can be skipped over (sparse hole) rather than written.
func TestIsAllZeros(t *testing.T) {
	if !isAllZeros(nil) {
		t.Error("isAllZeros(nil) = false, want true")
	}
	if !isAllZeros([]byte{}) {
		t.Error("isAllZeros(empty) = false, want true")
	}
	if !isAllZeros(make([]byte, 4096)) {
		t.Error("isAllZeros(zeros) = false, want true")
	}
	nonZero := make([]byte, 4096)
	nonZero[4095] = 1
	if isAllZeros(nonZero) {
		t.Error("isAllZeros(mostly zero, last byte set) = true, want false")
	}
	nonZero2 := make([]byte, 100)
	nonZero2[0] = 0xFF
	if isAllZeros(nonZero2) {
		t.Error("isAllZeros(first byte set) = true, want false")
	}
}

// TestCopySparseBytes verifies that writing a byte slice through
// copySparseBytes produces a file whose readable contents exactly match the
// input, whether or not any given 256KB block happened to be all zeros.
func TestCopySparseBytes(t *testing.T) {
	blockSize := 256 * 1024

	data := make([]byte, blockSize+blockSize/2) // 1.5 blocks
	// First block: all zeros (should be seeked over, not written).
	// Second, partial block: non-zero content.
	for i := blockSize; i < len(data); i++ {
		data[i] = byte(i % 251)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sparse_bytes.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := copySparseBytes(f, data); err != nil {
		f.Close()
		t.Fatalf("copySparseBytes: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("file contents mismatch after copySparseBytes: got %d bytes, want %d bytes", len(got), len(data))
	}
}

// TestCopySparseBytesAllZero ensures a fully-zero payload round-trips to a
// correctly sized (all-zero) file purely through Seek+Truncate, with no
// Write call needed.
func TestCopySparseBytesAllZero(t *testing.T) {
	data := make([]byte, 300*1024) // > one 256KB block, all zero

	dir := t.TempDir()
	path := filepath.Join(dir, "sparse_allzero.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := copySparseBytes(f, data); err != nil {
		f.Close()
		t.Fatalf("copySparseBytes: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len(data)) {
		t.Fatalf("file size = %d, want %d", fi.Size(), len(data))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("expected all-zero content of length %d, contents differ", len(data))
	}
}

// TestCopySparse verifies the io.Reader-driven sparse copy path (used for
// large files streamed through the 1MB buffer pool), including a payload
// that spans more than one internal buffer's worth of data so the read loop
// runs more than once.
func TestCopySparse(t *testing.T) {
	// Build data spanning more than one 1MB internal buffer: 1MB of zeros
	// followed by ~64KB of non-zero content.
	zeroPart := make([]byte, 1024*1024)
	tailPart := make([]byte, 64*1024)
	for i := range tailPart {
		tailPart[i] = byte(i%200 + 1) // guaranteed non-zero
	}
	data := append(append([]byte{}, zeroPart...), tailPart...)

	dir := t.TempDir()
	path := filepath.Join(dir, "sparse_reader.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	src := bytes.NewReader(data)
	if err := copySparse(f, src, int64(len(data))); err != nil {
		f.Close()
		t.Fatalf("copySparse: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("file contents mismatch after copySparse: got %d bytes, want %d bytes", len(got), len(data))
	}
}

// TestStripComponents mirrors GNU tar's --strip-components semantics: strip
// N leading path components, dropping entries that don't have enough of
// them left over.
func TestStripComponents(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		count    int
		wantName string
		wantKeep bool
	}{
		{"strip one of three", "a/b/c", 1, "b/c", true},
		{"strip exactly all", "a/b/c", 3, "", false},
		{"strip more than exists", "a/b", 5, "", false},
		{"leading slash stripped first", "/a/b", 1, "b", true},
		{"zero strip keeps path", "a/b", 0, "a/b", true},
		{"dot resolves to empty", ".", 0, "", false},
		{"empty name", "", 0, "", false},
		{"dot segments cleaned", "./a/./b", 1, "b", true},
		{"single component, zero strip", "a", 0, "a", true},
		{"single component, strip one", "a", 1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotKeep := stripComponents(tc.path, tc.count)
			if gotName != tc.wantName || gotKeep != tc.wantKeep {
				t.Errorf("stripComponents(%q, %d) = (%q, %v), want (%q, %v)",
					tc.path, tc.count, gotName, gotKeep, tc.wantName, tc.wantKeep)
			}
		})
	}
}

// TestGetSmallBuffer_Small verifies that small allocations are rounded up
// to a power-of-two-capacity slice of exactly the requested length, so that
// putSmallBuffer can later recycle them into memPools.
func TestGetSmallBuffer_Small(t *testing.T) {
	sizes := []int64{1, 2, 3, 100, 4096, 4097, 1 << 20}
	for _, size := range sizes {
		b := getSmallBuffer(size)
		if int64(len(b)) != size {
			t.Errorf("getSmallBuffer(%d) len = %d, want %d", size, len(b), size)
		}
		c := cap(b)
		if c&(c-1) != 0 {
			t.Errorf("getSmallBuffer(%d) cap = %d, want a power of two", size, c)
		}
		if int64(c) < size {
			t.Errorf("getSmallBuffer(%d) cap = %d, smaller than requested size", size, c)
		}
		putSmallBuffer(b)
	}
}

// TestGetSmallBuffer_ZeroOrNegative checks the guard clause that avoids
// touching the pool machinery for empty/invalid sizes.
func TestGetSmallBuffer_ZeroOrNegative(t *testing.T) {
	if b := getSmallBuffer(0); b != nil {
		t.Errorf("getSmallBuffer(0) = %v, want nil", b)
	}
	if b := getSmallBuffer(-1); b != nil {
		t.Errorf("getSmallBuffer(-1) = %v, want nil", b)
	}
}

// TestGetSmallBuffer_Large checks the bypass path for allocations above the
// 16MB pooling threshold: it should allocate exactly-sized memory rather
// than going through memPools.
func TestGetSmallBuffer_Large(t *testing.T) {
	size := int64(16*1024*1024 + 1)
	b := getSmallBuffer(size)
	if int64(len(b)) != size {
		t.Fatalf("getSmallBuffer(large) len = %d, want %d", len(b), size)
	}
	if cap(b) != len(b) {
		t.Fatalf("getSmallBuffer(large) cap = %d, want exactly %d (no pooling)", cap(b), len(b))
	}
	// putSmallBuffer must be a safe no-op for oversized buffers.
	putSmallBuffer(b)
}

// TestPutSmallBuffer_NonPowerOfTwoCap ensures putSmallBuffer silently
// ignores slices whose capacity isn't a power of two instead of corrupting
// memPools (which assumes power-of-two bucket sizes).
func TestPutSmallBuffer_NonPowerOfTwoCap(t *testing.T) {
	b := make([]byte, 3) // cap 3 is not a power of two
	putSmallBuffer(b)    // must not panic

	var empty []byte
	putSmallBuffer(empty) // cap 0, must also be a safe no-op
}

// TestExtractorOptions_Setters exercises every With* option that isn't
// already covered indirectly by higher-level extraction tests, verifying
// each mutates exactly the field it documents.
func TestExtractorOptions_Setters(t *testing.T) {
	apply := func(t *testing.T, opt ExtractorOption) extractorOptions {
		t.Helper()
		var o extractorOptions
		if err := opt(&o); err != nil {
			t.Fatalf("option returned error: %v", err)
		}
		return o
	}

	t.Run("SafeWrites", func(t *testing.T) {
		if o := apply(t, WithExtractorSafeWrites(true)); !o.safeWrites {
			t.Error("safeWrites not set")
		}
		if o := apply(t, WithExtractorSafeWrites(false)); o.safeWrites {
			t.Error("safeWrites unexpectedly set")
		}
	})

	t.Run("UnlinkFirst", func(t *testing.T) {
		if o := apply(t, WithExtractorUnlinkFirst(true)); !o.unlinkFirst {
			t.Error("unlinkFirst not set")
		}
	})

	t.Run("NumericOwner", func(t *testing.T) {
		if o := apply(t, WithExtractorNumericOwner(true)); !o.numericOwner {
			t.Error("numericOwner not set")
		}
	})

	t.Run("KeepBroken", func(t *testing.T) {
		if o := apply(t, WithExtractorKeepBroken(true)); !o.keepBroken {
			t.Error("keepBroken not set")
		}
	})

	t.Run("Tolerant", func(t *testing.T) {
		if o := apply(t, WithExtractorTolerant(true)); !o.tolerant {
			t.Error("tolerant not set")
		}
	})

	t.Run("Sparse", func(t *testing.T) {
		if o := apply(t, WithExtractorSparse(true)); !o.sparse {
			t.Error("sparse not set")
		}
	})

	t.Run("Password", func(t *testing.T) {
		if o := apply(t, WithExtractorPassword("secret")); o.password != "secret" {
			t.Errorf("password = %q, want %q", o.password, "secret")
		}
	})

	t.Run("MaxRatio", func(t *testing.T) {
		if o := apply(t, WithExtractorMaxRatio(42)); o.maxDecompressionRatio != 42 {
			t.Errorf("maxDecompressionRatio = %d, want 42", o.maxDecompressionRatio)
		}
	})
}
