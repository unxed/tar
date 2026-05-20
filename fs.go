package tar

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"time"
)

type TarFS struct {
	ArchivePath string
	IndexPath   string
	Index       *Index
	method      uint16
}

// NewFS opens a tar archive as a standard Go fs.FS (File System).
// This enables integration with http.FileServer, fs.WalkDir, etc.
// If the SQLite index does not exist, it will be generated automatically.
func NewFS(archivePath, indexPath string) (*TarFS, error) {
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		if err := IndexArchive(archivePath, indexPath); err != nil {
			return nil, err
		}
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	method, err := DetectFormat(f)
	f.Close()
	if err != nil {
		return nil, err
	}

	idx, err := OpenIndex(indexPath)
	if err != nil {
		return nil, err
	}

	return &TarFS{
		ArchivePath: archivePath,
		IndexPath:   indexPath,
		Index:       idx,
		method:      method,
	}, nil
}

func (t *TarFS) Close() error {
	return t.Index.Close()
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

	f, err := os.Open(t.ArchivePath)
	if err != nil {
		return nil, err
	}

	// For uncompressed TARs, random access is instantaneous O(1) via SectionReader.
	if t.method == Store {
		sr := io.NewSectionReader(f, node.Offset, node.Size)
		return &tarFile{node: node, r: sr, c: f}, nil
	}

	// Emulate random access for compressed streams.
	di, ok := decompressors.Load(t.method)
	if !ok {
		f.Close()
		return nil, ErrAlgorithm
	}
	decompressorObj := di.(Decompressor)

	var dcomp io.ReadCloser

	// O(1) Fast path: ZSTD / BZIP2 block offset lookup
	table := ""
	if t.method == ZSTD {
		table = "zstdblocks"
	} else if t.method == BZIP2 {
		table = "bzip2blocks"
	}

	if table != "" {
		if importer, ok := di.(BlockOffsetImporter); ok {
			bo, err := t.Index.GetClosestBlockOffset(table, node.Offset)
			if err == nil && bo != nil {
				dcomp, err = importer.ResumeFromBlockOffset(f, bo)
				if err == nil {
					remaining := node.Offset - bo.DataOffset
					if remaining > 0 {
						if _, err := io.CopyN(io.Discard, dcomp, remaining); err != nil && err != io.EOF {
							dcomp.Close()
							f.Close()
							return nil, err
						}
					}

					lr := io.LimitReader(dcomp, node.Size)
					return &tarFile{node: node, r: lr, c: multiCloser{dcomp, f}}, nil
				}
			}
		}
	}

	// O(1) Fast path: GZIP serialized index lookup
	if t.method == GZIP {
		if importer, ok := di.(GzipIndexImporter); ok {
			indexData, err := t.Index.GetGzipIndex()
			if err == nil && len(indexData) > 0 {
				dcomp, uncompOffset, err := importer.ResumeFromGzipIndex(f, indexData, node.Offset)
				if err == nil {
					// GZIP index seekable decoder can Seek() directly inside dcomp
					if seeker, ok := dcomp.(io.ReadSeeker); ok {
						seeker.Seek(node.Offset, io.SeekStart)
					} else {
						remaining := node.Offset - uncompOffset
						if remaining > 0 {
							if _, err := io.CopyN(io.Discard, dcomp, remaining); err != nil && err != io.EOF {
								dcomp.Close()
								f.Close()
								return nil, err
							}
						}
					}

					lr := io.LimitReader(dcomp, node.Size)
					return &tarFile{node: node, r: lr, c: multiCloser{dcomp, f}}, nil
				}
			}
		}
	}

	// O(N) Fallback path: decompress from the beginning
	f.Seek(0, io.SeekStart)
	dcomp, err = decompressorObj.Decompress(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	_, err = io.CopyN(io.Discard, dcomp, node.Offset)
	if err != nil && err != io.EOF {
		dcomp.Close()
		f.Close()
		return nil, err
	}

	lr := io.LimitReader(dcomp, node.Size)
	return &tarFile{node: node, r: lr, c: multiCloser{dcomp, f}}, nil
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