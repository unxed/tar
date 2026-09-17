package tar

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// updaterTestArchive writes an archive of first.txt with the given method and
// an embedded index, and returns its path.
func updaterTestArchive(t *testing.T, name string, method uint16) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "first.txt")
	if err := os.WriteFile(src, []byte("first-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	a, err := NewArchiver(path, dir, WithArchiverMethod(method), WithArchiverEmbeddedIndex(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Archive(context.Background(), map[string]os.FileInfo{src: fi}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func updaterTestAppend(t *testing.T, path string, entries ...func(u *Updater) error) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	u, err := NewUpdater(f, APPEND_MODE_OVERWRITE)
	if err != nil {
		t.Fatalf("NewUpdater: %v", err)
	}
	for _, e := range entries {
		if err := e(u); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := u.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// updaterTestList reads the tar stream r with the standard library's reader,
// which stops at the first end-of-archive marker as tar and 7-Zip do.
func updaterTestList(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	got := make(map[string]string)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		if _, dup := got[hdr.Name]; dup {
			t.Errorf("%s is in the archive twice", hdr.Name)
		}
		got[hdr.Name] = string(data)
	}
}

func updaterTestEntries(u *Updater) error {
	if err := u.Append("second.txt", 14, []byte("second-content")); err != nil {
		return err
	}
	return u.Append("first.txt", 7, []byte("renewed"))
}

// TestUpdater_AppendSeenByStandardReaders: entries appended to an archive with
// an embedded index are read by a reader that stops at end-of-archive, and an
// appended name replaces the entry it names. An uncompressed archive used to
// get them after its index, and a compressed one after its end-of-archive
// marker, where only this package's reader looked.
func TestUpdater_AppendSeenByStandardReaders(t *testing.T) {
	want := map[string]string{"first.txt": "renewed", "second.txt": "second-content"}

	t.Run("uncompressed", func(t *testing.T) {
		path := updaterTestArchive(t, "store.tar", Store)
		updaterTestAppend(t, path, updaterTestEntries)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if got := updaterTestList(t, f); !mapsEqual(got, want) {
			t.Errorf("the archive reads as %v, want %v", got, want)
		}
	})

	t.Run("zstd", func(t *testing.T) {
		path := updaterTestArchive(t, "comp.tar.zst", ZSTD)
		updaterTestAppend(t, path, updaterTestEntries)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		zr, err := zstd.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		if got := updaterTestList(t, zr); !mapsEqual(got, want) {
			t.Errorf("the archive reads as %v, want %v", got, want)
		}
	})
}

// TestUpdater_AppendHeader: a directory and a symbolic link go in as what they
// are, and a header with no data is not read from.
func TestUpdater_AppendHeader(t *testing.T) {
	path := updaterTestArchive(t, "store.tar", Store)
	updaterTestAppend(t, path, func(u *Updater) error {
		if err := u.AppendHeader(&Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}, nil); err != nil {
			return err
		}
		return u.AppendHeader(&Header{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "../first.txt", Mode: 0o777}, nil)
	})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	types := make(map[string]byte)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types[hdr.Name] = hdr.Typeflag
		if hdr.Name == "dir/link" && hdr.Linkname != "../first.txt" {
			t.Errorf("dir/link points at %q", hdr.Linkname)
		}
	}
	if types["dir/"] != tar.TypeDir || types["dir/link"] != tar.TypeSymlink || types["first.txt"] != tar.TypeReg {
		t.Errorf("entry types %v", types)
	}
}

// TestUpdater_RefusesDataAfterEnd: an uncompressed archive with something
// after its end-of-archive marker is not cut back to append.
func TestUpdater_RefusesDataAfterEnd(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("something that is not a zero block")
	path := filepath.Join(t.TempDir(), "trailing.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := NewUpdater(f, APPEND_MODE_OVERWRITE); !errors.Is(err, ErrDataAfterEnd) {
		t.Fatalf("NewUpdater gave %v, want ErrDataAfterEnd", err)
	}
	if fi, _ := f.Stat(); fi.Size() != int64(buf.Len()) {
		t.Errorf("the archive was changed to %d bytes", fi.Size())
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
