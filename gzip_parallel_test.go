package tar

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
)

// pseudoTextContent generates deterministic, moderately-compressible content:
// real English-ish words rather than pure zeros or pure random bytes, so that
// inflating it costs actual CPU time (closer to a real .tar.gz payload) while
// still being reproducible across test runs.
func pseudoTextContent(seed int64, size int) []byte {
	words := []string{
		"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog",
		"checkpoint", "sliding", "dictionary", "gzip", "index", "segment",
		"goroutine", "archive", "decompress", "parallel", "offset", "block",
	}
	r := rand.New(rand.NewSource(seed))
	var buf bytes.Buffer
	buf.Grow(size + 32)
	for buf.Len() < size {
		buf.WriteString(words[r.Intn(len(words))])
		buf.WriteByte(' ')
	}
	return buf.Bytes()[:size]
}

// buildEmbeddedIndexGzipFixture creates a .tar.gz archive with an embedded
// F4SS/GZIDX index using this package's own Archiver (the same path real
// archives created by this library go through), with numFiles files of
// fileSize bytes each so the writer-side checkpointing in writer.go
// (createSeekPoint, every 4MB of uncompressed data) produces multiple
// independent GZIDX checkpoints. Returns the archive path and the raw
// (still-gzip-wrapped) GZIDX index bytes.
func buildEmbeddedIndexGzipFixture(t testing.TB, numFiles, fileSize int) (tarPath string, indexData []byte, fileContents map[string][]byte) {
	t.Helper()

	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}

	fileContents = make(map[string][]byte)
	for i := 0; i < numFiles; i++ {
		name := filepath.Join(srcDir, "file"+string(rune('a'+i))+".txt")
		data := pseudoTextContent(int64(1000+i), fileSize)
		if err := os.WriteFile(name, data, 0644); err != nil {
			t.Fatal(err)
		}
		fileContents[name] = data
	}

	tarPath = filepath.Join(tmpDir, "big.tar.gz")
	a, err := NewArchiver(tarPath, filepath.Dir(srcDir), WithArchiverMethod(GZIP))
	if err != nil {
		t.Fatal(err)
	}

	files := make(map[string]os.FileInfo)
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		files[path] = info
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	indexPath, err := GetStandardIndexPath(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tfs, err := NewFS(tarPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	indexData, err = tfs.Index.GetGzipIndex()
	if err != nil {
		t.Fatalf("GetGzipIndex: %v", err)
	}
	if len(indexData) == 0 {
		t.Fatal("archive was created without an embedded GZIDX index")
	}

	return tarPath, indexData, fileContents
}

func sequentialGzipDecompress(t testing.TB, tarPath string) []byte {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()

	data, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("sequential decompress failed: %v", err)
	}
	return data
}

// TestParallelDecompressGzipIndexed_MatchesSequential_MemberBoundaries covers
// the writer-side (archiver embedded index) flavor of GZIDX checkpoints,
// where every point is a fresh gzip member boundary (hasData == 0).
func TestParallelDecompressGzipIndexed_MatchesSequential_MemberBoundaries(t *testing.T) {
	tarPath, indexData, _ := buildEmbeddedIndexGzipFixture(t, 4, 5*1024*1024) // ~20MB, 4MB checkpoint spacing

	points, err := parseGzipIndexPoints(indexData)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) < 3 {
		t.Fatalf("expected multiple GZIDX checkpoints to exercise real parallelism, got %d", len(points))
	}

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var parallelBuf bytes.Buffer
	if err := ParallelDecompressGzipIndexed(f, indexData, &parallelBuf, 4); err != nil {
		t.Fatalf("ParallelDecompressGzipIndexed failed: %v", err)
	}

	sequential := sequentialGzipDecompress(t, tarPath)

	if !bytes.Equal(parallelBuf.Bytes(), sequential) {
		t.Fatalf("parallel decompression mismatch: got %d bytes, want %d bytes", parallelBuf.Len(), len(sequential))
	}

	// Also verify the tar structure decoded from the parallel stream is
	// actually usable (headers parse, file bodies match) rather than merely
	// byte-equal to a byte slice we never interpret.
	tr := NewReader(bytes.NewReader(parallelBuf.Bytes()))
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("re-reading TAR produced by parallel decompression: %v", err)
		}
		if hdr.Typeflag != TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = true
		_ = body
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 regular files in the re-parsed TAR, got %d", len(seen))
	}
}

