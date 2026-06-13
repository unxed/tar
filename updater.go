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

var ErrArchiveLocked = errors.New("tar: cannot modify archive, it is locked")

type Updater struct {
	f           *os.File
	tw          *tar.Writer
	isCompressed bool
	compMethod  uint16
	shadowStart int64
}

// NewUpdater opens a .tar or compressed .tar file for appending.
func NewUpdater(f *os.File, mode AppendMode) (*Updater, error) {
	if mode != APPEND_MODE_OVERWRITE {
		return nil, errors.New("tar: only APPEND_MODE_OVERWRITE is supported")
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	method, err := DetectFormat(f)
	if err != nil {
		return nil, err
	}

	// Detect if it is a compressed F4SS archive with shadow streams
	shadowStart, shadowSize, err := LocateShadowStream(f, stat.Size(), method)
	isCompressed := method != Store

	var truncateTo int64
	if isCompressed && shadowStart > 0 && shadowSize > 0 {
		// Truncate to the start of Stream 2 (shadow stream), removing old metadata and magic footer
		truncateTo = shadowStart
	} else {
		// Fallback for uncompressed standard TAR
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
			truncateTo = stat.Size() - searchSize + int64(zeroBlocksStart)
		} else {
			truncateTo = stat.Size()
		}
	}

	// Check if the archive is locked in the index (F4SS) before doing any modifications
	if shadowStart > 0 && shadowSize > 0 {
		propBytes, err := extractShadowFile(f, stat.Size(), method, ".tarext/f4/properties.txt")
		if err == nil && len(propBytes) > 0 {
			props := parseProperties(propBytes)
			if props["locked"] == "true" {
				return nil, ErrArchiveLocked
			}
		}
	}

	if err := f.Truncate(truncateTo); err != nil {
		return nil, err
	}
	if _, err := f.Seek(truncateTo, io.SeekStart); err != nil {
		return nil, err
	}

	var tw *tar.Writer
	if isCompressed {
		// Initialize the appropriate compressor starting from truncated position
		ci, ok := compressors.Load(method)
		if !ok {
			return nil, ErrAlgorithm
		}
		comp, err := ci.(Compressor)(f)
		if err != nil {
			return nil, err
		}
		tw = tar.NewWriter(comp)
	} else {
		tw = tar.NewWriter(f)
	}

	return &Updater{
		f:            f,
		tw:           tw,
		isCompressed: isCompressed,
		compMethod:   method,
		shadowStart:  shadowStart,
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
			targetEnd = tr.pos + ((hdr.Size + 511) &^ 511)
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