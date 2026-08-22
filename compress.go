package tar

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/unxed/xz"
)

// Compression formats
const (
	Store uint16 = 0
	GZIP  uint16 = 1
	BZIP2 uint16 = 2
	XZ    uint16 = 3
	ZSTD  uint16 = 4
)

type Compressor func(w io.Writer) (io.WriteCloser, error)

type Decompressor interface {
	Decompress(r io.Reader) (io.ReadCloser, error)
}

// BlockOffsetExporter exports boundary maps for block-based compression formats (ZSTD, BZIP2).
type BlockOffsetExporter interface {
	ExportBlockOffsets() []BlockOffset
}

// BlockOffsetImporter resumes decompression from a specific block boundary.
type BlockOffsetImporter interface {
	ResumeFromBlockOffset(r io.ReaderAt, bo *BlockOffset) (io.ReadCloser, error)
}

// GzipIndexExporter exports the complete serialized GZIP index (compatible with zran.c/rapidgzip).
type GzipIndexExporter interface {
	ExportGzipIndex() ([]byte, error)
}

// GzipIndexImporter resumes GZIP decompression using the serialized index.
type GzipIndexImporter interface {
	ResumeFromGzipIndex(r io.ReaderAt, indexData []byte, targetOffset int64) (reader io.ReadCloser, uncompOffset int64, err error)
}

var (
	compressors   sync.Map
	decompressors sync.Map
)

// -- GZIP --
type gzipFormat struct{}

func (gzipFormat) Decompress(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(r, 1024*1024)
	return gzip.NewReader(br)
}

type trackingByteReader struct {
	r   *bufio.Reader
	pos int64
}

func (t *trackingByteReader) Read(p []byte) (n int, err error) {
	n, err = t.r.Read(p)
	t.pos += int64(n)
	return n, err
}

func (t *trackingByteReader) ReadByte() (byte, error) {
	b, err := t.r.ReadByte()
	if err == nil {
		t.pos++
	}
	return b, err
}

type trackingWriter struct {
	w   io.Writer
	pos int64
}

func (t *trackingWriter) Write(p []byte) (n int, err error) {
	n, err = t.w.Write(p)
	t.pos += int64(n)
	return n, err
}

type gzipIndexTrackingReader struct {
	tr               *trackingByteReader
	fr               io.ReadCloser
	streamCompBase   int64
	streamUncompBase int64
	uncompOffset     int64
	spacing          int64
	points           []gzPoint
	lastCheckpoint   int64
	eof              bool
	err              error
}

func NewGzipIndexTrackingReader(r io.Reader) (*gzipIndexTrackingReader, error) {
	br := bufio.NewReaderSize(r, 1024*1024) // 1MB buffer instead of default 4KB
	tr := &trackingByteReader{r: br}
	gtr := &gzipIndexTrackingReader{
		tr:      tr,
		spacing: 1024 * 1024, // 1MB standard spacing
		points: []gzPoint{
			{
				compOffset:   0,
				uncompOffset: 0,
				bits:         0,
				hasData:      0,
			},
		},
	}
	if err := gtr.nextStream(); err != nil {
		return nil, err
	}
	return gtr, nil
}

