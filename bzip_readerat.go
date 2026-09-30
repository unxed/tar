package tar

import (
	"bytes"
	"compress/bzip2"
	"errors"
	"io"
	"sort"
	"sync"
)

// BzipReaderAt gives random access to the uncompressed bytes of a bzip2 file
// available only as an io.ReaderAt. A bzip2 stream is a sequence of blocks that
// are compressed independently, each starting with the 48-bit magic
// 0x314159265359 at any bit position, so the file is scanned once for those
// magics and a read decodes only the block or blocks it falls in. A block is
// decoded on its own by giving the decoder a one-block stream made of its bits
// (the block's own CRC doubles as the stream CRC of a single block). Sizes of
// the blocks are learned by decoding them, so the first read at a far offset
// decodes the blocks before it once; after that the sizes are known. Several
// concatenated streams are supported (unxed/f4#1678). Safe for concurrent use;
// calls are serialised.
type BzipReaderAt struct {
	ra     io.ReaderAt
	size   int64
	blocks []bzBlock // compressed extent of each block, in order

	mu sync.Mutex
	// starts[k] is the uncompressed start of block k, known for the blocks
	// sized so far; block k has a known size once starts[k+1] exists, and
	// every block is sized when len(starts) == len(blocks)+1.
	starts   []int64
	decoded  int64
	cacheIdx int
	cache    []byte
}

type bzBlock struct {
	bit   int64 // bit offset of the block's magic
	end   int64 // bit offset of the next magic (block or end of stream)
	level byte  // '1'..'9' of the stream it belongs to
}

const (
	bzBlockMagic = 0x314159265359
	bzEndMagic   = 0x177245385090
	bzMagicMask  = 1<<48 - 1
	// bzMaxBlock bounds one decoded block, which the initial run-length
	// encoding can make far larger than the 900 kB block size it stands for.
	bzMaxBlock = 256 << 20
)

// NewBzipReaderAt opens ra, size bytes of bzip2 data.
func NewBzipReaderAt(ra io.ReaderAt, size int64) (*BzipReaderAt, error) {
	if size < 14 {
		return nil, errors.New("tar: not a bzip2 stream")
	}
	var head [4]byte
	if _, err := ra.ReadAt(head[:], 0); err != nil {
		return nil, err
	}
	if !bzHeader(head[:]) {
		return nil, errors.New("tar: not a bzip2 stream")
	}
	blocks, err := scanBzipBlocks(ra, size)
	if err != nil {
		return nil, err
	}
	return &BzipReaderAt{ra: ra, size: size, blocks: blocks, starts: []int64{0}, cacheIdx: -1}, nil
}

func bzHeader(b []byte) bool {
	return len(b) >= 4 && b[0] == 'B' && b[1] == 'Z' && b[2] == 'h' && b[3] >= '1' && b[3] <= '9'
}

// scanBzipBlocks finds every block magic and end-of-stream magic in the file.
func scanBzipBlocks(ra io.ReaderAt, size int64) ([]bzBlock, error) {
	type mark struct {
		bit int64
		end bool
	}
	var marks []mark

	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var window uint64
	var bitPos int64 // bits consumed so far
	for off := int64(0); off < size; off += chunk {
		n := int64(chunk)
		if off+n > size {
			n = size - off
		}
		if _, err := ra.ReadAt(buf[:n], off); err != nil && err != io.EOF {
			return nil, err
		}
		for _, b := range buf[:n] {
			for i := 7; i >= 0; i-- {
				window = window<<1 | uint64(b>>uint(i)&1)
				bitPos++
				if bitPos < 48 {
					continue
				}
				switch window & bzMagicMask {
				case bzBlockMagic:
					marks = append(marks, mark{bit: bitPos - 48})
				case bzEndMagic:
					marks = append(marks, mark{bit: bitPos - 48, end: true})
				}
			}
		}
	}

	var blocks []bzBlock
	level := byte(0)
	if hdr := make([]byte, 4); true {
		if _, err := ra.ReadAt(hdr, 0); err != nil {
			return nil, err
		}
		level = hdr[3]
	}
	open := -1 // index in blocks of the block waiting for its end
	for _, m := range marks {
		if open >= 0 {
			blocks[open].end = m.bit
			open = -1
		}
		if m.end {
			// The end magic is followed by the 32-bit stream CRC and padding
			// to a byte; a following stream starts with its own header.
			next := (m.bit + 48 + 32 + 7) / 8
			if next+4 <= size {
				var h [4]byte
				if _, err := ra.ReadAt(h[:], next); err == nil && bzHeader(h[:]) {
					level = h[3]
				}
			}
			continue
		}
		blocks = append(blocks, bzBlock{bit: m.bit, level: level})
		open = len(blocks) - 1
	}
	if open >= 0 {
		return nil, errors.New("tar: bzip2 stream has no end")
	}
	return blocks, nil
}

// Size returns the uncompressed size once every block has been sized, else -1.
func (z *BzipReaderAt) Size() int64 {
	z.mu.Lock()
	defer z.mu.Unlock()
	if len(z.starts) == len(z.blocks)+1 {
		return z.starts[len(z.starts)-1]
	}
	return -1
}

