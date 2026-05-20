package tar

import (
	"bytes"
	"compress/bzip2"
	"io"
	"runtime"
	"sync"

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
type Decompressor func(r io.Reader) (io.ReadCloser, error)

var (
	compressors   sync.Map
	decompressors sync.Map
)

func init() {
	// GZIP
	compressors.Store(GZIP, Compressor(func(w io.Writer) (io.WriteCloser, error) {
		return gzip.NewWriter(w), nil
	}))
	decompressors.Store(GZIP, Decompressor(func(r io.Reader) (io.ReadCloser, error) {
		return gzip.NewReader(r)
	}))

	// BZIP2
	decompressors.Store(BZIP2, Decompressor(func(r io.Reader) (io.ReadCloser, error) {
		return io.NopCloser(bzip2.NewReader(r)), nil
	}))

	// XZ
	compressors.Store(XZ, Compressor(func(w io.Writer) (io.WriteCloser, error) {
		return xz.NewWriter(w)
	}))
	decompressors.Store(XZ, Decompressor(func(r io.Reader) (io.ReadCloser, error) {
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(xr), nil
	}))

	// ZSTD
	compressors.Store(ZSTD, Compressor(func(w io.Writer) (io.WriteCloser, error) {
		return zstd.NewWriter(w, zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	}))
	decompressors.Store(ZSTD, Decompressor(func(r io.Reader) (io.ReadCloser, error) {
		dec, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return dec.IOReadCloser(), nil
	}))
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