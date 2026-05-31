package tar

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
)

type AppendMode int

const (
	APPEND_MODE_OVERWRITE AppendMode = iota
)

type Updater struct {
	f  *os.File
	tw *tar.Writer
}

// NewUpdater opens an UNCOMPRESSED .tar file for appending.
func NewUpdater(f *os.File, mode AppendMode) (*Updater, error) {
	if mode != APPEND_MODE_OVERWRITE {
		return nil, errors.New("tar: only APPEND_MODE_OVERWRITE is supported")
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	searchSize := int64(10240)
	if stat.Size() < searchSize {
		searchSize = stat.Size()
	}

	if searchSize == 0 {
		return &Updater{f: f, tw: tar.NewWriter(f)}, nil
	}

	buf := make([]byte, searchSize)
	_, err = f.ReadAt(buf, stat.Size()-searchSize)
	if err != nil && err != io.EOF {
		return nil, err
	}

	zeroBlocksStart := -1
	for i := len(buf) - 512; i >= 0; i -= 512 {
		if bytes.Equal(buf[i:i+512], make([]byte, 512)) {
			zeroBlocksStart = i
		} else {
			break
		}
	}

	if zeroBlocksStart != -1 {
		truncateTo := stat.Size() - searchSize + int64(zeroBlocksStart)
		if err := f.Truncate(truncateTo); err != nil {
			return nil, err
		}
		if _, err := f.Seek(truncateTo, io.SeekStart); err != nil {
			return nil, err
		}
	} else {
		f.Seek(0, io.SeekEnd)
	}

	return &Updater{
		f:  f,
		tw: tar.NewWriter(f),
	}, nil
}

// Append creates a new file entry in the archive.
func (u *Updater) Append(name string, size int64, data []byte) error {
	if _, err := u.f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	stat, err := u.f.Stat()
	if err != nil {
		return err
	}
	endOfArchive := stat.Size()

	tr := &trackingReader{r: u.f}
	trd := NewReader(tr)

	var targetStart int64 = -1
	var targetEnd int64 = -1

	for {
		headerOffset := tr.pos
		hdr, err := trd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		if hdr.Name == name {
			targetStart = headerOffset
			nextHeaderOffset := tr.pos
			_, nextErr := trd.Next()
			if nextErr == io.EOF {
				targetEnd = endOfArchive
			} else if nextErr == nil {
				targetEnd = nextHeaderOffset
			} else {
				targetEnd = endOfArchive
			}
			break
		}
	}

	if targetStart != -1 && targetEnd != -1 {
		removeSize := targetEnd - targetStart
		if targetEnd < endOfArchive {
			const chunkBufSize = 32 * 1024
			buffer := make([]byte, chunkBufSize)
			rp := targetEnd
			wp := targetStart
			for rp < endOfArchive {
				n, err := u.f.ReadAt(buffer, rp)
				if err != nil && err != io.EOF {
					return err
				}
				if n == 0 {
					break
				}
				_, err = u.f.WriteAt(buffer[:n], wp)
				if err != nil {
					return err
				}
				rp += int64(n)
				wp += int64(n)
			}
		}
		newSize := endOfArchive - removeSize
		if err := u.f.Truncate(newSize); err != nil {
			return err
		}
		if _, err := u.f.Seek(newSize, io.SeekStart); err != nil {
			return err
		}
		u.tw = NewWriter(u.f)
	} else {
		if _, err := u.f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		u.tw = NewWriter(u.f)
	}

	hdr := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: size,
	}
	if err := u.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := u.tw.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func (u *Updater) Close() error {
	return u.tw.Close()
}