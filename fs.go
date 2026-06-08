package tar

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
    "io"
    "fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

type TarFS struct {
	ArchivePath      string
	IndexPath        string
	Index            *Index
	method           uint16
	xzBlocks         []BlockOffset
	closer           io.Closer
	isTemporaryIndex bool
	password         string
}

type FSOption func(*fsOptions)

type fsOptions struct {
	password string
}

// WithFSPassword provides the password for decrypting F4Crypt encrypted archives in TarFS.
func WithFSPassword(p string) FSOption {
	return func(o *fsOptions) {
		o.password = p
	}
}
// NewFS opens a tar archive as a standard Go fs.FS (File System).
// This enables integration with http.FileServer, fs.WalkDir, etc.
// If the SQLite index does not exist, it will be generated automatically.
func NewFS(archivePath, indexPath string, opts ...FSOption) (*TarFS, error) {
	var options fsOptions
	for _, o := range opts {
		o(&options)
	}

	if indexPath == "" {
		var err error
		indexPath, err = GetStandardIndexPath(archivePath)
		if err != nil {
			return nil, err
		}
	}

	mvr, size, err := OpenMultiVolume(archivePath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	var ra io.ReaderAt = mvr

	ra, size, err = checkF4Crypt(ra, size, options.password)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	method, err := DetectFormat(ra)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	// Try to find embedded F4 Shadow Index
	isTemporaryIndex := false
	if _, errStat := os.Stat(indexPath); os.IsNotExist(errStat) {
		shadowPayload, errShadow := extractShadowIndex(ra, size, method)
		if errShadow == nil && len(shadowPayload) > 0 {
			// Write embedded SQLite index to disk temporarily (SQLite requires a physical file)
			if errWrite := os.WriteFile(indexPath, shadowPayload, 0600); errWrite == nil {
				isTemporaryIndex = true
			}
		} else {
			// No embedded index found, build it on-the-fly
			if errIdx := IndexArchive(archivePath, indexPath); errIdx != nil {
				mvr.Close()
				return nil, errIdx
			}
		}
	}

	idx, err := OpenIndex(indexPath)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	var xzBlocks []BlockOffset
	if method == XZ {
		xzBlocks, _ = parseXZIndex(ra, size)
	}

	return &TarFS{
		ArchivePath: archivePath,
		IndexPath:   indexPath,
		Index:       idx,
		method:           method,
		xzBlocks:         xzBlocks,
		closer:           mvr,
		isTemporaryIndex: isTemporaryIndex,
		password:         options.password,
	}, nil
}
// GetStandardIndexPath attempts to place the SQLite index next to the archive (sidecar).
// If the directory is read-only, it falls back to the user's cache directory.
func GetStandardIndexPath(archivePath string) (string, error) {
	absPath, err := filepath.Abs(archivePath)
	if err != nil {
		return "", err
	}

	// Try sidecar file first (ratarmount default behavior)
	sidecarPath := absPath + ".index.sqlite"

	// Проверяем права на запись в директорию рядом с архивом
	if _, errStat := os.Stat(sidecarPath); errStat == nil {
		// Файл уже существует. Проверяем, можем ли мы открыть его на чтение/запись
		f, err := os.OpenFile(sidecarPath, os.O_RDWR, 0644)
		if err == nil {
			f.Close()
			return sidecarPath, nil
		}
	} else {
		// Файла нет. Проверяем возможность создания, но обязательно удаляем за собой
		f, err := os.OpenFile(sidecarPath, os.O_CREATE|os.O_RDWR, 0644)
		if err == nil {
			f.Close()
			os.Remove(sidecarPath) // Безопасно удаляем, так как мы только что его создали для теста
			return sidecarPath, nil
		}
	}

	// Fallback to cache directory if archive directory is read-only
	var cacheDir string
	if runtime.GOOS == "windows" {
		cacheDir = os.Getenv("LOCALAPPDATA")
		if cacheDir == "" {
			cacheDir = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		cacheDir = filepath.Join(cacheDir, "ratarmount", "Cache")
	} else {
		cacheDir = os.Getenv("XDG_CACHE_HOME")
		if cacheDir == "" {
			home := os.Getenv("HOME")
			if home == "" {
				return "", fmt.Errorf("tar: cannot determine user home directory")
			}
			cacheDir = filepath.Join(home, ".cache")
		}
		cacheDir = filepath.Join(cacheDir, "ratarmount")
	}

	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		// If even cache dir creation fails, try sidecar anyway (it will fail later, but it's the last resort)
		return sidecarPath, nil
	}

	cleanName := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, absPath)

	cleanName = strings.TrimPrefix(cleanName, "_")
	return filepath.Join(cacheDir, cleanName+".sqlite"), nil
}

