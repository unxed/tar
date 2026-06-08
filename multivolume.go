package tar

import (
	"fmt"
	"io"
	"os"
)

type MultiVolumeReader struct {
	files   []*os.File
	offsets []int64
	size    int64
}

func (m *MultiVolumeReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= m.size { return 0, io.EOF }
	for i := range m.files {
		fileStart := m.offsets[i]
		fileEnd := m.size
		if i+1 < len(m.offsets) { fileEnd = m.offsets[i+1] }
		if off >= fileStart && off < fileEnd {
			relOff := off - fileStart
			canRead := fileEnd - off
			toRead := int64(len(p))
			if toRead > canRead { toRead = canRead }
			nPart, err := m.files[i].ReadAt(p[:toRead], relOff)
			n += nPart
			if err != nil && err != io.EOF { return n, err }
			if n < len(p) && nPart == int(toRead) {
				nextN, nextErr := m.ReadAt(p[n:], off+int64(nPart))
				return n + nextN, nextErr
			}
			return n, err
		}
	}
	return 0, io.EOF
}

func (m *MultiVolumeReader) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= m.size { return 0, fmt.Errorf("write out of bounds") }
	for i := range m.files {
		fileStart := m.offsets[i]
		fileEnd := m.size
		if i+1 < len(m.offsets) { fileEnd = m.offsets[i+1] }
		if off >= fileStart && off < fileEnd {
			relOff := off - fileStart
			canWrite := fileEnd - off
			toWrite := int64(len(p))
			if toWrite > canWrite { toWrite = canWrite }
			nPart, err := m.files[i].WriteAt(p[:toWrite], relOff)
			n += nPart
			if err != nil { return n, err }
			if n < len(p) && nPart == int(toWrite) {
				nextN, nextErr := m.WriteAt(p[n:], off+int64(nPart))
				return n + nextN, nextErr
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("write out of bounds")
}

func (m *MultiVolumeReader) Append(data []byte) error {
	lastFile := m.files[len(m.files)-1]
	if _, err := lastFile.Seek(0, io.SeekEnd); err != nil { return err }
	n, err := lastFile.Write(data)
	if err == nil { m.size += int64(n) }
	return err
}

func (m *MultiVolumeReader) Close() error {
	var lastErr error
	for _, f := range m.files {
		if err := f.Close(); err != nil { lastErr = err }
	}
	return lastErr
}

func OpenMultiVolume(mainPath string, flag int) (*MultiVolumeReader, int64, error) {
	var files []*os.File
	var offsets []int64
	var totalSize int64

	for i := 1; ; i++ {
		volPath := fmt.Sprintf("%s.%03d", mainPath, i)
		f, err := os.OpenFile(volPath, flag, 0644)
		if err != nil { break }
		fi, _ := f.Stat()
		offsets = append(offsets, totalSize)
		totalSize += fi.Size()
		files = append(files, f)
	}

	if len(files) > 0 {
		m := &MultiVolumeReader{files: files, offsets: offsets, size: totalSize}
		return m, totalSize, nil
	}

	fMain, err := os.OpenFile(mainPath, flag, 0644)
	if err != nil { return nil, 0, err }
	fiMain, _ := fMain.Stat()
	m := &MultiVolumeReader{files: []*os.File{fMain}, offsets: []int64{0}, size: fiMain.Size()}
	return m, fiMain.Size(), nil
}

type MultiVolumeWriter struct {
	mainPath    string
	splitSize   int64
	currentFile *os.File
	volumeIndex int
	written     int64
}

func NewMultiVolumeWriter(mainPath string, splitSize int64) (*MultiVolumeWriter, error) {
	m := &MultiVolumeWriter{mainPath: mainPath, splitSize: splitSize}
	if err := m.openNextVolume(); err != nil { return nil, err }
	return m, nil
}

func (m *MultiVolumeWriter) openNextVolume() error {
	if m.currentFile != nil {
		if err := m.currentFile.Close(); err != nil { return err }
	}
	m.volumeIndex++
	volPath := fmt.Sprintf("%s.%03d", m.mainPath, m.volumeIndex)
	f, err := os.Create(volPath)
	if err != nil { return err }
	m.currentFile = f
	m.written = 0
	return nil
}

func (m *MultiVolumeWriter) Write(p []byte) (n int, err error) {
	total := 0
	for len(p) > 0 {
		room := m.splitSize - m.written
		if room <= 0 {
			if err := m.openNextVolume(); err != nil { return total, err }
			room = m.splitSize
		}
		chunk := int64(len(p))
		if chunk > room { chunk = room }
		wn, err := m.currentFile.Write(p[:chunk])
		total += wn
		m.written += int64(wn)
		if err != nil { return total, err }
		p = p[chunk:]
	}
	return total, nil
}

func (m *MultiVolumeWriter) Close() error {
	if m.currentFile == nil { return nil }
	err := m.currentFile.Close()
	m.currentFile = nil
	return err
}

func (m *MultiVolumeWriter) Sync() error {
	if m.currentFile != nil { return m.currentFile.Sync() }
	return nil
}

func (m *MultiVolumeWriter) Name() string {
	return m.mainPath
}