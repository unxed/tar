package tar

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	stdgzip "compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
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
	return newGzipIndexTrackingReader(r)
}

type trackingByteReader struct {
	r   io.Reader
	pos int64
}

func (t *trackingByteReader) Read(p []byte) (n int, err error) {
	n, err = t.r.Read(p)
	t.pos += int64(n)
	return n, err
}

func (t *trackingByteReader) ReadByte() (byte, error) {
	var buf [1]byte
	n, err := t.Read(buf[:])
	if n == 1 {
		return buf[0], nil
	}
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}

type gzipIndexTrackingReader struct {
	r              io.ReadCloser
	trackReader    *trackingByteReader
	uncompOffset   int64
	spacing        int64
	points         []gzPoint
	lastCheckpoint int64
}

func newGzipIndexTrackingReader(r io.Reader) (*gzipIndexTrackingReader, error) {
	tr := &trackingByteReader{r: r}
	gz, err := stdgzip.NewReader(tr)
	if err != nil {
		return nil, err
	}
	gtr := &gzipIndexTrackingReader{
		r:            gz,
		trackReader:  tr,
		spacing:      1024 * 1024, // 1MB standard spacing
		points: []gzPoint{
			{
				compOffset:   0,
				uncompOffset: 0,
				bits:         0,
				hasData:      0,
			},
		},
	}
	return gtr, nil
}

func (g *gzipIndexTrackingReader) Read(p []byte) (n int, err error) {
	for i := 0; i < len(p); i++ {
		var buf [1]byte
		n1, err1 := g.r.Read(buf[:])
		if n1 > 0 {
			p[i] = buf[0]
			n++
			g.uncompOffset++

			if g.uncompOffset-g.lastCheckpoint >= g.spacing {
				if g.isAtBlockBoundary() {
					g.captureCheckpoint()
				}
			}
		}
		if err1 != nil {
			return n, err1
		}
	}
	return n, nil
}

var boundaryPCs sync.Map // Map of uintptr -> bool

func (g *gzipIndexTrackingReader) isAtBlockBoundary() bool {
	defer func() {
		_ = recover()
	}()

	stdGzReaderVal := reflect.ValueOf(g.r).Elem()
	decompressorField := stdGzReaderVal.FieldByName("decompressor")
	if !decompressorField.IsValid() {
		return false
	}
	decompressorVal := reflect.NewAt(decompressorField.Type(), unsafe.Pointer(decompressorField.UnsafeAddr())).Elem()
	if decompressorVal.IsNil() {
		return false
	}

	flateDecompressorPtr := decompressorVal.Elem()
	if flateDecompressorPtr.Kind() != reflect.Ptr {
		return false
	}
	flateDecompressorVal := flateDecompressorPtr.Elem()

	stepField := flateDecompressorVal.FieldByName("step")
	if stepField.IsValid() {
		pc := stepField.Pointer()
		if pc == 0 {
			return false
		}

		// O(1) Fast path: check cached PC values
		if val, ok := boundaryPCs.Load(pc); ok {
			return val.(bool)
		}

		// O(N) Slow path: look up name once and cache it
		fn := runtime.FuncForPC(pc)
		if fn != nil {
			name := fn.Name()
			isBoundary := strings.Contains(name, "readBlockHeader") || strings.Contains(name, "nextBlock")
			boundaryPCs.Store(pc, isBoundary)
			return isBoundary
		}
	}
	return false
}

func (g *gzipIndexTrackingReader) Close() error {
	return g.r.Close()
}

