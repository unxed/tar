package tar

import (
	"archive/tar"
	"bufio"
	"io"
	"errors"
	"os"
	"sync"
	"unsafe"
)

type ReadCloser struct {
	*tar.Reader
	f         io.Closer
	dcomp     io.ReadCloser
	rawReader io.Reader
}

// OpenReader opens a TAR or compressed TAR file (.tar, .tar.gz, .tar.zst, etc.).
func OpenReader(name string) (*ReadCloser, error) {
	return openReaderWithPassword(name, "")
}

func openReaderWithPassword(name string, password string) (*ReadCloser, error) {
	mvr, size, err := OpenMultiVolume(name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	ra, size, err := checkF4Recovery(mvr, size)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	raDec, sizeDec, err := checkXCrypt(ra, size, password)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	method, err := DetectFormat(raDec)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	var rd io.Reader = io.NewSectionReader(raDec, 0, sizeDec)
	var dcomp io.ReadCloser

	if method != Store {
		di, ok := decompressors.Load(method)
		if !ok {
			mvr.Close()
			return nil, ErrAlgorithm
		}

		dcomp, err = di.(Decompressor).Decompress(rd)
		if err != nil {
			mvr.Close()
			return nil, err
		}
		if method == GZIP {
			rd = makeBufferedPipelineReader(dcomp)
		} else {
			rd = bufio.NewReaderSize(dcomp, 1024*1024)
		}
	}

	return &ReadCloser{
		Reader:    tar.NewReader(rd),
		f:         mvr,
		dcomp:     dcomp,
		rawReader: rd,
	}, nil
}

type bufferedPipe struct {
	ch      chan []byte
	errCh   chan error
	current []byte
	r       io.ReadCloser
	err     error
}

var chunkPool = sync.Pool{
	New: func() any {
		return new(buffer64K)
	},
}

func makeBufferedPipelineReader(r io.ReadCloser) io.ReadCloser {
	ch := make(chan []byte, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		for {
			bufPtr := chunkPool.Get().(*buffer64K)
			n, err := io.ReadFull(r, bufPtr[:])
			if n > 0 {
				ch <- bufPtr[:n]
			} else {
				chunkPool.Put(bufPtr)
			}
			if err != nil {
				r.Close()
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					errCh <- err
				} else {
					errCh <- io.EOF
				}
				return
			}
		}
	}()

	return &bufferedPipe{
		ch:    ch,
		errCh: errCh,
		r:     r,
	}
}

func (bp *bufferedPipe) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	if len(bp.current) == 0 {
		if bp.err != nil {
			return 0, bp.err
		}
		var ok bool
		bp.current, ok = <-bp.ch
		if !ok {
			select {
			case err := <-bp.errCh:
				bp.err = err
			default:
				bp.err = io.EOF
			}
			return 0, bp.err
		}
	}

	n := copy(b, bp.current)
	leftover := bp.current[n:]
	if len(leftover) == 0 {
		if cap(bp.current) == 65536 {
			fullSlice := bp.current[:65536]
			ptr := (*buffer64K)(unsafe.Pointer(&fullSlice[0]))
			chunkPool.Put(ptr)
		}
		bp.current = nil
	} else {
		bp.current = leftover
	}

	return n, nil
}

func (bp *bufferedPipe) Close() error {
	bp.r.Close()
	for b := range bp.ch {
		if cap(b) == 65536 {
			fullSlice := b[:65536]
			ptr := (*buffer64K)(unsafe.Pointer(&fullSlice[0]))
			chunkPool.Put(ptr)
		}
	}
	return nil
}

func (rc *ReadCloser) Next() (*Header, error) {
	hdr, err := rc.Reader.Next()
	if err == io.EOF && rc.rawReader != nil {
		newReader := tar.NewReader(rc.rawReader)
		newHdr, newErr := newReader.Next()
		if newErr == nil {
			rc.Reader = newReader
			return newHdr, nil
		}
	}
	return hdr, err
}

func (rc *ReadCloser) Close() error {
	var err1, err2 error
	if rc.dcomp != nil {
		err1 = rc.dcomp.Close()
	}
	if rc.f != nil {
		err2 = rc.f.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}

var ErrAlgorithm = errors.New("tar: unsupported compression algorithm")