func readVLI(r io.Reader) (uint64, error) {
	var v uint64
	var shift uint
	for {
		var b [1]byte
		if _, err := r.Read(b[:]); err != nil {
			return 0, err
		}
		v |= uint64(b[0]&0x7F) << shift
		if b[0]&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 64 {
			return 0, errors.New("tar: VLI overflow")
		}
	}
	return v, nil
}

func parseXZIndex(r io.ReaderAt, fileSize int64) ([]BlockOffset, error) {
	if fileSize < 24 {
		return nil, errors.New("tar: file too small for XZ index")
	}
	var footer [12]byte
	if _, err := r.ReadAt(footer[:], fileSize-12); err != nil {
		return nil, err
	}
	if footer[10] != 0x59 || footer[11] != 0x5a {
		return nil, errors.New("tar: invalid XZ footer magic")
	}
	backwardSize := binary.LittleEndian.Uint32(footer[4:8])
	indexSize := int64(backwardSize+1) * 4

	indexOffset := fileSize - 12 - indexSize
	if indexOffset < 12 {
		return nil, errors.New("tar: invalid XZ index offset")
	}

	sr := io.NewSectionReader(r, indexOffset, indexSize)
	var indIndicator [1]byte
	if _, err := sr.Read(indIndicator[:]); err != nil {
		return nil, err
	}
	if indIndicator[0] != 0x00 {
		return nil, errors.New("tar: invalid XZ index indicator")
	}

	numRecords, err := readVLI(sr)
	if err != nil {
		return nil, err
	}

	offsets := make([]BlockOffset, numRecords)
	var currComp int64 = 12
	var currUncomp int64 = 0

	for i := uint64(0); i < numRecords; i++ {
		unpaddedSize, err := readVLI(sr)
		if err != nil {
			return nil, err
		}
		uncompressedSize, err := readVLI(sr)
		if err != nil {
			return nil, err
		}

		offsets[i] = BlockOffset{
			BlockOffset: currComp,
			DataOffset:  currUncomp,
		}

		paddedSize := (unpaddedSize + 3) &^ 3
		currComp += int64(paddedSize)
		currUncomp += int64(uncompressedSize)
	}

	return offsets, nil
}

func (t *TarFS) Close() error {
	var err1, err2 error
	err1 = t.Index.Close()
	if t.closer != nil {
		err2 = t.closer.Close()
	}

	if t.isTemporaryIndex {
		os.Remove(t.IndexPath)
	}

	if err1 != nil {
		return err1
	}
	return err2
}

func (t *TarFS) RecursiveSize(name string) (int64, error) {
	return t.Index.RecursiveSize("/" + name)
}

