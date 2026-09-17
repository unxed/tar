package tar

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/unxed/xz"
)

type AppendMode int

const (
	APPEND_MODE_OVERWRITE AppendMode = iota
)

var ErrArchiveLocked = errors.New("tar: cannot modify archive, it is locked")

// ErrDataAfterEnd is returned by NewUpdater for an uncompressed archive that
// has something other than zero blocks after its end-of-archive marker and
// before its embedded index, if it has one. An entry written there would not
// be seen, and truncating the archive to append would destroy what is there.
var ErrDataAfterEnd = errors.New("tar: data after the end of the archive; not updating it in place")

// Updater adds entries to an existing archive.
//
// Every reader that follows the tar format stops at the end-of-archive blocks,
// so an appended entry has to take their place. An uncompressed archive is
// updated in place: it is cut back to where its last entry ends -- which also
// drops an embedded index (F4SS), since the index no longer describes the
// archive -- and entries are written from there. Appending the name of an
// entry the archive already holds removes that entry first.
//
// A compressed archive cannot be cut there: its end-of-archive blocks are
// inside the compressed stream. Entries written after that stream were read
// by this package's reader, which carries on past end-of-archive, but not by
// tar or 7-Zip, which stop where the format says to. So appended entries are
// kept aside, and Close writes the archive anew: its entries, less those
// whose names were appended, then the appended ones, in one compressed
// stream. The embedded index is dropped here too.
type Updater struct {
	f      *os.File
	method uint16
	opts   []WriterOption

	// Uncompressed archives. The archive is cut back to entriesEnd when the
	// first entry is appended; tw is nil until then.
	tw         *tar.Writer
	spans      []entrySpan
	entriesEnd int64

	// Compressed archives.
	srcLimit  int64
	pending   *os.File
	pendingTW *tar.Writer
	names     map[string]bool
}

// entrySpan is where an entry of an uncompressed archive lies, extended
// headers and padding included.
type entrySpan struct {
	name       string
	start, end int64
}

func NewUpdater(f *os.File, mode AppendMode, opts ...WriterOption) (*Updater, error) {
	if mode != APPEND_MODE_OVERWRITE {
		return nil, errors.New("tar: only APPEND_MODE_OVERWRITE is supported")
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	u := &Updater{f: f, method: Store, opts: opts}
	if stat.Size() == 0 {
		return u, nil
	}

	method, err := DetectFormat(f)
	if err != nil {
		return nil, err
	}
	u.method = method

	limit := stat.Size()
	shadowStart, shadowSize, _ := LocateShadowStream(f, stat.Size(), method)
	if shadowStart > 0 && shadowSize > 0 {
		propBytes, err := extractShadowFile(f, stat.Size(), method, ".tarext/f4/properties.txt")
		if err == nil && len(propBytes) > 0 {
			props := parseProperties(propBytes)
			if props["locked"] == "true" {
				return nil, ErrArchiveLocked
			}
		}
		limit = shadowStart
	}

	if method == Store {
		spans, end, err := walkEntries(f, limit)
		if err != nil {
			return nil, err
		}
		// Nothing is cut until an entry is appended: an updater that is
		// closed without one, as when a removal it does not support
		// fails, leaves the archive and its embedded index as they were.
		u.spans = spans
		u.entriesEnd = end
		return u, nil
	}

	if _, ok := compressors.Load(method); !ok {
		return nil, ErrAlgorithm
	}
	if _, ok := decompressors.Load(method); !ok {
		return nil, ErrAlgorithm
	}
	pending, err := os.CreateTemp(filepath.Dir(f.Name()), ".tar-append-*")
	if err != nil {
		return nil, err
	}
	u.srcLimit = limit
	u.pending = pending
	u.pendingTW = tar.NewWriter(pending)
	u.names = make(map[string]bool)
	return u, nil
}

// walkEntries reads the entries of the uncompressed tar stream in the first
// limit bytes of r and returns where each lies and where the last one ends,
// which is where its end-of-archive blocks begin. Anything but zero bytes
// between there and limit is ErrDataAfterEnd.
func walkEntries(r io.ReaderAt, limit int64) ([]entrySpan, int64, error) {
	sr := io.NewSectionReader(r, 0, limit)
	tr := tar.NewReader(sr)
	var spans []entrySpan
	var end int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		// A sparse entry's Size is the size of the file it expands to,
		// not of what the archive stores, so its data is read through to
		// find where it stops. Every other entry stores Size bytes, or
		// none for the types the format gives no data.
		if hdr.Typeflag == tar.TypeGNUSparse || hdr.PAXRecords["GNU.sparse.major"] != "" || hdr.PAXRecords["GNU.sparse.size"] != "" {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return nil, 0, err
			}
			pos, _ := sr.Seek(0, io.SeekCurrent)
			spans = append(spans, entrySpan{name: hdr.Name, start: end, end: roundUpBlock(pos)})
		} else {
			pos, _ := sr.Seek(0, io.SeekCurrent)
			size := hdr.Size
			switch hdr.Typeflag {
			case tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeDir, tar.TypeFifo:
				size = 0
			}
			spans = append(spans, entrySpan{name: hdr.Name, start: end, end: roundUpBlock(pos + size)})
		}
		end = spans[len(spans)-1].end
	}

	rest := io.NewSectionReader(r, end, limit-end)
	buf := make([]byte, 64*1024)
	for {
		n, err := rest.Read(buf)
		if !bytes.Equal(buf[:n], make([]byte, n)) {
			return nil, 0, ErrDataAfterEnd
		}
		if err == io.EOF {
			return spans, end, nil
		}
		if err != nil {
			return nil, 0, err
		}
	}
}

