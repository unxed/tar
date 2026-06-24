package tar

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sync/errgroup"
)

type ExtractorOption func(*extractorOptions) error

type extractorOptions struct {
	concurrency           int
	chownErrorHandler     func(name string, err error) error
	maxFileSize           int64
	maxDecompressionRatio int64
	keepOldFiles          bool
	keepNewerFiles        bool
	noTimes               bool
	stripComponents       int
	xattrs                bool
	incremental           bool
	sparse                bool
	safeWrites            bool
	unlinkFirst           bool
	numericOwner          bool
	keepBroken            bool
	tolerant              bool
	password              string
}

// WithExtractorSafeWrites extracts files atomically by writing to a temporary file and renaming (--safe-writes).
// WithExtractorPassword provides the password for decrypting F4Crypt encrypted archives.
func WithExtractorPassword(p string) ExtractorOption {
	return func(o *extractorOptions) error {
		o.password = p
		return nil
	}
}
func WithExtractorSafeWrites(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.safeWrites = b
		return nil
	}
}

// WithExtractorUnlinkFirst removes existing files prior to extracting over them (-U, --unlink-first).
func WithExtractorUnlinkFirst(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.unlinkFirst = b
		return nil
	}
}

// WithExtractorNumericOwner always uses numeric user/group IDs from the archive rather than resolving Uname/Gname (--numeric-owner).
func WithExtractorNumericOwner(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.numericOwner = b
		return nil
	}
}
func WithExtractorKeepBroken(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.keepBroken = b
		return nil
	}
}

func WithExtractorTolerant(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.tolerant = b
		return nil
	}
}

// WithExtractorSparse enables extracting files as sparse files by seeking over zero-blocks (-S, --sparse).
func WithExtractorSparse(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.sparse = b
		return nil
	}
}

var sparseBufCh = make(chan []byte, 64)

func getSparseBuf() []byte {
	select {
	case b := <-sparseBufCh:
		return b
	default:
		return make([]byte, 1024*1024)
	}
}

func putSparseBuf(b []byte) {
	select {
	case sparseBufCh <- b:
	default:
	}
}

func isAllZeros(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	if p[0] != 0 {
		return false
	}
	// Highly optimized SIMD-comparison via standard Go runtime bytealg
	return len(p) == 1 || p[0] == p[1] && bytes.Equal(p[:len(p)-1], p[1:])
}

func copySparseBytes(dst *os.File, data []byte) error {
	var offset int
	blockSize := 256 * 1024 // 256KB optimal size for in-memory zero checking
	for offset < len(data) {
		end := offset + blockSize
		if end > len(data) {
			end = len(data)
		}
		block := data[offset:end]
		if isAllZeros(block) {
			if _, err := dst.Seek(int64(len(block)), io.SeekCurrent); err != nil {
				return err
			}
		} else {
			if _, err := dst.Write(block); err != nil {
				return err
			}
		}
		offset = end
	}
	return dst.Truncate(int64(len(data)))
}

func copySparse(dst *os.File, src io.Reader, size int64) error {
	buf := getSparseBuf()
	defer putSparseBuf(buf)

	var written int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if isAllZeros(buf[:n]) {
				_, seekErr := dst.Seek(int64(n), io.SeekCurrent)
				if seekErr != nil {
					return seekErr
				}
			} else {
				_, wErr := dst.Write(buf[:n])
				if wErr != nil {
					return wErr
				}
			}
			written += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return dst.Truncate(size)
}

// WithExtractorIncremental enables processing of GNU Dumpdir headers to remove deleted files during incremental restores.
func WithExtractorIncremental(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.incremental = b
		return nil
	}
}

// WithExtractorXattrs enables restoration of extended attributes (xattrs, POSIX ACLs, SELinux).
func WithExtractorXattrs(b bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.xattrs = b
		return nil
	}
}

func WithExtractorConcurrency(n int) ExtractorOption {
	return func(o *extractorOptions) error {
		o.concurrency = n
		return nil
	}
}

func WithExtractorMaxFileSize(n int64) ExtractorOption {
	return func(o *extractorOptions) error {
		o.maxFileSize = n
		return nil
	}
}
func WithExtractorMaxRatio(n int64) ExtractorOption {
	return func(o *extractorOptions) error {
		o.maxDecompressionRatio = n
		return nil
	}
}
// WithExtractorKeepOldFiles prevents overwriting existing files (-k or --keep-old-files)
func WithExtractorKeepOldFiles(keep bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.keepOldFiles = keep
		return nil
	}
}

