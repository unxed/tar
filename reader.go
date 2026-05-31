package tar

import (
	"archive/tar"
	"io"
    "errors"
)

type ReadCloser struct {
	*tar.Reader
	f     io.Closer
	dcomp io.ReadCloser
}

// OpenReader opens a TAR or compressed TAR file (.tar, .tar.gz, .tar.zst, etc.).
func OpenReader(name string) (*ReadCloser, error) {
	ra, size, closer, err := openMultiVolume(name)
	if err != nil {
		return nil, err
	}

	method, err := DetectFormat(ra)
	if err != nil {
		closer.Close()
		return nil, err
	}

	var rd io.Reader = io.NewSectionReader(ra, 0, size)
	var dcomp io.ReadCloser

	if method != Store {
		di, ok := decompressors.Load(method)
		if !ok {
			closer.Close()
			return nil, ErrAlgorithm
		}

		dcomp, err = di.(Decompressor).Decompress(rd)
		if err != nil {
			closer.Close()
			return nil, err
		}
		rd = dcomp
	}

	return &ReadCloser{
		Reader: tar.NewReader(rd),
		f:      closer,
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