func roundUpBlock(n int64) int64 {
	return (n + blockSize - 1) &^ (blockSize - 1)
}

const blockSize = 512

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
	return u.AppendHeader(&tar.Header{Name: name, Mode: 0644, Size: size}, r)
}

// AppendHeader adds an entry described by hdr -- a file, a directory, a link,
// a device node or a FIFO -- with the hdr.Size bytes r holds for a file, and
// nothing read from r for the other types.
func (u *Updater) AppendHeader(hdr *Header, r io.Reader) error {
	if u.pending != nil {
		u.names[hdr.Name] = true
		return writeEntry(u.pendingTW, hdr, r)
	}

	if u.tw == nil {
		if err := u.f.Truncate(u.entriesEnd); err != nil {
			return err
		}
		if _, err := u.f.Seek(u.entriesEnd, io.SeekStart); err != nil {
			return err
		}
		u.tw = tar.NewWriter(u.f)
	}
	if err := u.tw.Flush(); err != nil {
		return err
	}
	end, err := u.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	for i, s := range u.spans {
		if s.name != hdr.Name {
			continue
		}
		if err := u.removeSpan(i, end); err != nil {
			return err
		}
		end -= s.end - s.start
		break
	}
	if err := writeEntry(u.tw, hdr, r); err != nil {
		return err
	}
	if err := u.tw.Flush(); err != nil {
		return err
	}
	newEnd, err := u.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	u.spans = append(u.spans, entrySpan{name: hdr.Name, start: end, end: newEnd})
	return nil
}

// removeSpan takes entry i out of an uncompressed archive whose entries end at
// end, moving the entries after it down, and leaves the file positioned at
// the new end.
func (u *Updater) removeSpan(i int, end int64) error {
	s := u.spans[i]
	n := s.end - s.start
	buffer := make([]byte, 2*1024*1024)
	for rp, wp := s.end, s.start; rp < end; {
		chunk := buffer
		if left := end - rp; left < int64(len(chunk)) {
			chunk = chunk[:left]
		}
		m, err := u.f.ReadAt(chunk, rp)
		if m > 0 {
			if _, werr := u.f.WriteAt(chunk[:m], wp); werr != nil {
				return werr
			}
			rp += int64(m)
			wp += int64(m)
		}
		if err != nil && err != io.EOF {
			return err
		}
		if m == 0 {
			break
		}
	}
	if err := u.f.Truncate(end - n); err != nil {
		return err
	}
	if _, err := u.f.Seek(end-n, io.SeekStart); err != nil {
		return err
	}
	u.spans = append(u.spans[:i], u.spans[i+1:]...)
	for j := i; j < len(u.spans); j++ {
		u.spans[j].start -= n
		u.spans[j].end -= n
	}
	u.tw = tar.NewWriter(u.f)
	return nil
}

func writeEntry(tw *tar.Writer, hdr *tar.Header, r io.Reader) error {
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if r != nil && hdr.Size > 0 {
		if _, err := io.CopyBuffer(tw, r, make([]byte, 1024*1024)); err != nil {
			return err
		}
	}
	return nil
}