func (g *gzipIndexTrackingReader) nextStream() error {
	var hdr [10]byte
	if _, err := io.ReadFull(g.tr, hdr[:]); err != nil {
		if err == io.EOF {
			return io.EOF
		}
		return err
	}
	if hdr[0] != 0x1f || hdr[1] != 0x8b {
		return errors.New("tar: invalid gzip magic")
	}
	if hdr[2] != 8 {
		return errors.New("tar: unsupported gzip method")
	}
	flg := hdr[3]

	if flg&0x04 != 0 {
		var xlen [2]byte
		if _, err := io.ReadFull(g.tr, xlen[:]); err != nil {
			return err
		}
		ln := int(xlen[0]) | (int(xlen[1]) << 8)
		if _, err := io.CopyN(io.Discard, g.tr, int64(ln)); err != nil {
			return err
		}
	}
	if flg&0x08 != 0 {
		if err := g.readNullTerminated(); err != nil {
			return err
		}
	}
	if flg&0x10 != 0 {
		if err := g.readNullTerminated(); err != nil {
			return err
		}
	}
	if flg&0x02 != 0 {
		var crc [2]byte
		if _, err := io.ReadFull(g.tr, crc[:]); err != nil {
			return err
		}
	}

	g.streamCompBase = g.tr.pos
	g.streamUncompBase = g.uncompOffset

	cb := func(cp flate.InflateCheckpoint) {
		absUncomp := g.streamUncompBase + cp.UncompressedOffset
		absComp := g.streamCompBase + cp.CompressedOffset

		if absUncomp-g.lastCheckpoint >= g.spacing {
			win := make([]byte, 32768)
			if len(cp.Window) > 0 {
				copy(win[32768-len(cp.Window):], cp.Window)
			}
			g.points = append(g.points, gzPoint{
				compOffset:   uint64(absComp),
				uncompOffset: uint64(absUncomp),
				bits:         cp.BitOffset,
				hasData:      1,
				window:       win,
			})
			g.lastCheckpoint = absUncomp
		}
	}

	g.fr = flate.NewReaderOpts(g.tr, flate.WithEobCallback(cb))
	return nil
}

func (g *gzipIndexTrackingReader) readNullTerminated() error {
	for {
		b, err := g.tr.ReadByte()
		if err != nil {
			return err
		}
		if b == 0 {
			return nil
		}
	}
}

func (g *gzipIndexTrackingReader) Read(p []byte) (n int, err error) {
	if g.eof {
		return 0, io.EOF
	}
	if g.err != nil {
		return 0, g.err
	}

	n, err = g.fr.Read(p)
	g.uncompOffset += int64(n)

	if err == io.EOF {
		g.fr.Close()

		var trailer [8]byte
		if _, terr := io.ReadFull(g.tr, trailer[:]); terr != nil {
			g.err = terr
			if n > 0 {
				return n, nil
			}
			return 0, terr
		}

		newHeaderOffset := g.tr.pos

		terr := g.nextStream()
		if terr == io.EOF || terr == io.ErrUnexpectedEOF {
			g.eof = true
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		if terr != nil {
			g.err = terr
			if n > 0 {
				return n, nil
			}
			return 0, terr
		}

		g.points = append(g.points, gzPoint{
			compOffset:   uint64(newHeaderOffset),
			uncompOffset: uint64(g.uncompOffset),
			bits:         0,
			hasData:      0,
		})
		g.lastCheckpoint = g.uncompOffset

		return n, nil
	}

	return n, err
}

func (g *gzipIndexTrackingReader) Close() error {
	if g.fr != nil {
		return g.fr.Close()
	}
	return nil
}

func (g *gzipIndexTrackingReader) ExportGzipIndex() ([]byte, error) {
	compSize := g.tr.pos
	uncompSize := g.uncompOffset

	gzidxBuf := new(bytes.Buffer)
	gzidxBuf.Write([]byte("GZIDX"))
	gzidxBuf.WriteByte(1) // version
	gzidxBuf.WriteByte(0) // flags

	binary.Write(gzidxBuf, binary.LittleEndian, uint64(compSize))
	binary.Write(gzidxBuf, binary.LittleEndian, uint64(uncompSize))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(g.spacing))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(32768))
	binary.Write(gzidxBuf, binary.LittleEndian, uint32(len(g.points)))

	for _, p := range g.points {
		binary.Write(gzidxBuf, binary.LittleEndian, p.compOffset)
		binary.Write(gzidxBuf, binary.LittleEndian, p.uncompOffset)
		gzidxBuf.WriteByte(p.bits)
		gzidxBuf.WriteByte(p.hasData)
	}

	for _, p := range g.points {
		if p.hasData == 1 {
			gzidxBuf.Write(p.window)
		}
	}

	var cmpBuf bytes.Buffer
	gw := gzip.NewWriter(&cmpBuf)
	_, err := gw.Write(gzidxBuf.Bytes())
	if err != nil {
		return nil, err
	}
	gw.Close()

	return cmpBuf.Bytes(), nil
}

