package tar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// ZstdReaderAt gives random access to the uncompressed bytes of a zstd file
// available only as an io.ReaderAt, using the frame boundaries. A zstd file
// made of several frames (pzstd, a chunked writer, zstd --seekable's data
// part, f4's own block-per-frame archives) can be entered at any frame: the
// frames are found by walking their block headers, without decompressing
// them, and a read decodes only the frame or frames it falls in.
//
// A file that is one frame can only be decoded from its start, since a zstd
// block depends on the window before it; there a read continues the decoder
// it already has when it goes forward and starts again only when it goes back.
// Skippable frames are stepped over. Safe for concurrent use; calls are
// serialised (unxed/f4#1678).
type ZstdReaderAt struct {
	ra   io.ReaderAt
	size int64

	mu      sync.Mutex
	frames  []zstdFrame
	next    int64 // compressed offset of the first frame not yet in frames
	total   int64 // uncompressed size once every frame is known, else -1
	decoded int64

	cur *zstdCursor
}

type zstdFrame struct {
	comp, compLen int64
	start         int64 // uncompressed offset of the frame's first byte
	usize         int64 // uncompressed length
}

// zstdCursor is a decoder positioned inside one frame.
type zstdCursor struct {
	frame int
	dec   *zstd.Decoder
	pos   int64 // uncompressed offset of the decoder's next byte
}

// NewZstdReaderAt opens ra, size bytes of zstd data.
func NewZstdReaderAt(ra io.ReaderAt, size int64) (*ZstdReaderAt, error) {
	if size < 0 {
		return nil, errors.New("tar: ZstdReaderAt requires a known size")
	}
	z := &ZstdReaderAt{ra: ra, size: size, total: -1}
	// The first frame must be there and be zstd, or this is the wrong reader.
	var magic [4]byte
	if _, err := ra.ReadAt(magic[:], 0); err != nil {
		return nil, fmt.Errorf("tar: reading zstd magic: %w", err)
	}
	if !isZstdMagic(magic[:]) {
		return nil, errors.New("tar: not a zstd stream")
	}
	return z, nil
}

func isZstdMagic(b []byte) bool {
	m := binary.LittleEndian.Uint32(b)
	return m == 0xFD2FB528 || (m&0xFFFFFFF0) == 0x184D2A50
}

// Size returns the uncompressed size once every frame is known, else -1.
func (z *ZstdReaderAt) Size() int64 {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.total
}

// Decoded reports how many uncompressed bytes have been decoded in total.
func (z *ZstdReaderAt) Decoded() int64 {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.decoded
}

// Close releases the decoder. The ReaderAt is the caller's.
func (z *ZstdReaderAt) Close() error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.dropCursorLocked()
	return nil
}

func (z *ZstdReaderAt) dropCursorLocked() {
	if z.cur != nil {
		z.cur.dec.Close()
		z.cur = nil
	}
}