// WithExtractorKeepNewerFiles prevents overwriting files that are newer on disk (--keep-newer-files)
func WithExtractorKeepNewerFiles(keep bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.keepNewerFiles = keep
		return nil
	}
}
// WithExtractorNoTimes prevents restoring original modification times (-m / --touch)
func WithExtractorNoTimes(noTimes bool) ExtractorOption {
	return func(o *extractorOptions) error {
		o.noTimes = noTimes
		return nil
	}
}

// WithExtractorStripComponents strips the specified number of leading components from file names on extraction (--strip-components)
func WithExtractorStripComponents(count int) ExtractorOption {
	return func(o *extractorOptions) error {
		o.stripComponents = count
		return nil
	}
}

func stripComponents(name string, count int) (string, bool) {
	cleaned := filepath.ToSlash(filepath.Clean(name))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." || cleaned == "" {
		return "", false
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) <= count {
		return "", false
	}
	return strings.Join(parts[count:], "/"), true
}

// WithExtractorChownErrorHandler allows gracefully ignoring chown errors
// which frequently happen when extracting archives as an unprivileged user.
func WithExtractorChownErrorHandler(fn func(name string, err error) error) ExtractorOption {
	return func(o *extractorOptions) error {
		o.chownErrorHandler = fn
		return nil
	}
}

type Extractor struct {
	rc      *ReadCloser
	chroot  string
	options extractorOptions
	written int64
	entries int64
}

func (e *Extractor) Written() (bytes, entries int64) {
	return atomic.LoadInt64(&e.written), atomic.LoadInt64(&e.entries)
}

func NewExtractor(filename, chroot string, opts ...ExtractorOption) (*Extractor, error) {
	var err error
	if chroot, err = filepath.Abs(chroot); err != nil {
		return nil, err
	}

	e := &Extractor{
		chroot: chroot,
		options: extractorOptions{
			concurrency:           runtime.GOMAXPROCS(0),
			maxFileSize:           0,   // unlimited
			maxDecompressionRatio: 500, // 500:1 is a safe default for most data
			xattrs:                true,
			chownErrorHandler: func(name string, err error) error {
				if pe, ok := err.(*os.PathError); ok {
					if errno, ok := pe.Err.(syscall.Errno); ok && errno == syscall.EPERM {
						return nil
					}
				}
				fmt.Fprintf(os.Stderr, "tar: %s: %v (continuing)\n", name, err)
				return nil
			},
		},
	}

	for _, o := range opts {
		o(&e.options)
	}

	rc, err := openReaderWithPassword(filename, e.options.password)
	if err != nil {
		return nil, err
	}
	e.rc = rc

	return e, nil
}

func (e *Extractor) Close() error {
	return e.rc.Close()
}

