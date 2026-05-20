package tar

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sync/errgroup"
)

type ExtractorOption func(*extractorOptions) error

type extractorOptions struct {
	concurrency           int
	chownErrorHandler     func(name string, err error) error
	maxFileSize           int64
	maxDecompressionRatio int64
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
}

func NewExtractor(filename, chroot string, opts ...ExtractorOption) (*Extractor, error) {
	rc, err := OpenReader(filename)
	if err != nil {
		return nil, err
	}

	if chroot, err = filepath.Abs(chroot); err != nil {
		rc.Close()
		return nil, err
	}

	e := &Extractor{
		rc:     rc,
		chroot: chroot,
		options: extractorOptions{
			concurrency:           runtime.GOMAXPROCS(0),
			maxFileSize:           1024 * 1024 * 1024, // 1GB default
			maxDecompressionRatio: 200,                // 200:1 default
		},
	}

	for _, o := range opts {
		o(&e.options)
	}

	return e, nil
}

func (e *Extractor) Close() error {
	return e.rc.Close()
}

// Extract reads TAR sequentially but delegates disk I/O and chmod/chown to a worker pool.
func (e *Extractor) Extract(ctx context.Context) error {
	limiter := make(chan struct{}, e.options.concurrency)
	wg, ctx := errgroup.WithContext(ctx)

	// Directories and their attributes to apply after all files are extracted.
	dirs := make(map[string]*Header)
	var links []*Header

	for {
		hdr, err := e.rc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		path, err := filepath.Abs(filepath.Join(e.chroot, hdr.Name))
		if err != nil {
			return err
		}

		if !strings.HasPrefix(path, e.chroot+string(filepath.Separator)) && path != e.chroot {
			return fmt.Errorf("%s cannot be extracted outside of chroot (%s)", path, e.chroot)
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		switch hdr.Typeflag {
		case TypeDir:
			os.MkdirAll(path, 0777)
			dirs[path] = hdr

		case TypeSymlink, TypeLink:
			// Store links and resolve them strictly after extracting regular files to avoid race conditions.
			links = append(links, hdr)
			continue

		case TypeChar, TypeBlock, TypeFifo:
			wg.Go(func() error {
				err := extractSpecialFile(path, hdr)
				if err != nil {
					return err
				}

				err = lchown(path, hdr.Uid, hdr.Gid)
				if err != nil && e.options.chownErrorHandler != nil {
					err = e.options.chownErrorHandler(path, err)
				}
				return err
			})

		case TypeReg, TypeRegA:
			os.MkdirAll(filepath.Dir(path), 0777)

			// Protection against ZIP/TAR bombs: check header size and actual data read
			if e.options.maxFileSize > 0 && hdr.Size > e.options.maxFileSize {
				return fmt.Errorf("tar: file %q size %d exceeds limit %d", hdr.Name, hdr.Size, e.options.maxFileSize)
			}

			// We must read the file sequentially from the stream.
			var data []byte
			if hdr.Size > 0 {
				lr := io.LimitReader(e.rc, hdr.Size)
				data, err = io.ReadAll(lr)
				if err != nil {
					return err
				}

				if e.options.maxDecompressionRatio > 0 && int64(len(data)) > e.options.maxDecompressionRatio*hdr.Size {
					return fmt.Errorf("tar: file %q suspicious compression ratio", hdr.Name)
				}
			}

			limiter <- struct{}{}
			wg.Go(func() error {
				defer func() { <-limiter }()

				f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
				if err != nil {
					return err
				}

				if len(data) > 0 {
					_, err = io.Copy(f, bytes.NewReader(data))
				}
				f.Close()

				if err != nil {
					return err
				}

				lchtimes(path, hdr.AccessTime, hdr.ModTime)
				os.Chmod(path, os.FileMode(hdr.Mode))

				err = lchown(path, hdr.Uid, hdr.Gid)
				if err != nil && e.options.chownErrorHandler != nil {
					err = e.options.chownErrorHandler(path, err)
				}
				return err
			})
		}
	}

	if err := wg.Wait(); err != nil {
		return err
	}

	// Restore symlinks and hardlinks in the second phase
	for _, hdr := range links {
		path, err := filepath.Abs(filepath.Join(e.chroot, hdr.Name))
		if err != nil {
			return err
		}
		os.Remove(path) // Ignore error
		if hdr.Typeflag == TypeSymlink {
			if err := os.Symlink(hdr.Linkname, path); err != nil {
				return err
			}
		} else {
			targetPath := filepath.Join(e.chroot, hdr.Linkname)
			if err := os.Link(targetPath, path); err != nil {
				return err
			}
		}
		lchtimes(path, hdr.AccessTime, hdr.ModTime)
		os.Chmod(path, os.FileMode(hdr.Mode))
		err = lchown(path, hdr.Uid, hdr.Gid)
		if err != nil && e.options.chownErrorHandler != nil {
			err = e.options.chownErrorHandler(path, err)
		}
		if err != nil {
			return err
		}
	}

	// Apply directory times and permissions
	for path, hdr := range dirs {
		lchtimes(path, hdr.AccessTime, hdr.ModTime)
		os.Chmod(path, os.FileMode(hdr.Mode))

		err := lchown(path, hdr.Uid, hdr.Gid)
		if err != nil && e.options.chownErrorHandler != nil {
			err = e.options.chownErrorHandler(path, err)
		}
		if err != nil {
			return err
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