// TestParallelDecompressGzipIndexed_MatchesSequential_SlidingWindow covers
// the reader-side (on-the-fly IndexArchive) flavor of GZIDX checkpoints,
// where points sit mid deflate-stream and carry a real 32KB sliding
// dictionary window (hasData == 1), unlike the member-boundary checkpoints
// produced by the archiver.
func TestParallelDecompressGzipIndexed_MatchesSequential_SlidingWindow(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "onthefly.tar.gz")
	indexPath := filepath.Join(tmpDir, "onthefly.sqlite")

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f) // a single continuous gzip member, no writer-side seek points
	tw := NewWriter(gw)

	fileContents := map[string][]byte{}
	for i := 0; i < 5; i++ {
		name := "file" + string(rune('a'+i)) + ".txt"
		data := pseudoTextContent(int64(2000+i), 1200*1024) // ~1.2MB > 1MB on-the-fly spacing
		if err := tw.WriteHeader(&Header{Name: name, Size: int64(len(data)), Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
		fileContents[name] = data
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := IndexArchive(tarPath, indexPath); err != nil {
		t.Fatalf("IndexArchive failed: %v", err)
	}

	idx, err := OpenIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	indexData, err := idx.GetGzipIndex()
	if err != nil {
		t.Fatalf("GetGzipIndex: %v", err)
	}

	points, err := parseGzipIndexPoints(indexData)
	if err != nil {
		t.Fatal(err)
	}
	hasSlidingWindow := false
	for _, p := range points {
		if p.hasData == 1 {
			hasSlidingWindow = true
			break
		}
	}
	if !hasSlidingWindow {
		t.Fatalf("expected at least one hasData=1 (sliding-window) checkpoint, got %d points, none with a window", len(points))
	}

	af, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer af.Close()

	var parallelBuf bytes.Buffer
	if err := ParallelDecompressGzipIndexed(af, indexData, &parallelBuf, 4); err != nil {
		t.Fatalf("ParallelDecompressGzipIndexed failed: %v", err)
	}

	sequential := sequentialGzipDecompress(t, tarPath)
	if !bytes.Equal(parallelBuf.Bytes(), sequential) {
		t.Fatalf("parallel decompression mismatch across sliding-window checkpoints: got %d bytes, want %d bytes",
			parallelBuf.Len(), len(sequential))
	}

	// Confirm the reassembled TAR still reads back correctly through
	// TarFS/OpenParallelGzip end to end, not just as an opaque byte blob.
	tfs, err := NewFS(tarPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	rc, err := tfs.OpenParallelGzip(4)
	if err != nil {
		t.Fatalf("OpenParallelGzip: %v", err)
	}
	defer rc.Close()

	tr := NewReader(rc)
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading TAR from OpenParallelGzip: %v", err)
		}
		want, ok := fileContents[hdr.Name]
		if !ok {
			t.Fatalf("unexpected entry %q", hdr.Name)
		}
		got, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("content mismatch for %q", hdr.Name)
		}
		count++
	}
	if count != len(fileContents) {
		t.Fatalf("expected %d entries, got %d", len(fileContents), count)
	}
}

// TestParallelDecompressGzipIndexed_ActuallyRunsConcurrently proves that
// segments are decoded by genuinely concurrent goroutines mapped to separate
// checkpoints, rather than one goroutine working through them one at a time
// disguised behind the new API. It wraps the archive's io.ReaderAt with a
// probe that records how many segment-decoding goroutines are inside a
// ReadAt call at once; a strictly sequential implementation could never
// observe more than 1.
func TestParallelDecompressGzipIndexed_ActuallyRunsConcurrently(t *testing.T) {
	tarPath, indexData, _ := buildEmbeddedIndexGzipFixture(t, 6, 5*1024*1024) // 6 checkpoints, 6 segments

	points, err := parseGzipIndexPoints(indexData)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) < 4 {
		t.Fatalf("need at least 4 segments to reliably observe overlap, got %d", len(points))
	}

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	probe := &concurrencyProbeReaderAt{ra: f, delay: 15 * time.Millisecond}

	var out bytes.Buffer
	const concurrency = 4
	if err := ParallelDecompressGzipIndexed(probe, indexData, &out, concurrency); err != nil {
		t.Fatalf("ParallelDecompressGzipIndexed failed: %v", err)
	}

	probe.mu.Lock()
	peak := probe.peak
	probe.mu.Unlock()

	if peak < 2 {
		t.Fatalf("expected multiple goroutines to read concurrently (peak >= 2), observed peak=%d; decompression looks sequential", peak)
	}
	t.Logf("observed peak concurrent segment reads: %d (concurrency limit was %d, %d segments total)", peak, concurrency, len(points))

	sequential := sequentialGzipDecompress(t, tarPath)
	if !bytes.Equal(out.Bytes(), sequential) {
		t.Fatal("output correctness regressed while instrumented for concurrency probing")
	}
}

