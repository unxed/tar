package tar

import (
	"context"
	"encoding/base64"
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
	wc            *WriteCloser
	options       archiverOptions
	m             sync.Mutex
	seenHardLinks map[hardlinkKey]string
}

func NewArchiver(filename string, chroot string, opts ...ArchiverOption) (*Archiver, error) {
	var err error
	if chroot, err = filepath.Abs(chroot); err != nil {
		return nil, err
	}

	a := &Archiver{
		options: archiverOptions{
			method: Store,
			chroot: chroot,
		},
		seenHardLinks: make(map[hardlinkKey]string),
	}

	for _, o := range opts {
		if err := o(&a.options); err != nil {
			return nil, err
		}
	}

	var wopts []WriterOption
	if a.options.indexPath != "" {
		wopts = append(wopts, WithWriterIndex(a.options.indexPath))
	}

	wc, err := CreateWriter(filename, a.options.method, wopts...)
	if err != nil {
		return nil, err
	}
	a.wc = wc
	return a, nil
}

func (a *Archiver) Close() error {
	return a.wc.Close()
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