// ReadAt implements io.ReaderAt over the uncompressed data.
func (z *ZstdReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("tar: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	z.mu.Lock()
	defer z.mu.Unlock()

	done := 0
	for done < len(p) {
		at := off + int64(done)
		idx, err := z.frameAtLocked(at)
		if err != nil {
			return done, err
		}
		if idx < 0 {
			return done, io.EOF
		}
		n, err := z.readInFrameLocked(idx, p[done:], at)
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

// frameAtLocked finds the frame holding uncompressed offset at, extending the
// frame list as far as needed; -1 means at is past the end.
func (z *ZstdReaderAt) frameAtLocked(at int64) (int, error) {
	for {
		// Binary search the known frames.
		lo, hi := 0, len(z.frames)
		for lo < hi {
			mid := (lo + hi) / 2
			f := z.frames[mid]
			switch {
			case at < f.start:
				hi = mid
			case at >= f.start+f.usize:
				lo = mid + 1
			default:
				return mid, nil
			}
		}
		if z.total >= 0 {
			return -1, nil
		}
		if err := z.addFrameLocked(); err != nil {
			return -1, err
		}
	}
}

// addFrameLocked reads the next frame's boundaries, decoding it only when its
// header does not say how long it is once decompressed.
func (z *ZstdReaderAt) addFrameLocked() error {
	if z.next >= z.size {
		z.total = z.endLocked()
		return nil
	}
	compLen, fcs, skippable, err := scanZstdFrame(z.ra, z.next, z.size)
	if err != nil {
		return err
	}
	f := zstdFrame{comp: z.next, compLen: compLen, start: z.endLocked()}
	switch {
	case skippable:
		f.usize = 0
	case fcs >= 0:
		f.usize = fcs
	default:
		n, err := z.countLocked(f)
		if err != nil {
			return err
		}
		f.usize = n
	}
	z.next += compLen
	if f.usize > 0 {
		z.frames = append(z.frames, f)
	}
	if z.next >= z.size {
		z.total = f.start + f.usize
	}
	return nil
}

// endLocked is the uncompressed offset just past the last known frame.
func (z *ZstdReaderAt) endLocked() int64 {
	if len(z.frames) == 0 {
		return 0
	}
	last := z.frames[len(z.frames)-1]
	return last.start + last.usize
}

// countLocked decodes a frame to learn its uncompressed length.
func (z *ZstdReaderAt) countLocked(f zstdFrame) (int64, error) {
	dec, err := zstd.NewReader(io.NewSectionReader(z.ra, f.comp, f.compLen), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	n, err := io.Copy(io.Discard, dec)
	z.decoded += n
	return n, err
}

// readInFrameLocked reads from frame idx at uncompressed offset at, up to the
// end of the frame, continuing the current decoder when it sits in that frame
// at or before at.
func (z *ZstdReaderAt) readInFrameLocked(idx int, p []byte, at int64) (int, error) {
	f := z.frames[idx]
	if z.cur != nil && (z.cur.frame != idx || z.cur.pos > at) {
		z.dropCursorLocked()
	}
	if z.cur == nil {
		dec, err := zstd.NewReader(io.NewSectionReader(z.ra, f.comp, f.compLen), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return 0, err
		}
		z.cur = &zstdCursor{frame: idx, dec: dec, pos: f.start}
	}
	if skip := at - z.cur.pos; skip > 0 {
		n, err := io.CopyN(io.Discard, z.cur.dec, skip)
		z.cur.pos += n
		z.decoded += n
		if err != nil {
			z.dropCursorLocked()
			return 0, err
		}
	}
	if room := f.start + f.usize - at; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := io.ReadFull(z.cur.dec, p)
	z.cur.pos += int64(n)
	z.decoded += int64(n)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	if err != nil && err != io.EOF {
		z.dropCursorLocked()
	}
	return n, err
}

// scanZstdFrame reads one frame's extent at off from its headers alone. It
// returns the frame's compressed length, its uncompressed length when the
// frame header records it (else -1), and whether it is a skippable frame.
func scanZstdFrame(ra io.ReaderAt, off, size int64) (compLen, fcs int64, skippable bool, err error) {
	var head [8]byte
	if _, err := ra.ReadAt(head[:4], off); err != nil {
		return 0, 0, false, fmt.Errorf("tar: zstd frame at %d: %w", off, err)
	}
	magic := binary.LittleEndian.Uint32(head[:4])
	if magic&0xFFFFFFF0 == 0x184D2A50 {
		if _, err := ra.ReadAt(head[4:8], off+4); err != nil {
			return 0, 0, false, err
		}
		n := 8 + int64(binary.LittleEndian.Uint32(head[4:8]))
		if off+n > size {
			return 0, 0, false, errors.New("tar: zstd skippable frame runs past the end")
		}
		return n, 0, true, nil
	}
	if magic != 0xFD2FB528 {
		return 0, 0, false, fmt.Errorf("tar: bad zstd frame magic at %d", off)
	}
	pos := off + 4
	var desc [1]byte
	if _, err := ra.ReadAt(desc[:], pos); err != nil {
		return 0, 0, false, err
	}
	pos++
	d := desc[0]
	single := d&0x20 != 0
	checksum := d&0x04 != 0
	if !single {
		pos++ // window descriptor
	}
	pos += [4]int64{0, 1, 2, 4}[d&3] // dictionary id
	fcs = -1
	var fieldLen int64
	switch d >> 6 {
	case 0:
		if single {
			fieldLen = 1
		}
	case 1:
		fieldLen = 2
	case 2:
		fieldLen = 4
	case 3:
		fieldLen = 8
	}
	if fieldLen > 0 {
		var b [8]byte
		if _, err := ra.ReadAt(b[:fieldLen], pos); err != nil {
			return 0, 0, false, err
		}
		v := int64(binary.LittleEndian.Uint64(b[:]))
		if d>>6 == 1 {
			v += 256
		}
		fcs = v
		pos += fieldLen
	}
	for {
		var bh [3]byte
		if _, err := ra.ReadAt(bh[:], pos); err != nil {
			return 0, 0, false, fmt.Errorf("tar: zstd block header at %d: %w", pos, err)
		}
		h := uint32(bh[0]) | uint32(bh[1])<<8 | uint32(bh[2])<<16
		last := h&1 != 0
		btype := (h >> 1) & 3
		bsize := int64(h >> 3)
		pos += 3
		switch btype {
		case 0, 2:
			pos += bsize
		case 1:
			pos++
		default:
			return 0, 0, false, errors.New("tar: reserved zstd block type")
		}
		if last {
			break
		}
	}
	if checksum {
		pos += 4
	}
	if pos > size {
		return 0, 0, false, errors.New("tar: zstd frame runs past the end")
	}
	return pos - off, fcs, false, nil
}
