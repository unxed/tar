package tar

import (
	"archive/tar"
	"io"
	"os"
)

type WriteCloser struct {
	*tar.Writer
	f    *os.File
	comp io.WriteCloser
}

// CreateWriter creates a new TAR or compressed TAR file.
// Method should be one of Store, GZIP, BZIP2, XZ, ZSTD.
func CreateWriter(name string, method uint16) (*WriteCloser, error) {
	f, err := os.Create(name)
	if err != nil {
		return nil, err
	}

	var wr io.Writer = f
	var comp io.WriteCloser

	if method != Store {
		ci, ok := compressors.Load(method)
		if !ok {
			f.Close()
			return nil, ErrAlgorithm
		}
		comp, err = ci.(Compressor)(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		wr = comp
	}

	return &WriteCloser{
		Writer: tar.NewWriter(wr),
		f:      f,
		comp:   comp,
	}, nil
}

func (wc *WriteCloser) Close() error {
	var err1, err2, err3 error
	err1 = wc.Writer.Close() // Flushes tar EOF blocks
	if wc.comp != nil {
		err2 = wc.comp.Close() // Flushes compression frame
	}
	if wc.f != nil {
		err3 = wc.f.Close()
	}
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err3
}