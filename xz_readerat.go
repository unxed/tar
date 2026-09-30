package tar

import (
	"errors"
	"io"
	"sync"

	"github.com/unxed/xz"
)

// XzReaderAt gives random access to the uncompressed bytes of an .xz file
// available only as an io.ReaderAt. The file's own index, which lists every
// block with its compressed and uncompressed size, is read from the end, so the
// exact uncompressed size is known at once and a read decodes only the block or
// blocks it falls in. A file written as one block (the default for a single
// thread) can only be decoded from its start; there a forward read continues
// the decoder it has and only a read going back starts over (unxed/f4#1678).
// Safe for concurrent use; calls are serialised.
type XzReaderAt struct {
	ra     io.ReaderAt
	blocks []xz.Block
	total  int64

	mu      sync.Mutex
	decoded int64
	cur     *xzCursor
}

type xzCursor struct {
	block int
	rd    io.Reader
	close io.Closer
	pos   int64 // uncompressed offset of the decoder's next byte
}

// NewXzReaderAt opens ra, size bytes of xz data.
func NewXzReaderAt(ra io.ReaderAt, size int64) (*XzReaderAt, error) {
	if size < 0 {
		return nil, errors.New("tar: XzReaderAt requires a known size")
	}
	blocks, err := xz.ParseBlocks(ra, size)
	if err != nil {
		return nil, err
	}
	var total int64
	if n := len(blocks); n > 0 {
		last := blocks[n-1]
		total = last.UncompressedOffset + last.UncompressedSize
	}
	return &XzReaderAt{ra: ra, blocks: blocks, total: total}, nil
}

// Size returns the uncompressed size, known from the file's index.
func (x *XzReaderAt) Size() int64 { return x.total }

// Decoded reports how many uncompressed bytes have been decoded in total.
func (x *XzReaderAt) Decoded() int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.decoded
}

// Close releases the decoder. The ReaderAt is the caller's.
func (x *XzReaderAt) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dropLocked()
	return nil
}

func (x *XzReaderAt) dropLocked() {
	if x.cur != nil {
		if x.cur.close != nil {
			_ = x.cur.close.Close()
		}
		x.cur = nil
	}
}

// ReadAt implements io.ReaderAt over the uncompressed data.
func (x *XzReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("tar: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()

	done := 0
	for done < len(p) {
		at := off + int64(done)
		idx := x.blockAt(at)
		if idx < 0 {
			return done, io.EOF
		}
		n, err := x.readInBlockLocked(idx, p[done:], at)
		done += n
		if err != nil && err != io.EOF {
			return done, err
		}
		if n == 0 {
			return done, io.EOF
		}
	}
	return done, nil
}

// blockAt binary-searches the block holding uncompressed offset at; -1 is past
// the end.
func (x *XzReaderAt) blockAt(at int64) int {
	lo, hi := 0, len(x.blocks)
	for lo < hi {
		mid := (lo + hi) / 2
		b := x.blocks[mid]
		switch {
		case at < b.UncompressedOffset:
			hi = mid
		case at >= b.UncompressedOffset+b.UncompressedSize:
			lo = mid + 1
		default:
			return mid
		}
	}
	return -1
}

func (x *XzReaderAt) readInBlockLocked(idx int, p []byte, at int64) (int, error) {
	b := x.blocks[idx]
	if x.cur != nil && (x.cur.block != idx || x.cur.pos > at) {
		x.dropLocked()
	}
	if x.cur == nil {
		cfg := xz.ReaderConfig{}
		rd, err := cfg.NewBlockReader(io.NewSectionReader(x.ra, b.Offset, b.CompressedSize), b.StreamFlags)
		if err != nil {
			return 0, err
		}
		c := &xzCursor{block: idx, rd: rd, pos: b.UncompressedOffset}
		if cl, ok := rd.(io.Closer); ok {
			c.close = cl
		}
		x.cur = c
	}
	if skip := at - x.cur.pos; skip > 0 {
		n, err := io.CopyN(io.Discard, x.cur.rd, skip)
		x.cur.pos += n
		x.decoded += n
		if err != nil {
			x.dropLocked()
			return 0, err
		}
	}
	if room := b.UncompressedOffset + b.UncompressedSize - at; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := io.ReadFull(x.cur.rd, p)
	x.cur.pos += int64(n)
	x.decoded += int64(n)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	if err != nil && err != io.EOF {
		x.dropLocked()
	}
	return n, err
}