func (t *TarFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	node, err := t.Index.Lookup("/" + name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	if node.Type == TypeDir || (node.Path == "/" && node.Name == "") {
		return &dirFile{node: node, tfs: t, offset: 0}, nil
	}

	mvr, size, err := OpenMultiVolume(t.ArchivePath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	var ra io.ReaderAt = mvr

	ra, size, err = checkF4Crypt(ra, size, t.password)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	targetOffset := node.Offset
	if node.IsSparse {
		targetOffset = node.OffsetHeader
	}

	// For uncompressed TARs, random access is instantaneous O(1) via SectionReader.
	if t.method == Store {
		if node.IsSparse {
			sr := io.NewSectionReader(ra, targetOffset, size - targetOffset)
			tr := NewReader(sr)
			if _, err := tr.Next(); err != nil {
				mvr.Close()
				return nil, err
			}
			return &tarFile{node: node, r: tr, c: mvr}, nil
		}
		sr := io.NewSectionReader(ra, targetOffset, node.Size)
		return &tarFile{node: node, r: sr, c: mvr}, nil
	}

	// Emulate random access for compressed streams.
	di, ok := decompressors.Load(t.method)
	if !ok {
		mvr.Close()
		return nil, ErrAlgorithm
	}
	decompressorObj := di.(Decompressor)

	var dcomp io.ReadCloser

	// O(1) Fast path: XZ native index block lookup
	if t.method == XZ && len(t.xzBlocks) > 0 {
		var best *BlockOffset
		for i := range t.xzBlocks {
			if t.xzBlocks[i].DataOffset <= targetOffset {
				if best == nil || t.xzBlocks[i].DataOffset > best.DataOffset {
					best = &t.xzBlocks[i]
				}
			}
		}

		if best != nil {
			var header [12]byte
			_, err = ra.ReadAt(header[:], 0)
			if err == nil {
				sr := io.NewSectionReader(ra, best.BlockOffset, 1<<63-1)
				mr := io.MultiReader(bytes.NewReader(header[:]), sr)
				xr, err := xz.NewReader(mr)
				if err == nil {
					dcomp = io.NopCloser(xr)
					remaining := targetOffset - best.DataOffset
					if remaining > 0 {
						if _, err := io.CopyN(io.Discard, dcomp, remaining); err != nil && err != io.EOF {
							dcomp.Close()
							mvr.Close()
							return nil, err
						}
					}

					if node.IsSparse {
						tr := NewReader(dcomp)
						if _, err := tr.Next(); err != nil {
							dcomp.Close()
							mvr.Close()
							return nil, err
						}
						return &tarFile{node: node, r: tr, c: multiCloser{dcomp, mvr}}, nil
					}
					lr := io.LimitReader(dcomp, node.Size)
					return &tarFile{node: node, r: lr, c: multiCloser{dcomp, mvr}}, nil
				}
			}
		}
	}

	// O(1) Fast path: ZSTD / BZIP2 block offset lookup
	table := ""
	if t.method == ZSTD {
		table = "zstdblocks"
	} else if t.method == BZIP2 {
		table = "bzip2blocks"
	}

	if table != "" {
		if importer, ok := di.(BlockOffsetImporter); ok {
			bo, err := t.Index.GetClosestBlockOffset(table, targetOffset)
			if err == nil && bo != nil {
				dcomp, err = importer.ResumeFromBlockOffset(ra, bo)
				if err == nil {
					remaining := targetOffset - bo.DataOffset
					if remaining > 0 {
						if _, err := io.CopyN(io.Discard, dcomp, remaining); err != nil && err != io.EOF {
							dcomp.Close()
							mvr.Close()
							return nil, err
						}
					}

					if node.IsSparse {
						tr := NewReader(dcomp)
						if _, err := tr.Next(); err != nil {
							dcomp.Close()
							mvr.Close()
							return nil, err
						}
						return &tarFile{node: node, r: tr, c: multiCloser{dcomp, mvr}}, nil
					}
					lr := io.LimitReader(dcomp, node.Size)
					return &tarFile{node: node, r: lr, c: multiCloser{dcomp, mvr}}, nil
				}
			}
		}
	}

	// O(1) Fast path: GZIP serialized index lookup
	if t.method == GZIP {
		if importer, ok := di.(GzipIndexImporter); ok {
			indexData, err := t.Index.GetGzipIndex()
			if err == nil && len(indexData) > 0 {
				dcomp, uncompOffset, err := importer.ResumeFromGzipIndex(ra, indexData, targetOffset)
				if err == nil {
					// GZIP index seekable decoder can Seek() directly inside dcomp
					if seeker, ok := dcomp.(io.ReadSeeker); ok {
						seeker.Seek(targetOffset, io.SeekStart)
					} else {
						remaining := targetOffset - uncompOffset
						if remaining > 0 {
							if _, err := io.CopyN(io.Discard, dcomp, remaining); err != nil && err != io.EOF {
								dcomp.Close()
								mvr.Close()
								return nil, err
							}
						}
					}

					if node.IsSparse {
						tr := NewReader(dcomp)
						if _, err := tr.Next(); err != nil {
							dcomp.Close()
							mvr.Close()
							return nil, err
						}
						return &tarFile{node: node, r: tr, c: multiCloser{dcomp, mvr}}, nil
					}
					lr := io.LimitReader(dcomp, node.Size)
					return &tarFile{node: node, r: lr, c: multiCloser{dcomp, mvr}}, nil
				}
			}
		}
	}

	// O(N) Fallback path: decompress from the beginning
	sr := io.NewSectionReader(ra, 0, size)
	dcomp, err = decompressorObj.Decompress(sr)
	if err != nil {
		mvr.Close()
		return nil, err
	}

	_, err = io.CopyN(io.Discard, dcomp, targetOffset)
	if err != nil && err != io.EOF {
		dcomp.Close()
		mvr.Close()
		return nil, err
	}

	if node.IsSparse {
		tr := NewReader(dcomp)
		if _, err := tr.Next(); err != nil {
			dcomp.Close()
			mvr.Close()
			return nil, err
		}
		return &tarFile{node: node, r: tr, c: multiCloser{dcomp, mvr}}, nil
	}
	lr := io.LimitReader(dcomp, node.Size)
	return &tarFile{node: node, r: lr, c: multiCloser{dcomp, mvr}}, nil
}

type multiCloser []io.Closer

func (mc multiCloser) Close() error {
	var firstErr error
	for _, c := range mc {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// -- fs.FileInfo / fs.DirEntry Implementation

type fileInfo struct {
	node *FileNode
}

func (fi fileInfo) Name() string { return fi.node.Name }
func (fi fileInfo) Size() int64  { return fi.node.Size }
func (fi fileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(fi.node.Mode & 0777)
	if fi.node.Type == TypeDir || (fi.node.Path == "/" && fi.node.Name == "") {
		mode |= fs.ModeDir
	}
	if fi.node.Type == TypeSymlink || fi.node.Type == TypeLink {
		mode |= fs.ModeSymlink
	}
	return mode
}
func (fi fileInfo) ModTime() time.Time { return fi.node.ModTime }
func (fi fileInfo) IsDir() bool        { return fi.Mode().IsDir() }
func (fi fileInfo) Sys() any           { return fi.node }

// -- fs.File implementations

type dirFile struct {
	node   *FileNode
	tfs    *TarFS
	offset int
	kids   []FileNode
	loaded bool
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return fileInfo{d.node}, nil }
func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.node.Name, Err: bytes.ErrTooLarge}
}
func (d *dirFile) Close() error { return nil }
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.loaded {
		p := d.node.Path
		if d.node.Name != "" {
			if p == "/" {
				p = "/" + d.node.Name
			} else {
				p = p + "/" + d.node.Name
			}
		}
		kids, err := d.tfs.Index.List(p)
		if err != nil {
			return nil, err
		}
		d.kids = kids
		d.loaded = true
	}

	if d.offset >= len(d.kids) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}

	end := len(d.kids)
	if n > 0 && d.offset+n < end {
		end = d.offset + n
	}

	var res []fs.DirEntry
	for _, k := range d.kids[d.offset:end] {
		kCopy := k
		res = append(res, fs.FileInfoToDirEntry(fileInfo{&kCopy}))
	}
	d.offset = end
	return res, nil
}

type tarFile struct {
	node *FileNode
	r    io.Reader
	c    io.Closer
}

func (f *tarFile) Stat() (fs.FileInfo, error) { return fileInfo{f.node}, nil }
func (f *tarFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *tarFile) Close() error               { return f.c.Close() }