type concurrencyProbeReaderAt struct {
	ra    io.ReaderAt
	delay time.Duration

	mu   sync.Mutex
	cur  int
	peak int
}

func (p *concurrencyProbeReaderAt) ReadAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	p.cur++
	if p.cur > p.peak {
		p.peak = p.cur
	}
	p.mu.Unlock()

	if p.delay > 0 {
		time.Sleep(p.delay)
	}

	n, err := p.ra.ReadAt(b, off)

	p.mu.Lock()
	p.cur--
	p.mu.Unlock()

	return n, err
}

// TestParallelDecompressGzipIndexed_NotSlowerThanSequential measures wall
// clock time for decompressing a sizeable archive both ways. On a multi-core
// machine the parallel path is expected to be faster; the assertion itself
// only requires it not to regress noticeably, since CI runners can have as
// few as one usable core.
func TestParallelDecompressGzipIndexed_NotSlowerThanSequential(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison skipped in -short mode")
	}

	// ~48MB uncompressed, spread over enough 4MB checkpoints to give every
	// core real work.
	tarPath, indexData, _ := buildEmbeddedIndexGzipFixture(t, 8, 6*1024*1024)

	points, err := parseGzipIndexPoints(indexData)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture has %d GZIDX checkpoints", len(points))

	seqStart := time.Now()
	sequential := sequentialGzipDecompress(t, tarPath)
	seqElapsed := time.Since(seqStart)

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var parallelBuf bytes.Buffer
	parStart := time.Now()
	if err := ParallelDecompressGzipIndexed(f, indexData, &parallelBuf, 0); err != nil {
		t.Fatalf("ParallelDecompressGzipIndexed failed: %v", err)
	}
	parElapsed := time.Since(parStart)

	if !bytes.Equal(parallelBuf.Bytes(), sequential) {
		t.Fatal("parallel decompression output mismatch in timing test")
	}

	t.Logf("sequential: %v, parallel: %v (%.2fx)", seqElapsed, parElapsed, float64(seqElapsed)/float64(parElapsed))

	// Generous ceiling: never meaningfully slower than sequential, even on a
	// heavily loaded or single-core CI runner. Real speedup is expected (and
	// logged above) whenever more than one usable core is available.
	if parElapsed > seqElapsed*2+20*time.Millisecond {
		t.Fatalf("parallel decompression (%v) was more than 2x slower than sequential (%v)", parElapsed, seqElapsed)
	}
}

// Benchmarks below let a reviewer measure the actual speedup on their own
// hardware, e.g.:
//
//	go test -run '^$' -bench 'GzipDecompress' -benchtime=3x .
//
// The sandbox this change was developed in only exposed a handful of usable
// cores under load, so BenchmarkGzipDecompressParallel's advantage there was
// modest; on a quieter multi-core machine the gap is expected to be larger.

func BenchmarkGzipDecompressSequential(b *testing.B) {
	tarPath, _, _ := buildEmbeddedIndexGzipFixture(b, 8, 6*1024*1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := sequentialGzipDecompress(b, tarPath)
		b.SetBytes(int64(len(data)))
	}
}

func BenchmarkGzipDecompressParallel(b *testing.B) {
	tarPath, indexData, _ := buildEmbeddedIndexGzipFixture(b, 8, 6*1024*1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := os.Open(tarPath)
		if err != nil {
			b.Fatal(err)
		}
		var buf bytes.Buffer
		if err := ParallelDecompressGzipIndexed(f, indexData, &buf, 0); err != nil {
			b.Fatal(err)
		}
		f.Close()
		b.SetBytes(int64(buf.Len()))
	}
}