// Decoded reports how many uncompressed bytes have been decoded in total.
func (z *BzipReaderAt) Decoded() int64 {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.decoded
}

// Close drops the cached block. The ReaderAt is the caller's.
func (z *BzipReaderAt) Close() error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.cache, z.cacheIdx = nil, -1
	return nil
}

// ReadAt implements io.ReaderAt over the uncompressed data.
func (z *BzipReaderAt) ReadAt(p []byte, off int64) (int, error) {
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
		idx, err := z.blockAtLocked(at)
		if err != nil {
			return done, err
		}
		if idx < 0 {
			return done, io.EOF
		}
		data, err := z.blockDataLocked(idx)
		if err != nil {
			return done, err
		}
		n := copy(p[done:], data[at-z.starts[idx]:])
		done += n
		if n == 0 {
			return done, io.EOF
		}
	}
	return done, nil
}

// blockAtLocked finds the block holding uncompressed offset at, sizing blocks
// (by decoding them) as far as needed; -1 is past the end.
func (z *BzipReaderAt) blockAtLocked(at int64) (int, error) {
	for {
		last := len(z.starts) - 1 // blocks 0..last-1 are sized
		if last > 0 && at < z.starts[last] {
			return sort.Search(last, func(i int) bool { return z.starts[i+1] > at }), nil
		}
		if last == len(z.blocks) {
			return -1, nil
		}
		data, err := z.blockDataLocked(last)
		if err != nil {
			return -1, err
		}
		z.starts = append(z.starts, z.starts[last]+int64(len(data)))
	}
}

// blockDataLocked returns block i decoded, keeping the last one.
func (z *BzipReaderAt) blockDataLocked(i int) ([]byte, error) {
	if z.cacheIdx == i && z.cache != nil {
		return z.cache, nil
	}
	b := z.blocks[i]
	stream, err := z.oneBlockStream(b)
	if err != nil {
		return nil, err
	}
	rd := bzip2.NewReader(bytes.NewReader(stream))
	var out bytes.Buffer
	if _, err := io.Copy(&out, io.LimitReader(rd, bzMaxBlock+1)); err != nil {
		return nil, err
	}
	if out.Len() > bzMaxBlock {
		return nil, errors.New("tar: bzip2 block decodes to too much data")
	}
	z.decoded += int64(out.Len())
	z.cache, z.cacheIdx = out.Bytes(), i
	return z.cache, nil
}

// oneBlockStream builds a bzip2 stream of the single block b: the stream
// header, the block's bits, the end-of-stream magic and the stream CRC, which
// for one block is the block's own CRC (the 32 bits after its magic).
func (z *BzipReaderAt) oneBlockStream(b bzBlock) ([]byte, error) {
	firstByte := b.bit / 8
	lastByte := (b.end + 7) / 8
	if firstByte >= lastByte || lastByte > z.size {
		return nil, errors.New("tar: bad bzip2 block extent")
	}
	raw := make([]byte, lastByte-firstByte+1)
	if _, err := z.ra.ReadAt(raw[:lastByte-firstByte], firstByte); err != nil && err != io.EOF {
		return nil, err
	}
	shift := uint(b.bit % 8)
	nbits := b.end - b.bit

	out := make([]byte, 0, len(raw)+16)
	out = append(out, 'B', 'Z', 'h', b.level)

	var acc uint64 // bits waiting to be written, right-aligned
	var have uint  // how many
	put := func(v uint64, n uint) {
		for n > 0 {
			take := n
			if take > 32 {
				take = 32
			}
			part := (v >> (n - take)) & (1<<take - 1)
			acc = acc<<take | part
			have += take
			n -= take
			for have >= 8 {
				out = append(out, byte(acc>>(have-8)))
				have -= 8
				acc &= 1<<have - 1
			}
		}
	}
	// The block's bits, from bit `shift` of raw.
	for i := int64(0); i < nbits; {
		byteIdx := (int64(shift) + i) / 8
		bitInByte := uint((int64(shift) + i) % 8)
		avail := 8 - bitInByte
		n := uint(avail)
		if int64(n) > nbits-i {
			n = uint(nbits - i)
		}
		v := uint64(raw[byteIdx]>>(avail-n)) & (1<<n - 1)
		put(v, n)
		i += int64(n)
	}
	// The stream CRC of a one-block stream is the block's own: the 32 bits
	// after its magic.
	put(bzEndMagic, 48)
	put(bzBits(raw, shift+48, 32), 32)
	if have > 0 {
		out = append(out, byte(acc<<(8-have)))
	}
	return out, nil
}

// bzBits reads n (<= 32) bits starting at bit position pos of b, high bit first.
func bzBits(b []byte, pos uint, n uint) uint64 {
	var v uint64
	for i := uint(0); i < n; i++ {
		p := pos + i
		v = v<<1 | uint64(b[p/8]>>(7-p%8)&1)
	}
	return v
}
