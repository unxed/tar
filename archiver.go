package tar

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type ArchiverOption func(*archiverOptions) error

type archiverOptions struct {
	method      uint16
	chroot      string
	indexPath   string
	xattrs      bool
	embeddedIdx bool
	password    string
	recoveryPct int
	splitSize   int64
	lock        bool
	level       int
}

// WithArchiverLevel sets the compression level.
func WithArchiverLevel(level int) ArchiverOption {
	return func(o *archiverOptions) error {
		o.level = level
		return nil
	}
}

// WithArchiverLock locks the archive to prevent modifications.
func WithArchiverLock(b bool) ArchiverOption {
	return func(o *archiverOptions) error {
		o.lock = b
		return nil
	}
}

// WithArchiverSplitSize enables creation of multi-volume archives
func WithArchiverSplitSize(size int64) ArchiverOption {
	return func(o *archiverOptions) error {
		o.splitSize = size
		return nil
	}
}

// WithArchiverEmbeddedIndex appends the index directly inside the archive (F4 Shadow Stream).
// WithArchiverPassword enables F4Crypt AES-256-CTR encryption for the archive.
func WithArchiverPassword(p string) ArchiverOption {
	return func(o *archiverOptions) error {
		o.password = p
		return nil
	}
}
func WithArchiverEmbeddedIndex(b bool) ArchiverOption {
	return func(o *archiverOptions) error {
		o.embeddedIdx = b
		return nil
	}
}
// WithArchiverRecovery устанавливает процент избыточности PAR2 для защиты архива
func WithArchiverRecovery(pct int) ArchiverOption {
	return func(o *archiverOptions) error {
		o.recoveryPct = pct
		return nil
	}
}

// WithArchiverXattrs enables archiving of extended attributes (xattrs, POSIX ACLs, SELinux).
func WithArchiverXattrs(b bool) ArchiverOption {
	return func(o *archiverOptions) error {
		o.xattrs = b
		return nil
	}
}

func WithArchiverMethod(method uint16) ArchiverOption {
	return func(o *archiverOptions) error {
		o.method = method
		return nil
	}
}
func WithArchiverIndex(path string) ArchiverOption {
	return func(o *archiverOptions) error {
		o.indexPath = path
		return nil
	}
}

type Archiver struct {
	wc                   *WriteCloser
	options              archiverOptions
	m                    sync.Mutex
	seenHardLinks        map[hardlinkKey]string
	finalFilename        string
	tempFilename         string
	isTemporaryIndexPath bool
	comment              string
}

// SetComment sets the global archive comment stored inside the F4SS shadow metadata index.
func (a *Archiver) SetComment(comment string) {
	a.comment = comment
}

func NewArchiver(filename string, chroot string, opts ...ArchiverOption) (*Archiver, error) {
	var err error
	if chroot, err = filepath.Abs(chroot); err != nil {
		return nil, err
	}

	a := &Archiver{
		options: archiverOptions{
			method:      GZIP,
			chroot:      chroot,
			xattrs:      true,
			embeddedIdx: true, // Embedded index (F4SS) is enabled by default
		},
		seenHardLinks: make(map[hardlinkKey]string),
		finalFilename: filename,
	}

	for _, o := range opts {
		if err := o(&a.options); err != nil {
			return nil, err
		}
	}

	// If embedded index is enabled but no custom path was provided, generate a temporary SQLite file
	if a.options.embeddedIdx && a.options.indexPath == "" {
		tf, err := os.CreateTemp("", "f4tar-index-*.sqlite")
		if err != nil {
			return nil, err
		}
		tf.Close()
		a.options.indexPath = tf.Name()
		a.isTemporaryIndexPath = true
	}

	targetFile := filename
	if a.options.password != "" {
		tf, err := os.CreateTemp(filepath.Dir(filename), "f4crypt-*.tar")
		if err != nil {
			if a.isTemporaryIndexPath {
				os.Remove(a.options.indexPath)
			}
			return nil, err
		}
		targetFile = tf.Name()
		tf.Close()
		a.tempFilename = targetFile
	}

	var wopts []WriterOption
	if a.options.indexPath != "" {
		wopts = append(wopts, WithWriterIndex(a.options.indexPath))
	}
	if a.options.splitSize > 0 {
		wopts = append(wopts, WithWriterSplitSize(a.options.splitSize))
	}
	if a.options.level != 0 {
		wopts = append(wopts, WithWriterLevel(a.options.level))
	}

	wc, err := CreateWriter(targetFile, a.options.method, wopts...)
	if err != nil {
		if a.tempFilename != "" {
			os.Remove(a.tempFilename)
		}
		return nil, err
	}
	a.wc = wc
	return a, nil
}