func (g *gzipIndexTrackingReader) captureCheckpoint() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[GZIDX] Panic in captureCheckpoint: %v\n", r)
		}
	}()

	stdGzReaderVal := reflect.ValueOf(g.r).Elem()
	decompressorField := stdGzReaderVal.FieldByName("decompressor")
	if !decompressorField.IsValid() {
		return
	}
	decompressorVal := reflect.NewAt(decompressorField.Type(), unsafe.Pointer(decompressorField.UnsafeAddr())).Elem()
	if decompressorVal.IsNil() {
		return
	}

	flateDecompressorPtr := decompressorVal.Elem()
	if flateDecompressorPtr.Kind() != reflect.Ptr {
		return
	}
	flateDecompressorVal := flateDecompressorPtr.Elem()

	nbField := flateDecompressorVal.FieldByName("nb")
	if !nbField.IsValid() {
		return
	}
	nbVal := reflect.NewAt(nbField.Type(), unsafe.Pointer(nbField.UnsafeAddr())).Elem()
	nb := nbVal.Interface().(uint)

	// Get buffered bytes from bufio.Reader
	var buffered int
	rBufField := flateDecompressorVal.FieldByName("rBuf")
	if rBufField.IsValid() {
		rBufVal := reflect.NewAt(rBufField.Type(), unsafe.Pointer(rBufField.UnsafeAddr())).Elem()
		if !rBufVal.IsNil() {
			bufReader := rBufVal.Interface().(*bufio.Reader)
			buffered = bufReader.Buffered()
		}
	}

	totalBits := (g.trackReader.pos - int64(buffered))*8 - int64(nb)
	compOffset := uint64(totalBits / 8)
	bits := uint8(totalBits % 8)

	dictField := flateDecompressorVal.FieldByName("dict")
	if !dictField.IsValid() {
		return
	}

	histField := dictField.FieldByName("hist")
	if !histField.IsValid() {
		return
	}
	histVal := reflect.NewAt(histField.Type(), unsafe.Pointer(histField.UnsafeAddr())).Elem()
	histBytes := histVal.Interface().([]byte)

	wrField := dictField.FieldByName("wrPos")
	if !wrField.IsValid() {
		return
	}
	wrVal := reflect.NewAt(wrField.Type(), unsafe.Pointer(wrField.UnsafeAddr())).Elem()
	wr := wrVal.Interface().(int)

	window := make([]byte, 32768)
	if len(histBytes) == 32768 {
		copy(window, histBytes[wr:])
		copy(window[32768-wr:], histBytes[:wr])
	}

	g.points = append(g.points, gzPoint{
		compOffset:   compOffset,
		uncompOffset: uint64(g.uncompOffset),
		bits:         bits,
		hasData:      1,
		window:       window,
	})
	g.lastCheckpoint = g.uncompOffset
}

func (g *gzipIndexTrackingReader) ExportGzipIndex() ([]byte, error) {
	compSize := g.trackReader.pos
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
	gw := stdgzip.NewWriter(&cmpBuf)
	_, err := gw.Write(gzidxBuf.Bytes())
	if err != nil {
		return nil, err
	}
	gw.Close()

	return cmpBuf.Bytes(), nil
}

type bitShiftingReader struct {
	r     io.Reader
	shift uint8
	prev  byte
}

func (b *bitShiftingReader) Read(p []byte) (n int, err error) {
	if b.shift == 0 {
		return b.r.Read(p)
	}

	tmp := make([]byte, len(p))
	tn, terr := b.r.Read(tmp)
	if tn == 0 {
		return 0, terr
	}

	for i := 0; i < tn; i++ {
		curr := tmp[i]
		p[i] = (b.prev >> b.shift) | (curr << (8 - b.shift))
		b.prev = curr
	}
	return tn, terr
}

type gzPoint struct {
	compOffset   uint64
	uncompOffset uint64
	bits         uint8
	hasData      uint8
	window       []byte
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
	if best.bits > 0 {
		seekOffset -= 1
	}

	if best.hasData == 0 {
		sr := io.NewSectionReader(r, seekOffset, 1<<63-1)
		gr, err := gzip.NewReader(sr)
		if err != nil {
			return nil, 0, err
		}
		return gr, int64(best.uncompOffset), nil
	}

	sr := io.NewSectionReader(r, seekOffset, 1<<63-1)
	var reader io.Reader = sr

	if best.bits > 0 {
		var ch [1]byte
		if _, err := sr.Read(ch[:]); err != nil {
			return nil, 0, err
		}
		reader = &bitShiftingReader{
			r:     sr,
			shift: best.bits,
			prev:  ch[0],
		}
	}

	fr := flate.NewReaderDict(reader, best.window)
	return fr, int64(best.uncompOffset), nil
}

// -- BZIP2 --
type bzip2Format struct{}
func (bzip2Format) Decompress(r io.Reader) (io.ReadCloser, error) { return io.NopCloser(bzip2.NewReader(r)), nil }

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
func (zstdFormat) Decompress(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	return dec.IOReadCloser(), nil
}

// ZSTD frames are byte-aligned and independent. We can natively resume from any frame!
func (zstdFormat) ResumeFromBlockOffset(r io.ReaderAt, bo *BlockOffset) (io.ReadCloser, error) {
	sr := io.NewSectionReader(r, bo.BlockOffset, 1<<63-1)
	dec, err := zstd.NewReader(sr)
	if err != nil {
		return nil, err
	}
	return dec.IOReadCloser(), nil
}

func init() {
	compressors.Store(GZIP, Compressor(func(w io.Writer) (io.WriteCloser, error) { return gzip.NewWriter(w), nil }))
	decompressors.Store(GZIP, gzipFormat{})

	decompressors.Store(BZIP2, bzip2Format{})

	compressors.Store(XZ, Compressor(func(w io.Writer) (io.WriteCloser, error) { return xz.NewWriter(w) }))
	decompressors.Store(XZ, xzFormat{})

	compressors.Store(ZSTD, Compressor(func(w io.Writer) (io.WriteCloser, error) {
		return zstd.NewWriter(w, zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	}))
	decompressors.Store(ZSTD, zstdFormat{})
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