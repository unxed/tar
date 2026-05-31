package tar

import (
	"fmt"
	"io"
	"os"
)

type multiVolumeReader struct {
	files   []*os.File
	offsets []int64
	size    int64
}

func (m *multiVolumeReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= m.size {
		return 0, io.EOF
	}
	for i := range m.files {
		fileStart := m.offsets[i]
		fileEnd := m.size
		if i+1 < len(m.offsets) {
			fileEnd = m.offsets[i+1]
		}
		if off >= fileStart && off < fileEnd {
			relOff := off - fileStart
			canRead := fileEnd - off
			toRead := int64(len(p))
			if toRead > canRead {
				toRead = canRead
			}
			nPart, err := m.files[i].ReadAt(p[:toRead], relOff)
			n += nPart
			if err != nil && err != io.EOF {
				return n, err
			}
			if n < len(p) && nPart == int(toRead) {
				nextN, nextErr := m.ReadAt(p[n:], off+int64(nPart))
				return n + nextN, nextErr
			}
			return n, err
		}
	}
	return 0, io.EOF
}

func (m *multiVolumeReader) Close() error {
	var lastErr error
	for _, f := range m.files {
		if err := f.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func openMultiVolume(mainPath string) (io.ReaderAt, int64, io.Closer, error) {
	var files []*os.File
	var offsets []int64
	var totalSize int64

	for i := 1; ; i++ {
		volPath := fmt.Sprintf("%s.%03d", mainPath, i)
		f, err := os.Open(volPath)
		if err != nil {
			break
		}
		fi, _ := f.Stat()
		offsets = append(offsets, totalSize)
		totalSize += fi.Size()
		files = append(files, f)
	}

	if len(files) > 0 {
		m := &multiVolumeReader{
			files:   files,
			offsets: offsets,
			size:    totalSize,
		}
		return m, totalSize, m, nil
	}

	fMain, err := os.Open(mainPath)
	if err != nil {
		return nil, 0, nil, err
	}
	fiMain, _ := fMain.Stat()
	return fMain, fiMain.Size(), fMain, nil
}