func (a *Archiver) closeInternal() error {
	if !a.options.embeddedIdx || a.options.indexPath == "" {
		return a.wc.Close()
	}

	if err := a.wc.Writer.Close(); err != nil {
		return err
	}


	idx := a.wc.idx
	var gzidxData []byte
	if idx != nil {
		if len(a.wc.batch) > 0 {
			idx.Insert(a.wc.batch)
		}
		if a.wc.method == ZSTD && len(a.wc.zstdBlocks) > 0 {
			idx.InsertBlockOffsets("zstdblocks", a.wc.zstdBlocks)
		}
		if a.wc.method == GZIP && len(a.wc.gzPoints) > 0 {
			gzidx := &gzipIndexTrackingReader{
				points:       a.wc.gzPoints,
				uncompOffset: a.wc.uncompTracker.pos,
				spacing:      4 * 1024 * 1024,
				tr:           &trackingByteReader{pos: a.wc.compTracker.pos},
			}
			if data, err := gzidx.ExportGzipIndex(); err == nil {
				idx.SaveGzipIndex(data)
				gzidxData = data
			}
		}
	}

	if a.wc.comp != nil {
		a.wc.comp.Close()
	}

	idx.Close()

	// Read the generated SQLite index into memory
	idxData, err := os.ReadFile(a.options.indexPath)
	if err != nil {
		a.wc.f.Close()
		if a.isTemporaryIndexPath {
			os.Remove(a.options.indexPath)
		}
		return err
	}

	if a.isTemporaryIndexPath {
		os.Remove(a.options.indexPath)
	}

	shadowStartOffset := a.wc.compTracker.pos

	var shadowComp io.WriteCloser
	var shadowWriter io.Writer = a.wc.compTracker

	if a.options.method != Store {
		ci, _ := compressors.Load(a.options.method)
		shadowComp, _ = ci.(Compressor)(shadowWriter)
		shadowWriter = shadowComp
	}

	shadowTar := NewWriter(shadowWriter)
	shadowTar.WriteHeader(&Header{Name: ".tarext/", Mode: 0755, Typeflag: TypeDir})
	shadowTar.WriteHeader(&Header{Name: ".tarext/ratarmount/", Mode: 0755, Typeflag: TypeDir})

	shadowHdr := &Header{
		Name:     ".tarext/ratarmount/index.sqlite",
		Mode:     0644,
		Size:     int64(len(idxData)),
		Typeflag: TypeReg,
	}
	if err := shadowTar.WriteHeader(shadowHdr); err != nil {
		return err
	}
	if _, err := shadowTar.Write(idxData); err != nil {
		return err
	}

	// Write custom F4 metadata to .tarext/f4/properties.txt, keeping index.sqlite completely pristine
	if a.options.lock || a.comment != "" {
		shadowTar.WriteHeader(&Header{Name: ".tarext/f4/", Mode: 0755, Typeflag: TypeDir})
		props := make(map[string]string)
		if a.options.lock {
			props["locked"] = "true"
		}
		if a.comment != "" {
			props["comment"] = a.comment
		}
		propsData := serializeProperties(props)

		propHdr := &Header{
			Name:     ".tarext/f4/properties.txt",
			Mode:     0644,
			Size:     int64(len(propsData)),
			Typeflag: TypeReg,
		}
		if err := shadowTar.WriteHeader(propHdr); err == nil {
			shadowTar.Write(propsData)
		}
	}

	// Write standard GZIDX payload if GZIP method is used
	if a.options.method == GZIP && len(gzidxData) > 0 {
		shadowTar.WriteHeader(&Header{Name: ".tarext/GZIDX/", Mode: 0755, Typeflag: TypeDir})
		gzhdr := &Header{
			Name:     ".tarext/GZIDX/index.gzidx",
			Mode:     0644,
			Size:     int64(len(gzidxData)),
			Typeflag: TypeReg,
		}
		if err := shadowTar.WriteHeader(gzhdr); err == nil {
			shadowTar.Write(gzidxData)
		}
	}

	// Write standard DZIDX payload if ZSTD method is used and block offsets exist
	if a.options.method == ZSTD && len(a.wc.zstdBlocks) > 0 {
		dzidxData := exportDZIDX(a.wc.zstdBlocks, a.wc.compTracker.pos, a.wc.uncompTracker.pos)
		if len(dzidxData) > 0 {
			shadowTar.WriteHeader(&Header{Name: ".tarext/dictzip/", Mode: 0755, Typeflag: TypeDir})
			dzhdr := &Header{
				Name:     ".tarext/dictzip/index.dzidx",
				Mode:     0644,
				Size:     int64(len(dzidxData)),
				Typeflag: TypeReg,
			}
			if err := shadowTar.WriteHeader(dzhdr); err == nil {
				shadowTar.Write(dzidxData)
			}
		}
	}

	shadowTar.Close()

	if shadowComp != nil {
		shadowComp.Close()
	}

	shadowSize := a.wc.compTracker.pos - shadowStartOffset
	err = WriteMagicFooter(a.wc.f, a.options.method, shadowStartOffset, shadowSize)
	if err != nil {
		return err
	}

	return a.wc.f.Close()
}

