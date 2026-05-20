package tar

import (
	"archive/tar"
	"io"
	"os"
    "errors"
)

type ReadCloser struct {
	*tar.Reader
	f     *os.File
	dcomp io.ReadCloser
}

// OpenReader opens a TAR or compressed TAR file (.tar, .tar.gz, .tar.zst, etc.).
func OpenReader(name string) (*ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}

	method, err := DetectFormat(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	var rd io.Reader = f
	var dcomp io.ReadCloser

	if method != Store {
		di, ok := decompressors.Load(method)
		if !ok {
			f.Close()
			return nil, ErrAlgorithm
		}

		f.Seek(0, io.SeekStart)
		dcomp, err = di.(Decompressor).Decompress(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		rd = dcomp
	} else {
		f.Seek(0, io.SeekStart)
	}

	return &ReadCloser{
		Reader: tar.NewReader(rd),
		f:      f,
		dcomp:  dcomp,
	}, nil
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