type gzPoint struct {
	compOffset   uint64
	uncompOffset uint64
	bits         uint8
	hasData      uint8
	window       []byte
}

type resumedGzipReader struct {
	br      *bufio.Reader
	tbr     *trackingByteReader
	current io.ReadCloser
	isFlate bool
}

func (rg *resumedGzipReader) Read(p []byte) (n int, err error) {
	n, err = rg.current.Read(p)
	if err == io.EOF && rg.isFlate {
		rg.current.Close()

		var trailer [8]byte
		if _, terr := io.ReadFull(rg.br, trailer[:]); terr != nil {
			return n, terr
		}

		nextStream, terr := gzip.NewReader(rg.br)
		if terr != nil {
			if terr == io.EOF || terr == io.ErrUnexpectedEOF {
				return n, io.EOF
			}
			return n, terr
		}
		rg.current = nextStream
		rg.isFlate = false
		return n, nil
	}
	return n, err
}

func (rg *resumedGzipReader) Close() error {
	return rg.current.Close()
}

func (gzipFormat) ResumeFromGzipIndex(r io.ReaderAt, indexData []byte, targetOffset int64) (io.ReadCloser, int64, error) {
	gr, err := gzip.NewReader(bytes.NewReader(indexData))
	if err != nil {
		return nil, 0, err
	}
	defer gr.Close()

	dflidx, err := io.ReadAll(gr)
	if err != nil {
		return nil, 0, err
	}

	if len(dflidx) < 35 || string(dflidx[:5]) != "GZIDX" {
		return nil, 0, errors.New("tar: invalid GZIDX header")
	}

	version := dflidx[5]
	if version > 1 {
		return nil, 0, fmt.Errorf("tar: unsupported GZIDX version: %d", version)
	}

	numPoints := binary.LittleEndian.Uint32(dflidx[31:35])
	points := make([]gzPoint, numPoints)
	offset := 35

	for i := uint32(0); i < numPoints; i++ {
		points[i].compOffset = binary.LittleEndian.Uint64(dflidx[offset : offset+8])
		points[i].uncompOffset = binary.LittleEndian.Uint64(dflidx[offset+8 : offset+16])
		points[i].bits = dflidx[offset+16]
		points[i].hasData = dflidx[offset+17]
		offset += 18
	}

	for i := uint32(0); i < numPoints; i++ {
		if points[i].hasData == 1 {
			points[i].window = dflidx[offset : offset+32768]
			offset += 32768
		}
	}

	var best *gzPoint
	for i := range points {
		if points[i].uncompOffset <= uint64(targetOffset) {
			if best == nil || points[i].uncompOffset > best.uncompOffset {
				best = &points[i]
			}
		}
	}
	if best == nil {
		return nil, 0, errors.New("tar: no suitable seek point found")
	}

	seekOffset := int64(best.compOffset)
	sr := io.NewSectionReader(r, seekOffset, 1<<63-1)
	br := bufio.NewReaderSize(sr, 1024*1024) // 1MB buffer instead of default 4KB

	if best.hasData == 0 {
		gr, err := gzip.NewReader(br)
		if err != nil {
			return nil, 0, err
		}
		return gr, int64(best.uncompOffset), nil
	}

	cp := flate.InflateCheckpoint{
		UncompressedOffset: int64(best.uncompOffset),
		CompressedOffset:   seekOffset,
		Final:              false,
		BitOffset:          best.bits,
		Window:             best.window,
	}

	tbr := &trackingByteReader{r: br, pos: seekOffset}
	fr := flate.NewReaderOpts(tbr, flate.WithResumeFrom(cp))

	rg := &resumedGzipReader{
		br:      br,
		tbr:     tbr,
		current: fr,
		isFlate: true,
	}

	return rg, int64(best.uncompOffset), nil
}

// -- BZIP2 --
type bzip2Format struct{}

func (bzip2Format) Decompress(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(bzip2.NewReader(r)), nil
}

// -- XZ --
type xzFormat struct{}