func (a *Archiver) Close() error {
	err := a.closeInternal()
	if a.options.password != "" {
		encErr := encapsulateF4Crypt(a.finalFilename, a.tempFilename, a.options.password)
		os.Remove(a.tempFilename)
		if err == nil {
			err = encErr
		}
	}
	return err
}

// Archive writes files to the tar sequentially.
// For TAR, unlike ZIP, we cannot write data streams concurrently to the same file.
func (a *Archiver) Archive(ctx context.Context, files map[string]os.FileInfo) error {
	if a.options.xattrs {
		type virtualFile struct {
			path string
			info os.FileInfo
		}
		var virtualFiles []virtualFile

		for name, fi := range files {
			if fi != nil && fi.Mode().IsRegular() {
				streams, _ := getAlternativeDataStreamsFunc(name)
				for _, stream := range streams {
					streamPath := name + stream
					if streamFi, serr := os.Stat(streamPath); serr == nil {
						virtualFiles = append(virtualFiles, virtualFile{
							path: streamPath,
							info: streamFi,
						})
					}
				}
			}
		}

		for _, vf := range virtualFiles {
			files[vf.path] = vf.info
		}
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		fi := files[name]
		path, err := filepath.Abs(name)
		if err != nil {
			return err
		}

		if !strings.HasPrefix(path, a.options.chroot+string(filepath.Separator)) && path != a.options.chroot {
			return fmt.Errorf("%s cannot be archived from outside of chroot (%s)", name, a.options.chroot)
		}

		rel, err := filepath.Rel(a.options.chroot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			continue // Skip root
		}

		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		} else {
			link = getHardLinkTarget(fi, a.seenHardLinks)
		}

		hdr, err := FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if a.options.xattrs {
			if acl, err := getFileSecurityFunc(path); err == nil && len(acl) > 0 {
				if hdr.PAXRecords == nil {
					hdr.PAXRecords = make(map[string]string)
				}
				hdr.PAXRecords["MSWINDOWS.raw_sd"] = base64.StdEncoding.EncodeToString(acl)
			}
		}

		// Enrich header with Unix-specific metadata (UID/GID as text, devices, atime/ctime)
		sysHeader(fi, hdr)

		if link == "" {
		if a.options.xattrs {
			sysXattrs(path, hdr)
		}
			rememberHardLink(fi, hdr.Name, a.seenHardLinks)
		} else if fi.Mode()&os.ModeSymlink == 0 {
			// Bypass Go standard library limitation:
			// manually enforce hardlink type and target path
			hdr.Typeflag = TypeLink
			hdr.Linkname = link
			hdr.Size = 0
		}

		a.m.Lock()
		err = a.wc.WriteHeader(hdr)
		if err != nil {
			a.m.Unlock()
			return err
		}

		if fi.Mode().IsRegular() && hdr.Typeflag != TypeLink {
			f, err := os.Open(path)
			if err != nil {
				a.m.Unlock()
				return err
			}
			_, err = io.Copy(a.wc, f)
			f.Close()
			if err != nil {
				a.m.Unlock()
				return err
			}
		}
		a.m.Unlock()
	}

	return nil
}

