package tar

import (
	"archive/tar"
    "bufio"
	"io"
	"os"
	"github.com/ulikunitz/xz"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

type WriteCloser struct {
	*tar.Writer
	f             io.WriteCloser
	comp          io.WriteCloser
	method        uint16
	idx           *Index
	uncompTracker *trackingWriter
	compTracker   *trackingWriter
	lastFlushPos  int64
	batch         []FileNode
	seenParents   map[string]bool
	gzPoints      []gzPoint
	zstdBlocks    []BlockOffset
	splitSize     int64
}

// CreateWriter creates a new TAR or compressed TAR file.
// Method should be one of Store, GZIP, BZIP2, XZ, ZSTD.
type writerOptions struct {
	indexPath string
	splitSize int64
	level     int
}

func WithWriterLevel(level int) WriterOption {
	return func(o *writerOptions) {
		o.level = level
	}
}

type WriterOption func(*writerOptions)

// WithWriterIndex enables on-the-fly indexing during archive creation.
func WithWriterIndex(indexPath string) WriterOption {
	return func(o *writerOptions) {
		o.indexPath = indexPath
	}
}

func WithWriterSplitSize(size int64) WriterOption {
	return func(o *writerOptions) {
		o.splitSize = size
	}
}

type stdoutWrapper struct{ *os.File }

func (stdoutWrapper) Close() error { return nil }

func CreateWriter(name string, method uint16, opts ...WriterOption) (*WriteCloser, error) {
	var wopts writerOptions
	for _, o := range opts {
		o(&wopts)
	}

	var f io.WriteCloser
	var err error
	if wopts.splitSize > 0 {
		f, err = NewMultiVolumeWriter(name, wopts.splitSize)
	} else if name == "-" {
		f = stdoutWrapper{os.Stdout}
	} else {
		f, err = os.Create(name)
	}
	if err != nil {
		return nil, err
	}

	// Буферизируем вывод на уровне файла. 2МБ — оптимально для Windows/NTFS.
	bufOut := bufio.NewWriterSize(f, 2*1024*1024)
	compTracker := &trackingWriter{w: bufOut}
	var wr io.Writer = compTracker
	var comp io.WriteCloser

	if method != Store {
		if wopts.level != 0 {
			if method == GZIP {
				comp, err = gzip.NewWriterLevel(wr, wopts.level)
			} else if method == ZSTD {
				comp, err = zstd.NewWriter(wr, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(wopts.level)))
			} else if method == XZ {
				config := xz.WriterConfig{
					CheckSum: xz.CRC64,
				}
				// XZ levels are roughly mapped to internal presets
				if err = config.Verify(); err == nil {
					comp, err = config.NewWriter(wr)
				}
			}
		}
		if comp == nil && err == nil {
			ci, ok := compressors.Load(method)
			if !ok {
				f.Close()
				return nil, ErrAlgorithm
			}
			comp, err = ci.(Compressor)(wr)
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		wr = comp
	}

	uncompTracker := &trackingWriter{w: wr}

	wc := &WriteCloser{
		Writer:        tar.NewWriter(uncompTracker),
		f:             f,
		comp:          comp,
		method:        method,
		uncompTracker: uncompTracker,
		compTracker:   compTracker,
		seenParents:   make(map[string]bool),
	}

	if wopts.indexPath != "" {
		os.Remove(wopts.indexPath)
		idx, err := OpenIndex(wopts.indexPath)
		if err == nil {
			wc.idx = idx
			wc.idx.InitMetadata()
		}
	}

	if wc.idx != nil && method == GZIP {
		wc.gzPoints = append(wc.gzPoints, gzPoint{compOffset: 0, uncompOffset: 0, bits: 0, hasData: 0})
	}
	if wc.idx != nil && method == ZSTD {
		wc.zstdBlocks = append(wc.zstdBlocks, BlockOffset{BlockOffset: 0, DataOffset: 0})
	}

	return wc, nil
}

func (wc *WriteCloser) WriteHeader(hdr *Header) error {
	if wc.idx == nil {
		return wc.Writer.WriteHeader(hdr)
	}

	headerOffset := wc.uncompTracker.pos
	err := wc.Writer.WriteHeader(hdr)
	if err != nil {
		return err
	}
	dataOffset := wc.uncompTracker.pos

	insertParentFolders(hdr.Name, &wc.batch, wc.seenParents)
	dir, name := normalizePath(hdr.Name)

	wc.batch = append(wc.batch, FileNode{
		Path:         dir,
		Name:         name,
		OffsetHeader: headerOffset,
		Offset:       dataOffset,
		Size:         hdr.Size,
		Mode:         int64(hdr.Mode),
		ModTime:      hdr.ModTime,
		Type:         hdr.Typeflag,
		LinkName:     hdr.Linkname,
		Uid:          hdr.Uid,
		Gid:          hdr.Gid,
	})

	if len(wc.batch) >= 1000 {
		wc.idx.Insert(wc.batch)
		wc.batch = wc.batch[:0]
	}

	return nil
}

func (wc *WriteCloser) Write(p []byte) (int, error) {
	n, err := wc.Writer.Write(p)
	if err != nil || wc.idx == nil || wc.method == Store {
		return n, err
	}

	// Periodic Flush (every 4MB) to create seek points
	const flushThreshold = 4 * 1024 * 1024
	if wc.uncompTracker.pos-wc.lastFlushPos >= flushThreshold {
		if wc.method == ZSTD || wc.method == GZIP {
			wc.createSeekPoint()
		}
	}

	return n, err
}

func (wc *WriteCloser) createSeekPoint() {
	if wc.method == ZSTD || wc.method == GZIP || wc.method == XZ {
		// We close the current member/frame and start a new one.
		// This ensures the next byte in the output stream is a valid frame header
		// (GZIP magic or ZSTD magic), allowing O(1) random access seeking
		// without needing to recover the compression state (sliding window/dictionary).
		wc.comp.Close()

		if wc.method == ZSTD {
			wc.zstdBlocks = append(wc.zstdBlocks, BlockOffset{
				BlockOffset: wc.compTracker.pos,
				DataOffset:  wc.uncompTracker.pos,
			})
		} else {
			wc.gzPoints = append(wc.gzPoints, gzPoint{
				compOffset:   uint64(wc.compTracker.pos),
				uncompOffset: uint64(wc.uncompTracker.pos),
				bits:         0,
				hasData:      0,
			})
		}

		ci, _ := compressors.Load(wc.method)
		newComp, err := ci.(Compressor)(wc.compTracker)
		if err == nil {
			wc.comp = newComp
			// Update the target of our tracking wrapper so the tar.Writer
			// now pumps data into the new compression member.
			wc.uncompTracker.w = newComp
			wc.lastFlushPos = wc.uncompTracker.pos
		}
	}
}

func (wc *WriteCloser) Close() error {
	var err1, err2, err3 error
	err1 = wc.Writer.Close()

	if wc.idx != nil {
		if len(wc.batch) > 0 {
			wc.idx.Insert(wc.batch)
		}
		if wc.method == ZSTD && len(wc.zstdBlocks) > 0 {
			wc.idx.InsertBlockOffsets("zstdblocks", wc.zstdBlocks)
		}
		if wc.method == GZIP && len(wc.gzPoints) > 0 {
			gzidx := &gzipIndexTrackingReader{
				points:       wc.gzPoints,
				uncompOffset: wc.uncompTracker.pos,
				spacing:      4 * 1024 * 1024,
				tr:           &trackingByteReader{pos: wc.compTracker.pos},
			}
			if data, err := gzidx.ExportGzipIndex(); err == nil {
				wc.idx.SaveGzipIndex(data)
			}
		}
		wc.idx.Close()
	}

	if wc.comp != nil {
		err2 = wc.comp.Close()
	}

	// Сбрасываем буфер, чтобы все данные гарантированно ушли в `wc.f`
	if bw, ok := wc.compTracker.w.(*bufio.Writer); ok {
		if err := bw.Flush(); err != nil && err3 == nil {
			err3 = err
		}
	}

	if wc.f != nil {
		if err := wc.f.Close(); err != nil && err3 == nil {
			err3 = err
		}
	}
	if err1 != nil { return err1 }
	if err2 != nil { return err2 }
	return err3
}