func (xzFormat) Decompress(r io.Reader) (io.ReadCloser, error) {
	xr, err := xz.NewReader(r)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(xr), nil
}

// -- ZSTD --
type zstdFormat struct{}

var zstdDecoderPool sync.Pool

type pooledTarZstdReader struct {
	mu  sync.Mutex
	dec *zstd.Decoder
}

func (r *pooledTarZstdReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dec == nil {
		return 0, errors.New("tar: Read after Close")
	}
	return r.dec.Read(p)
}

func (r *pooledTarZstdReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.dec != nil {
		err = r.dec.Reset(nil)
		zstdDecoderPool.Put(r.dec)
		r.dec = nil
	}
	return err
}

func newTarZstdReader(r io.Reader) (io.ReadCloser, error) {
	dec, _ := zstdDecoderPool.Get().(*zstd.Decoder)
	if dec == nil {
		var err error
		concurrency := runtime.GOMAXPROCS(0) / 2
		if concurrency < 1 {
			concurrency = 1
		}
		dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(concurrency), zstd.WithDecoderMaxWindow(512<<20), zstd.WithDecoderMaxMemory(512<<20))
		if err != nil {
			return nil, err
		}
	}
	if err := dec.Reset(r); err != nil {
		zstdDecoderPool.Put(dec)
		return nil, err
	}
	return &pooledTarZstdReader{dec: dec}, nil
}

func (zstdFormat) Decompress(r io.Reader) (io.ReadCloser, error) {
	return newTarZstdReader(r)
}

// ZSTD frames are byte-aligned and independent. We can natively resume from any frame!
func (zstdFormat) ResumeFromBlockOffset(r io.ReaderAt, bo *BlockOffset) (io.ReadCloser, error) {
	sr := io.NewSectionReader(r, bo.BlockOffset, 1<<63-1)
	return newTarZstdReader(sr)
}

func init() {
	compressors.Store(GZIP, Compressor(func(w io.Writer) (io.WriteCloser, error) { return pgzip.NewWriter(w), nil }))
	decompressors.Store(GZIP, gzipFormat{})

	decompressors.Store(BZIP2, bzip2Format{})

	compressors.Store(XZ, Compressor(func(w io.Writer) (io.WriteCloser, error) { return xz.NewWriter(w) }))
	decompressors.Store(XZ, xzFormat{})

	var zstdWriterPool sync.Pool
	compressors.Store(ZSTD, Compressor(func(w io.Writer) (io.WriteCloser, error) {
		var enc *zstd.Encoder
		if v := zstdWriterPool.Get(); v != nil {
			enc = v.(*zstd.Encoder)
			enc.Reset(w)
		} else {
			enc, _ = zstd.NewWriter(w, zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
		}
		return &pooledZstdWriter{Encoder: enc, pool: &zstdWriterPool}, nil
	}))
	decompressors.Store(ZSTD, zstdFormat{})
}

type pooledZstdWriter struct {
	*zstd.Encoder
	pool *sync.Pool
}

func (p *pooledZstdWriter) Close() error {
	err := p.Encoder.Close()
	p.pool.Put(p.Encoder)
	return err
}

func RegisterCompressor(method uint16, comp Compressor) {
	compressors.Store(method, comp)
}

func RegisterDecompressor(method uint16, dcomp Decompressor) {
	decompressors.Store(method, dcomp)
}

// DetectFormat looks at the magic bytes to determine the compression method.
func DetectFormat(r io.ReaderAt) (uint16, error) {
	magic := make([]byte, 6)
	n, err := r.ReadAt(magic, 0)
	if err != nil && err != io.EOF {
		return Store, err
	}

	if n >= 2 && bytes.Equal(magic[:2], []byte{0x1f, 0x8b}) {
		return GZIP, nil
	}
	if n >= 3 && bytes.Equal(magic[:3], []byte{0x42, 0x5a, 0x68}) { // BZh
		return BZIP2, nil
	}
	if n >= 4 && bytes.Equal(magic[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return ZSTD, nil
	}
	if n >= 6 && bytes.Equal(magic[:6], []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}) {
		return XZ, nil
	}

	return Store, nil
}