// Convenience top-level API compatible with targz package
func Compress(inputFilePath, outputFilePath string) error {
	inputFilePath, err := filepath.Abs(inputFilePath)
	if err != nil {
		return err
	}

	method := GZIP
	if strings.HasSuffix(outputFilePath, ".tar") {
		method = Store
	}

	a, err := NewArchiver(outputFilePath, filepath.Dir(inputFilePath), WithArchiverMethod(method))
	if err != nil {
		return err
	}
	defer a.Close()

	files := make(map[string]os.FileInfo)
	err = filepath.Walk(inputFilePath, func(path string, info os.FileInfo, err error) error {
		if err != nil { return err }
		files[path] = info
		return nil
	})
	if err != nil {
		return err
	}

	return a.Archive(context.Background(), files)
}

func exportDZIDX(offsets []BlockOffset, totalComp, totalUncomp int64) []byte {
	if len(offsets) == 0 {
		return nil
	}
	p := len(offsets)
	entrySize := 4
	buf := new(bytes.Buffer)
	buf.Write([]byte("DZIDX"))
	buf.WriteByte(1) // version
	var entSizeBuf [2]byte
	binary.LittleEndian.PutUint16(entSizeBuf[:], uint16(entrySize))
	buf.Write(entSizeBuf[:])
	var chunkInterval uint32 = 4 * 1024 * 1024
	var chunkIntBuf [4]byte
	binary.LittleEndian.PutUint32(chunkIntBuf[:], chunkInterval)
	buf.Write(chunkIntBuf[:])
	var pBuf [4]byte
	binary.LittleEndian.PutUint32(pBuf[:], uint32(p))
	buf.Write(pBuf[:])
	var totalUncompBuf [8]byte
	binary.LittleEndian.PutUint64(totalUncompBuf[:], uint64(totalUncomp))
	buf.Write(totalUncompBuf[:])
	var totalCompBuf [8]byte
	binary.LittleEndian.PutUint64(totalCompBuf[:], uint64(totalComp))
	buf.Write(totalCompBuf[:])
	for i := 0; i < p; i++ {
		var compSize int64
		if i == p-1 {
			compSize = totalComp - offsets[i].BlockOffset
		} else {
			compSize = offsets[i+1].BlockOffset - offsets[i].BlockOffset
		}
		var szBuf [4]byte
		binary.LittleEndian.PutUint32(szBuf[:], uint32(compSize))
		buf.Write(szBuf[:])
	}
	return buf.Bytes()
}