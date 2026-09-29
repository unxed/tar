package tar

import (
	"errors"
	"io"
	"sync"
)

// GzipReaderAt gives random access to the uncompressed bytes of a GZIP file
// that is only available as an io.ReaderAt (a member of another archive, a
// remote file), without an index saved beforehand and without a local path.
//
// It builds the checkpoint index (GZIDX: a position in the deflate stream plus
// the 32 KiB window, every spacing bytes) as it goes. A read past everything
// scanned so far continues the one sequential scan, and serves its bytes
// straight from it; a read behind the scan resumes from the nearest earlier
// checkpoint. So the first pass over the file costs one decompression, and a
// later read anywhere costs at most one checkpoint interval, never a
// decompression from the start (f4#1678).
//
// Multi-member (concatenated) files are supported. Safe for concurrent use;
// calls are serialised.
type GzipReaderAt struct {
	ra   io.ReaderAt
	size int64 // compressed size

	mu       sync.Mutex
	scan     *gzipIndexTrackingReader
	frontier int64 // uncompressed bytes the scan has produced
	total    int64 // exact uncompressed size once the scan ended, else -1
	scanErr  error
	decoded  int64 // uncompressed bytes decoded so far, for tests and tuning
}

// DefaultGzipCheckpointSpacing is the uncompressed distance between
// checkpoints: each one keeps a 32 KiB window, so this is about 1.6% memory
// overhead while a far read decodes at most this much beyond its target.
const DefaultGzipCheckpointSpacing = 2 << 20

// NewGzipReaderAt opens ra, size bytes of GZIP data, for random access.
// spacing <= 0 selects DefaultGzipCheckpointSpacing.
func NewGzipReaderAt(ra io.ReaderAt, size int64, spacing int64) (*GzipReaderAt, error) {
	if size < 0 {
		return nil, errors.New("tar: GzipReaderAt requires a known size")
	}
	if spacing <= 0 {
		spacing = DefaultGzipCheckpointSpacing
	}
	scan, err := NewGzipIndexTrackingReader(io.NewSectionReader(ra, 0, size))
	if err != nil {
		return nil, err
	}
	scan.spacing = spacing
	return &GzipReaderAt{ra: ra, size: size, scan: scan, total: -1}, nil
}

// Size returns the uncompressed size once a read has reached the end of the
// data, and -1 before that.
func (g *GzipReaderAt) Size() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.total
}

// Decoded reports how many uncompressed bytes have been decoded in total.
func (g *GzipReaderAt) Decoded() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.decoded
}

// Close releases the scan. The underlying ReaderAt is the caller's.
func (g *GzipReaderAt) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.scan == nil {
		return nil
	}
	err := g.scan.Close()
	g.scan = nil
	if g.scanErr == nil {
		g.scanErr = errors.New("tar: GzipReaderAt is closed")
	}
	return err
}

// ReadAt implements io.ReaderAt over the uncompressed data.
func (g *GzipReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("tar: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.total >= 0 && off >= g.total {
		return 0, io.EOF
	}
	if off >= g.frontier {
		return g.readFromScan(p, off)
	}
	return g.readFromCheckpoint(p, off)
}

// readFromScan advances the sequential scan to off and reads p from it.
func (g *GzipReaderAt) readFromScan(p []byte, off int64) (int, error) {
	if g.scanErr != nil {
		return 0, g.scanErr
	}
	if skip := off - g.frontier; skip > 0 {
		n, err := io.CopyN(io.Discard, g.scan, skip)
		g.frontier += n
		g.decoded += n
		if err != nil {
			return g.endOfScan(0, err)
		}
	}
	n, err := io.ReadFull(g.scan, p)
	g.frontier += int64(n)
	g.decoded += int64(n)
	if err != nil {
		return g.endOfScan(n, err)
	}
	return n, nil
}

// endOfScan turns the scan's end or failure into a ReadAt result. The end of
// the data is io.EOF (a stream cut short inside the deflate data comes back
// from the decoder as its own error, not as a clean end); a real failure is
// remembered, so later reads at the frontier do not retry a broken stream.
func (g *GzipReaderAt) endOfScan(n int, err error) (int, error) {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		g.total = g.frontier
		return n, io.EOF
	}
	g.scanErr = err
	return n, err
}

// readFromCheckpoint reads p at off, which the scan has already passed, by
// resuming from the nearest checkpoint at or before it.
func (g *GzipReaderAt) readFromCheckpoint(p []byte, off int64) (int, error) {
	var best *gzPoint
	for i := range g.scan.points {
		pt := &g.scan.points[i]
		if int64(pt.uncompOffset) <= off && (best == nil || pt.uncompOffset > best.uncompOffset) {
			best = pt
		}
	}
	if best == nil {
		return 0, errors.New("tar: no gzip checkpoint at or before the offset")
	}
	rc, err := resumeAtPoint(g.ra, best)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()

	if skip := off - int64(best.uncompOffset); skip > 0 {
		n, err := io.CopyN(io.Discard, rc, skip)
		g.decoded += n
		if err != nil {
			return 0, eofOr(err)
		}
	}
	n, err := io.ReadFull(rc, p)
	g.decoded += int64(n)
	return n, eofOr(err)
}

func eofOr(err error) error {
	if err == io.ErrUnexpectedEOF {
		return io.EOF
	}
	return err
}