func (u *Updater) Close() error {
	if u.pending == nil {
		if u.tw == nil {
			return nil
		}
		return u.tw.Close()
	}
	defer func() {
		_ = u.pending.Close()
		_ = os.Remove(u.pending.Name())
	}()
	if err := u.pendingTW.Flush(); err != nil {
		return err
	}
	if len(u.names) == 0 {
		return nil
	}
	return u.rewrite()
}

// rewrite writes a compressed archive anew: the entries it holds, less those
// whose names were appended, then the appended ones.
func (u *Updater) rewrite() (err error) {
	out, err := os.CreateTemp(filepath.Dir(u.f.Name()), ".tar-rewrite-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
		_ = os.Remove(out.Name())
	}()

	comp, err := u.newCompressor(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(comp)

	dc, _ := decompressors.Load(u.method)
	dcomp, err := dc.(Decompressor).Decompress(io.NewSectionReader(u.f, 0, u.srcLimit))
	if err != nil {
		_ = comp.Close()
		return err
	}
	rd := bufio.NewReaderSize(dcomp, 1024*1024)
	src := tar.NewReader(rd)
	afterEnd := false
	next := func() (*tar.Header, error) {
		for {
			hdr, err := src.Next()
			if err == io.EOF {
				// Carry on past end-of-archive, as this package's
				// reader does: an earlier version of this updater
				// left appended entries there.
				more := tar.NewReader(rd)
				hdr, err = more.Next()
				if err != nil {
					return nil, io.EOF
				}
				src, afterEnd = more, true
			}
			if err != nil {
				return nil, err
			}
			// What follows end-of-archive under .tarext/ is an
			// embedded index (F4SS) that LocateShadowStream has no
			// footer for, as with xz; it describes the old archive.
			if afterEnd && strings.HasPrefix(hdr.Name, ".tarext/") {
				continue
			}
			return hdr, nil
		}
	}
	copyErr := copyEntries(tw, next, readerFunc(func(p []byte) (int, error) { return src.Read(p) }), u.names)
	if cerr := dcomp.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr == nil {
		if _, serr := u.pending.Seek(0, io.SeekStart); serr != nil {
			copyErr = serr
		} else {
			ptr := tar.NewReader(u.pending)
			copyErr = copyEntries(tw, ptr.Next, ptr, nil)
		}
	}
	if copyErr == nil {
		copyErr = tw.Close()
	}
	if cerr := comp.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		return fmt.Errorf("tar: rewriting the archive: %w", copyErr)
	}

	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := u.f.Truncate(0); err != nil {
		return err
	}
	if _, err := u.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = io.CopyBuffer(u.f, out, make([]byte, 1024*1024))
	return err
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// copyEntries copies every entry next produces, and its data from r, to tw,
// leaving out the names in skip.
func copyEntries(tw *tar.Writer, next func() (*tar.Header, error), r io.Reader, skip map[string]bool) error {
	for {
		hdr, err := next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if skip[hdr.Name] {
			continue
		}
		// The reader hands a sparse entry over expanded; it is written
		// back as the regular file it reads as.
		if hdr.Typeflag == tar.TypeGNUSparse {
			hdr.Typeflag = tar.TypeReg
		}
		for k := range hdr.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse.") {
				delete(hdr.PAXRecords, k)
			}
		}
		hdr.Format = tar.FormatUnknown
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.CopyBuffer(tw, r, make([]byte, 1024*1024)); err != nil {
			return err
		}
	}
}

func (u *Updater) newCompressor(w io.Writer) (io.WriteCloser, error) {
	var wopts writerOptions
	for _, o := range u.opts {
		o(&wopts)
	}
	if wopts.level != 0 {
		switch u.method {
		case GZIP:
			return pgzip.NewWriterLevel(w, wopts.level)
		case ZSTD:
			return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(wopts.level)))
		case XZ:
			dictCap := 8 * 1024 * 1024
			if wopts.level > 0 && wopts.level <= 9 {
				lzmaDictCapExps := []uint{18, 20, 21, 22, 22, 23, 23, 24, 25, 26}
				dictCap = 1 << lzmaDictCapExps[wopts.level]
			}
			config := xz.WriterConfig{CheckSum: xz.CRC64, DictCap: dictCap}
			if err := config.Verify(); err != nil {
				return nil, err
			}
			return config.NewWriter(w)
		}
	}
	ci, _ := compressors.Load(u.method)
	return ci.(Compressor)(w)
}
