package tar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"

	"golang.org/x/sync/errgroup"
)

// ErrNoGzipIndex is returned by the parallel GZIP decompression helpers when
// no usable GZIDX checkpoints are available.
var ErrNoGzipIndex = errors.New("tar: no GZIDX checkpoints available for parallel decompression")

// gzipSegment describes one independently-decodable slice of the uncompressed
// stream, anchored at a saved GZIDX checkpoint (see gzPoint).
type gzipSegment struct {
	point gzPoint
	// uncompLen is the number of uncompressed bytes belonging to this
	// segment, or -1 for the last segment (read until the stream ends).
	uncompLen int64
}

// buildGzipSegments turns a GZIDX checkpoint list into a sequence of
// non-overlapping segments covering the whole uncompressed stream, one per
// checkpoint. Checkpoints are sorted by uncompressed offset first so the
// result is well-defined even if the caller's index isn't already ordered.
func buildGzipSegments(points []gzPoint) []gzipSegment {
	if len(points) == 0 {
		return nil
	}
	sorted := make([]gzPoint, len(points))
	copy(sorted, points)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].uncompOffset < sorted[j].uncompOffset })

	segments := make([]gzipSegment, len(sorted))
	for i, p := range sorted {
		length := int64(-1)
		if i+1 < len(sorted) {
			length = int64(sorted[i+1].uncompOffset - p.uncompOffset)
		}
		segments[i] = gzipSegment{point: p, uncompLen: length}
	}
	return segments
}

// decodeGzipSegment inflates a single checkpoint-anchored segment in
// isolation: it opens its own decompression stream starting at the
// checkpoint's compressed offset (seeded with the saved 32KB sliding-window
// dictionary when the checkpoint isn't a plain member boundary) and reads
// exactly the bytes belonging to that segment.
func decodeGzipSegment(ra io.ReaderAt, seg gzipSegment) ([]byte, error) {
	rc, err := resumeAtPoint(ra, &seg.point)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	if seg.uncompLen < 0 {
		return io.ReadAll(rc)
	}
	buf := make([]byte, seg.uncompLen)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ParallelDecompressGzipIndexed decompresses a whole GZIP stream described by
// a previously saved GZIDX index (see GzipIndexExporter.ExportGzipIndex /
// FileIndex.GetGzipIndex) by splitting the work across the saved checkpoints:
// each segment between two consecutive checkpoints is inflated by its own
// goroutine, independently seeded from that checkpoint's saved offset and
// (when present) 32KB sliding-dictionary window, so multiple CPU cores can
// decompress the same archive concurrently instead of the single sequential
// decompressor used by OpenReader/Extract. Segments are written to w strictly
// in ascending order, so the output is byte-for-byte identical to a plain
// sequential decompression of the same stream.
//
// concurrency caps how many segments are inflated at once; concurrency <= 0
// selects runtime.GOMAXPROCS(0).
func ParallelDecompressGzipIndexed(ra io.ReaderAt, indexData []byte, w io.Writer, concurrency int) error {
	points, err := parseGzipIndexPoints(indexData)
	if err != nil {
		return err
	}
	if len(points) == 0 {
		return ErrNoGzipIndex
	}
	if concurrency <= 0 {
		concurrency = runtime.GOMAXPROCS(0)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	segments := buildGzipSegments(points)
	n := len(segments)

	type segResult struct {
		data []byte
		err  error
	}
	resultCh := make([]chan segResult, n)
	for i := range resultCh {
		resultCh[i] = make(chan segResult, 1)
	}

	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(concurrency)

	for i := 0; i < n; i++ {
		i := i
		g.Go(func() error {
			data, err := decodeGzipSegment(ra, segments[i])
			resultCh[i] <- segResult{data: data, err: err}
			return err
		})
	}

	writeErrCh := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			res := <-resultCh[i]
			if res.err != nil {
				writeErrCh <- fmt.Errorf("tar: parallel gzip segment %d: %w", i, res.err)
				return
			}
			if _, err := w.Write(res.data); err != nil {
				writeErrCh <- err
				return
			}
		}
		writeErrCh <- nil
	}()

	groupErr := g.Wait()
	if writeErr := <-writeErrCh; writeErr != nil {
		return writeErr
	}
	return groupErr
}

// pipeAfter runs fn in a background goroutine, feeding whatever it writes to
// the returned io.ReadCloser and propagating fn's returned error (if any) as
// the final Read error, the same way an io.Pipe is normally driven.
func pipeAfter(fn func(w io.Writer) error) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(fn(pw))
	}()
	return pr
}

// NewParallelGzipIndexedReader streams the decompressed bytes of a GZIP
// archive using ParallelDecompressGzipIndexed under the hood, so callers that
// need an io.Reader (e.g. archive/tar.NewReader or this package's
// tar.NewReader) can benefit from parallel inflate without buffering the
// whole decompressed archive in memory up front: segments are still decoded
// concurrently ahead of the reader, but handed to the consumer as an ordinary
// streaming Read().
func NewParallelGzipIndexedReader(ra io.ReaderAt, indexData []byte, concurrency int) io.ReadCloser {
	return pipeAfter(func(w io.Writer) error {
		return ParallelDecompressGzipIndexed(ra, indexData, w, concurrency)
	})
}

// OpenParallelGzip opens the underlying archive of a GZIP-compressed TarFS
// for sequential reading (e.g. with this package's NewReader, to walk every
// entry, or with archive/tar), but decompresses it with
// ParallelDecompressGzipIndexed: independent goroutines inflate the archive's
// saved GZIDX checkpoints concurrently instead of the one sequential
// decompressor OpenReader/Extract normally use. It requires the FS to have
// been opened against a GZIP archive with a saved GZIDX index (see
// GetGzipIndex); ErrNoGzipIndex is returned when none is available.
//
// concurrency caps how many segments are inflated at once; concurrency <= 0
// selects runtime.GOMAXPROCS(0).
func (t *TarFS) OpenParallelGzip(concurrency int) (io.ReadCloser, error) {
	if t.method != GZIP {
		return nil, errors.New("tar: OpenParallelGzip requires a GZIP archive")
	}

	indexData, err := t.Index.GetGzipIndex()
	if err != nil {
		return nil, err
	}
	if len(indexData) == 0 {
		return nil, ErrNoGzipIndex
	}

	mvr, size, err := OpenMultiVolume(t.ArchivePath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	var ra io.ReaderAt = mvr

	ra, _, err = checkXCrypt(ra, size, t.password)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	rc := NewParallelGzipIndexedReader(ra, indexData, concurrency)
	return &parallelGzipFile{r: rc, c: multiCloser{rc, mvr}}, nil
}

type parallelGzipFile struct {
	r io.Reader
	c multiCloser
}

func (f *parallelGzipFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *parallelGzipFile) Close() error               { return f.c.Close() }