// Extract reads TAR sequentially but delegates disk I/O and chmod/chown to a worker pool.
func (e *Extractor) Extract(ctx context.Context) error {
	parentCtx := ctx
	limiter := make(chan struct{}, e.options.concurrency)
	wg, ctx := errgroup.WithContext(parentCtx)

	// Directories and their attributes to apply after all files are extracted.
	dirs := make(map[string]*Header)
	var links []*Header

	mainErr := func() error {
		for {
			hdr, err := e.rc.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

		name := hdr.Name
		if strings.HasPrefix(name, MappedStringMarkStr) {
			name = string(encodeMappedString(name))
		}
		if e.options.stripComponents > 0 {
			stripped, ok := stripComponents(name, e.options.stripComponents)
			if !ok {
				continue // Skip file with fewer or equal components
			}
			name = stripped
		}

		path, err := filepath.Abs(filepath.Join(e.chroot, name))
		if err != nil {
			return err
		}

		prefix := e.chroot
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if !strings.HasPrefix(path, prefix) && path != e.chroot {
			return fmt.Errorf("%s cannot be extracted outside of chroot (%s)", path, e.chroot)
		}

		if err := e.linksToDirs(path); err != nil {
			return err
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Overwrite control policies (GNU/BSD tar compatibility)
		if hdr.Typeflag != TypeDir && hdr.Typeflag != TypeXGlobalHeader && hdr.Typeflag != TypeVol {
			if e.options.unlinkFirst {
				os.Remove(path) // Unconditionally remove before extraction
			}
			if e.options.keepOldFiles {
				if _, err := os.Stat(path); err == nil {
					continue // Skip extracting, file already exists
				}
			}
			if e.options.keepNewerFiles {
				if fi, err := os.Stat(path); err == nil {
					if fi.ModTime().After(hdr.ModTime) {
						continue // Skip extracting, disk file is newer
					}
				}
			}
		}

		switch hdr.Typeflag {
		case TypeXGlobalHeader, TypeVol:
			// Ignore global extended headers and volume labels
			continue

		case TypeGNUDumpDir:
			e.synthesizeDir(path)
			dirs[path] = hdr

			if e.options.incremental && hdr.Size > 0 {
				data, err := io.ReadAll(e.rc)
				if err != nil {
					return err
				}
				// GNU dumpdir list is [tag byte][filename]\0, terminated by an extra \0
				validNames := make(map[string]bool)
				for i := 0; i < len(data); {
					if data[i] == 0 {
						break
					}
					start := i + 1 // skip the tag byte (Y/N/D)
					end := start
					for end < len(data) && data[end] != 0 {
						end++
					}
					if end > start {
						validNames[string(data[start:end])] = true
					}
					i = end + 1
				}

				entries, err := os.ReadDir(path)
				if err == nil {
					for _, entry := range entries {
						if !validNames[entry.Name()] {
							os.RemoveAll(filepath.Join(path, entry.Name()))
						}
					}
				}
			} else if hdr.Size > 0 {
				buf := getSparseBuf()
				io.CopyBuffer(io.Discard, e.rc, buf)
				putSparseBuf(buf)
			}
			continue

		case TypeGNUMultiVol:
			// Wait for all previous asynchronous writes to complete to avoid races
			if err := wg.Wait(); err != nil {
				return err
			}
			// Reset wg with parentCtx
			wg, ctx = errgroup.WithContext(parentCtx)

			e.synthesizeDir(filepath.Dir(path))
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if hdr.Size > 0 {
				buf := getSparseBuf()
				_, err = io.CopyBuffer(f, e.rc, buf)
				putSparseBuf(buf)
			}
			f.Close()
			if err != nil {
				return err
			}
			continue

		case TypeDir:
			e.synthesizeDir(path)
			dirs[path] = hdr
			atomic.AddInt64(&e.entries, 1)

		case TypeSymlink, TypeLink:
			// Store links and resolve them strictly after extracting regular files to avoid race conditions.
			links = append(links, hdr)
			continue

		case TypeChar, TypeBlock, TypeFifo:
			e.synthesizeDir(filepath.Dir(path))
			wg.Go(func() error {
				err := extractSpecialFile(path, hdr)
				if err != nil {
					return err
				}

				if e.options.xattrs {
					applyXattrs(path, hdr)
				}

				uid, gid := resolveIds(hdr, e.options.numericOwner)
				err = lchown(path, uid, gid)
				if err != nil && e.options.chownErrorHandler != nil {
					err = e.options.chownErrorHandler(path, err)
				}
				if err != nil && e.options.tolerant {
					fmt.Printf("tar: skipping corrupted special file %q: %v\n", hdr.Name, err)
					return nil
				}
				atomic.AddInt64(&e.entries, 1)
				return err
			})

		case TypeReg, TypeRegA:
			e.synthesizeDir(filepath.Dir(path))

			// Protection against bombs
			if e.options.maxFileSize > 0 && hdr.Size > e.options.maxFileSize {
				return fmt.Errorf("tar: file %q size %d exceeds limit %d", hdr.Name, hdr.Size, e.options.maxFileSize)
			}

			const memBufferLimit = 16 * 1024 * 1024 // 16MB threshold

			if hdr.Size <= memBufferLimit {
				// Small files: read into memory and delegate I/O to worker pool
				var data []byte
				if hdr.Size > 0 {
					data = make([]byte, hdr.Size)
					if _, err = io.ReadFull(e.rc, data); err != nil {
						return err
					}
				}

				if strings.HasSuffix(hdr.Name, ":Zone.Identifier") && len(data) > 0 {
					data = sanitizeZoneIdentifier(data)
					hdr.Size = int64(len(data))
				}

				limiter <- struct{}{}
				h, p, _ := *hdr, path, data // Local copies for worker
				wg.Go(func() error {
					defer func() { <-limiter }()

					writePath := p
					hdr := &h
					if e.options.safeWrites {
						writePath = path + ".tmp"
					}

					f, err := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
					if err != nil {
						return err
					}

					cleanup := true
					defer func() {
						f.Close()
						if err != nil && cleanup && !e.options.keepBroken {
							os.Remove(writePath)
						}
					}()

					if err := preallocate(f, hdr.Size); err != nil {
						return err
					}

					if len(data) > 0 {
						if e.options.sparse {
							err = copySparseBytes(f, data)
						} else {
							_, err = io.Copy(f, bytes.NewReader(data))
						}
					}
					f.Close()
					if err != nil {
						return err
					}
					cleanup = false

					if e.options.xattrs {
						applyXattrs(writePath, hdr)
					}
					e.restoreNtfsAcl(writePath, hdr)
					if !e.options.noTimes {
						lchtimes(writePath, hdr.AccessTime, hdr.ModTime)
					}
					os.Chmod(writePath, os.FileMode(hdr.Mode))

					uid, gid := resolveIds(hdr, e.options.numericOwner)
					err = lchown(writePath, uid, gid)
					if err != nil && e.options.chownErrorHandler != nil {
						err = e.options.chownErrorHandler(writePath, err)
					}

					if e.options.safeWrites {
						if rerr := os.Rename(writePath, path); rerr != nil {
							os.Remove(writePath)
							return rerr
						}
					}
					if err != nil && e.options.tolerant {
						fmt.Printf("tar: skipping corrupted file %q: %v\n", hdr.Name, err)
						return nil
					}
					atomic.AddInt64(&e.written, hdr.Size)
					atomic.AddInt64(&e.entries, 1)
					return err
				})
			} else {
				// Large files: stream sequentially in the main loop to prevent OOM
				writePath := path
				if e.options.safeWrites {
					writePath = path + ".tmp"
				}

				f, err := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
				if err != nil {
					return err
				}

				cleanup := true
				defer func() {
					f.Close()
					if err != nil && cleanup && !e.options.keepBroken {
						os.Remove(writePath)
					}
				}()

				var r io.Reader = e.rc
				if e.options.maxDecompressionRatio > 0 && hdr.Size > 0 {
					// Logic: If the ratio of header.Size to the actual data in the stream
					// is too high, it's suspicious.
					// But better: we check how much we've written vs what's expected.
					// Since we use archive/tar, we can't easily see "compressed" size here,
					// but we can enforce the limit from the header.
				}

				if e.options.sparse {
					err = copySparse(f, r, hdr.Size)
				} else {
					if err := preallocate(f, hdr.Size); err != nil {
						return err
					}
					// Переиспользуем наш гигантский 1МБ пул буферов вместо дефолтных 32КБ
					buf := getSparseBuf()
					_, err = io.CopyBuffer(f, r, buf)
					putSparseBuf(buf)
				}
				if err != nil {
					return err
				}
				cleanup = false

				// Apply metadata in background to save time
				limiter <- struct{}{}
				h, p := *hdr, writePath // Local copies for worker
				wg.Go(func() error {
					defer func() { <-limiter }()
					hdr := &h
					writePath := p
					if e.options.xattrs {
						applyXattrs(writePath, hdr)
					}
					e.restoreNtfsAcl(writePath, hdr)
					if !e.options.noTimes {
						lchtimes(writePath, hdr.AccessTime, hdr.ModTime)
					}
					os.Chmod(writePath, os.FileMode(hdr.Mode))

					uid, gid := resolveIds(hdr, e.options.numericOwner)
					err := lchown(writePath, uid, gid)
					if err != nil && e.options.chownErrorHandler != nil {
						err = e.options.chownErrorHandler(writePath, err)
					}

					if e.options.safeWrites {
						if rerr := os.Rename(writePath, path); rerr != nil {
							os.Remove(writePath)
							return rerr
						}
					}
					if err != nil && e.options.tolerant {
						fmt.Printf("tar: skipping corrupted file %q: %v\n", hdr.Name, err)
						return nil
					}
					atomic.AddInt64(&e.written, hdr.Size)
					atomic.AddInt64(&e.entries, 1)
					return err
				})
			}
		}
		}
		return nil
	}()

	waitErr := wg.Wait()
	if mainErr != nil {
		return mainErr
	}
	if waitErr != nil {
		return waitErr
	}

	// Restore symlinks and hardlinks in the second phase
	for _, hdr := range links {
		hdrName := hdr.Name
		if strings.HasPrefix(hdrName, MappedStringMarkStr) {
			hdrName = string(encodeMappedString(hdrName))
		}
		path, err := filepath.Abs(filepath.Join(e.chroot, hdrName))
		if err != nil {
			return err
		}
		os.Remove(path) // Ignore error
		hdrLinkname := hdr.Linkname
		if strings.HasPrefix(hdrLinkname, MappedStringMarkStr) {
			hdrLinkname = string(encodeMappedString(hdrLinkname))
		}
		if hdr.Typeflag == TypeSymlink {
			if runtime.GOOS == "windows" {
				isDir := false
				if fi, err := os.Stat(filepath.Join(filepath.Dir(path), hdrLinkname)); err == nil {
					isDir = fi.IsDir()
				}
				if err := createWindowsSymlink(hdrLinkname, path, isDir); err != nil {
					return err
				}
			} else {
				if err := os.Symlink(hdrLinkname, path); err != nil {
					return err
				}
			}
		} else {
			targetPath := filepath.Join(e.chroot, hdrLinkname)
			if err := os.Link(targetPath, path); err != nil {
				return err
			}
		}
		if e.options.xattrs {
			applyXattrs(path, hdr)
		}
		e.restoreNtfsAcl(path, hdr)
		if !e.options.noTimes {
			lchtimes(path, hdr.AccessTime, hdr.ModTime)
		}
		os.Chmod(path, os.FileMode(hdr.Mode))
		uid, gid := resolveIds(hdr, e.options.numericOwner)
		err = lchown(path, uid, gid)
		if err != nil && e.options.chownErrorHandler != nil {
			err = e.options.chownErrorHandler(path, err)
		}
		if err != nil {
			return err
		}
	}

	// Apply directory times and permissions
	for path, hdr := range dirs {
		if e.options.xattrs {
			applyXattrs(path, hdr)
		}
		e.restoreNtfsAcl(path, hdr)
		if !e.options.noTimes {
			lchtimes(path, hdr.AccessTime, hdr.ModTime)
		}
		os.Chmod(path, os.FileMode(hdr.Mode))

		uid, gid := resolveIds(hdr, e.options.numericOwner)
		err := lchown(path, uid, gid)
		if err != nil && e.options.chownErrorHandler != nil {
			err = e.options.chownErrorHandler(path, err)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *Extractor) restoreNtfsAcl(path string, hdr *Header) {
	if len(hdr.PAXRecords) > 0 {
		if rawSD, ok := hdr.PAXRecords["MSWINDOWS.raw_sd"]; ok {
			if acl, err := base64.StdEncoding.DecodeString(rawSD); err == nil {
				applyNtfsAclFunc(path, acl)
			}
		}
	}
}

func (e *Extractor) linksToDirs(targetPath string) error {
	if !strings.HasPrefix(targetPath, e.chroot) {
		return nil
	}
	rel, err := filepath.Rel(e.chroot, targetPath)
	if err != nil {
		return err
	}
	if rel == "." || rel == "" {
		return nil
	}

	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	current := e.chroot
	for i := 0; i < len(parts)-1; i++ {
		current = filepath.Join(current, parts[i])
		fi, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(current); err != nil {
				return err
			}
		}
	}
	return nil
}
// synthesizeDir guarantees that a directory exists on disk,
// recovering missing structures and resolving file/directory conflicts on the fly.
func (e *Extractor) synthesizeDir(targetDir string) error {
	err := os.MkdirAll(targetDir, 0755)
	if err == nil {
		return nil
	}

	if !strings.HasPrefix(targetDir, e.chroot) {
		return nil
	}
	rel, errRel := filepath.Rel(e.chroot, targetDir)
	if errRel != nil {
		return errRel
	}

	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	current := e.chroot

	for i := 0; i < len(parts); i++ {
		current = filepath.Join(current, parts[i])
		fi, errStat := os.Lstat(current)
		if errStat != nil {
			if os.IsNotExist(errStat) {
				if errMk := os.Mkdir(current, 0755); errMk != nil && !os.IsExist(errMk) {
					return errMk
				}
			} else {
				return errStat
			}
		} else if !fi.IsDir() {
			if errRm := os.Remove(current); errRm == nil {
				if errMk := os.Mkdir(current, 0755); errMk != nil {
					return errMk
				}
			} else {
				return errRm
			}
		}
	}
	return nil
}

// Convenience top-level API compatible with targz package
func Extract(inputFilePath, outputFilePath string) error {
	e, err := NewExtractor(inputFilePath, outputFilePath)
	if err != nil {
		return err
	}
	defer e.Close()
	return e.Extract(context.Background())
}