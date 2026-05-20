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
// Appending to a compressed TAR (.tar.gz, etc) is complex because it requires
// either concatenating a new compression stream (which some readers don't support well)
// or fully rewriting the archive. Ratarmount supports concatenated streams, but standard
// tools behave unpredictably. So we strictly support uncompressed tar here for now.
func NewUpdater(f *os.File, mode AppendMode) (*Updater, error) {
	if mode != APPEND_MODE_OVERWRITE {
		return nil, errors.New("tar: only APPEND_MODE_OVERWRITE is supported")
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Seek backwards to find the standard tar EOF markers (two 512-byte blocks of zeros).
	// We search in the last 10KB.
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

	// Find where the sequence of trailing zeros begins.
	// A valid tar ends with 1024 zero bytes, but there might be padding.
	zeroBlocksStart := -1
	for i := len(buf) - 512; i >= 0; i -= 512 {
		if bytes.Equal(buf[i:i+512], make([]byte, 512)) {
			zeroBlocksStart = i
		} else {
			break
		}
	}

	if zeroBlocksStart != -1 {
		// Truncate the file at the start of the zero blocks so we can append there.
		truncateTo := stat.Size() - searchSize + int64(zeroBlocksStart)
		if err := f.Truncate(truncateTo); err != nil {
			return nil, err
		}
		if _, err := f.Seek(truncateTo, io.SeekStart); err != nil {
			return nil, err
		}
	} else {
		// No zero blocks found, just seek to end
		f.Seek(0, io.SeekEnd)
	}

	return &Updater{
		f:  f,
		tw: tar.NewWriter(f),
	}, nil
}

// Append creates a new file entry in the archive.
func (u *Updater) Append(name string, size int64, data []byte) error {
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