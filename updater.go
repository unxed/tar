package tar

import (
	"archive/tar"
    "bufio"
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
	f            *os.File
	tw           *tar.Writer
	comp         io.WriteCloser
	isCompressed bool
	compMethod   uint16
	shadowStart  int64
	buf          *bufio.Writer
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
	var comp io.WriteCloser

	if isCompressed {
		// Initialize the appropriate compressor starting from truncated position
		ci, ok := compressors.Load(method)
		if !ok {
			return nil, ErrAlgorithm
		}
		var err error
		comp, err = ci.(Compressor)(f)
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
		comp:         comp,
		isCompressed: isCompressed,
		compMethod:   method,
		shadowStart:  shadowStart,
	}, nil
}

// Append creates a new file entry in the archive.
func (u *Updater) Append(name string, size int64, data []byte) error {
	var r io.Reader
	if len(data) > 0 {
		r = bytes.NewReader(data)
	}
	return u.AppendReader(name, size, r)
}

// AppendReader creates a new file entry in the archive from an io.Reader stream.
func (u *Updater) AppendReader(name string, size int64, r io.Reader) error {
	println("[DIAG-UPD] Append: Starting append for", name, "size=", size)

	stat, err := u.f.Stat()
	if err != nil {
		return err
	}
	endOfArchive := stat.Size()
	println("[DIAG-UPD] Append: Current physical size=", endOfArchive)

	// Дамп байт перед началом записи
	headerBytes := make([]byte, 32)
	u.f.ReadAt(headerBytes, 0)
	print("[DIAG-UPD] Append: First 32 bytes of archive: ")
	for _, b := range headerBytes {
		print(uint8(b), " ")
	}
	println()

	var targetStart int64 = -1
	var targetEnd int64 = -1

	if !u.isCompressed {
		if _, err := u.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		tr := &trackingReader{r: u.f}
		trd := NewReader(tr)

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
				println("[DIAG-UPD] Append: Found existing entry duplication! targetStart=", targetStart, "targetEnd=", targetEnd)
				break
			}
		}
	} else {
		println("[DIAG-UPD] Append: Compressed archive, skipping duplication scan")
	}

	if targetStart != -1 && targetEnd != -1 {
		removeSize := targetEnd - targetStart
		if targetEnd < endOfArchive {
			const chunkBufSize = 2 * 1024 * 1024 // 2MB для быстрого сдвига данных
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
		println("[DIAG-UPD] Append: Compaction triggered. Truncating to", newSize)
		if err := u.f.Truncate(newSize); err != nil {
			return err
		}
		if _, err := u.f.Seek(newSize, io.SeekStart); err != nil {
			return err
		}
		u.tw = NewWriter(u.f)
	} else {
		pos, _ := u.f.Seek(0, io.SeekEnd)
		println("[DIAG-UPD] Append: Appending at SeekEnd position=", pos)
		if !u.isCompressed {
			u.tw = NewWriter(u.f)
		}
	}

	hdr := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: size,
	}
	println("[DIAG-UPD] Append: Writing Header for", name)
	if err := u.tw.WriteHeader(hdr); err != nil {
		println("[DIAG-UPD] Append: WriteHeader failed:", err.Error())
		return err
	}
	if r != nil {
		println("[DIAG-UPD] Append: Writing content size=", size)
		if _, err := io.CopyBuffer(u.tw, r, make([]byte, 1024*1024)); err != nil {
			println("[DIAG-UPD] Append: Write content failed:", err.Error())
			return err
		}
	}
	println("[DIAG-UPD] Append: Done append for", name)
	return nil
}

func (u *Updater) Close() error {
	var err error
	if u.tw != nil {
		err = u.tw.Close()
	}
	if u.comp != nil {
		if cerr := u.comp.Close(); cerr != nil {
			if err == nil { err = cerr }
		}
	}
	// Буфер удален.
	return err
}