package tar

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMultiCloser_NoError verifies that closing a set of closers that all
// succeed reports no error.
func TestMultiCloser_NoError(t *testing.T) {
	var calls int
	mc := multiCloser{
		closerFunc(func() error { calls++; return nil }),
		closerFunc(func() error { calls++; return nil }),
	}
	if err := mc.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("expected both closers to be called, got %d calls", calls)
	}
}

// TestMultiCloser_ClosesAllAndReturnsFirstError verifies that multiCloser
// closes every member (even after one fails) and surfaces the first error.
func TestMultiCloser_ClosesAllAndReturnsFirstError(t *testing.T) {
	errFirst := errors.New("first error")
	errSecond := errors.New("second error")
	var closed []int
	mc := multiCloser{
		closerFunc(func() error { closed = append(closed, 1); return errFirst }),
		closerFunc(func() error { closed = append(closed, 2); return errSecond }),
		closerFunc(func() error { closed = append(closed, 3); return nil }),
	}
	err := mc.Close()
	if !errors.Is(err, errFirst) {
		t.Fatalf("Close() = %v, want %v (first error wins)", err, errFirst)
	}
	if len(closed) != 3 {
		t.Fatalf("expected all 3 closers to run, got %v", closed)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestFileInfo_RegularFile checks fileInfo's fs.FileInfo implementation for
// a plain regular file node: no dir/symlink mode bits, plumbing straight
// through to the underlying FileNode.
func TestFileInfo_RegularFile(t *testing.T) {
	mt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	node := &FileNode{
		Path:    "/some/dir",
		Name:    "file.txt",
		Size:    12345,
		Mode:    0644,
		ModTime: mt,
		Type:    TypeReg,
	}
	fi := fileInfo{node}

	if fi.Name() != "file.txt" {
		t.Errorf("Name() = %q, want %q", fi.Name(), "file.txt")
	}
	if fi.Size() != 12345 {
		t.Errorf("Size() = %d, want %d", fi.Size(), 12345)
	}
	if !fi.ModTime().Equal(mt) {
		t.Errorf("ModTime() = %v, want %v", fi.ModTime(), mt)
	}
	if fi.Sys() != node {
		t.Errorf("Sys() did not return the underlying *FileNode")
	}
	if fi.IsDir() {
		t.Error("IsDir() = true for a regular file, want false")
	}
	mode := fi.Mode()
	if mode&fs.ModeDir != 0 {
		t.Errorf("Mode() has ModeDir set for a regular file: %v", mode)
	}
	if mode&fs.ModeSymlink != 0 {
		t.Errorf("Mode() has ModeSymlink set for a regular file: %v", mode)
	}
	if mode.Perm() != 0644 {
		t.Errorf("Mode().Perm() = %v, want %v", mode.Perm(), fs.FileMode(0644))
	}
}

// TestFileInfo_Directory checks that a node explicitly typed as TypeDir, and
// separately the synthetic archive-root node (Path "/", Name ""), both
// report as directories.
func TestFileInfo_Directory(t *testing.T) {
	dirNode := &FileNode{Name: "sub", Type: TypeDir, Mode: 0755}
	if fi := (fileInfo{dirNode}); !fi.IsDir() {
		t.Error("IsDir() = false for TypeDir node, want true")
	}

	rootNode := &FileNode{Path: "/", Name: "", Type: TypeReg}
	if fi := (fileInfo{rootNode}); !fi.IsDir() {
		t.Error("IsDir() = false for synthetic root node (Path=/, Name=\"\"), want true")
	}
}

// TestFileInfo_SymlinkAndHardlink checks that both TypeSymlink and TypeLink
// nodes get fs.ModeSymlink set on their reported mode.
func TestFileInfo_SymlinkAndHardlink(t *testing.T) {
	for _, typ := range []byte{TypeSymlink, TypeLink} {
		node := &FileNode{Name: "link", Type: typ, Mode: 0777}
		fi := fileInfo{node}
		if fi.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("Mode() for Type=%q missing ModeSymlink", typ)
		}
		if fi.IsDir() {
			t.Errorf("Mode() for Type=%q unexpectedly reports IsDir", typ)
		}
	}
}

// TestDirFile_Read verifies that reading directly from a directory handle
// (rather than using ReadDir) returns the documented error instead of data.
func TestDirFile_Read(t *testing.T) {
	d := &dirFile{node: &FileNode{Name: "adir", Type: TypeDir}}
	n, err := d.Read(make([]byte, 16))
	if n != 0 {
		t.Errorf("Read() n = %d, want 0", n)
	}
	if err == nil {
		t.Fatal("Read() err = nil, want a PathError")
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("Read() err = %v (%T), want *fs.PathError", err, err)
	}
	if pe.Op != "read" || pe.Path != "adir" {
		t.Errorf("PathError = %+v, want Op=read Path=adir", pe)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// TestGetCacheIndexPath_HomeUnset covers the error branch taken on
// non-Windows when neither XDG_CACHE_HOME nor HOME is set: GetCacheIndexPath
// cannot determine a cache directory and must return an error.
func TestGetCacheIndexPath_HomeUnset(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")

	_, err := GetCacheIndexPath("/some/archive.tar")
	if err == nil {
		t.Fatal("expected an error when neither XDG_CACHE_HOME nor HOME is set")
	}
	if !strings.Contains(err.Error(), "home directory") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestGetCacheIndexPath_MkdirAllFails covers the branch where the computed
// cache directory can't actually be created (a path component collides with
// an existing regular file).
func TestGetCacheIndexPath_MkdirAllFails(t *testing.T) {
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	// cacheDir will be "<blocker>/ratarmount", which os.MkdirAll cannot
	// create because "blocker" already exists as a regular file.
	t.Setenv("XDG_CACHE_HOME", blocker)

	_, err := GetCacheIndexPath("/some/archive.tar")
	if err == nil {
		t.Fatal("expected an error when the cache directory can't be created")
	}
}

// TestGetCacheIndexPath_Success verifies the happy path on the non-Windows
// branch, including the archive-path-to-filename sanitization (path
// separators and colons become underscores, with a leading underscore
// trimmed off).
func TestGetCacheIndexPath_Success(t *testing.T) {
	tmpDir := t.TempDir()
	xdgCache := filepath.Join(tmpDir, "xdgcache")
	t.Setenv("XDG_CACHE_HOME", xdgCache)

	archivePath := "/data/my:archive.tar"
	got, err := GetCacheIndexPath(archivePath)
	if err != nil {
		t.Fatalf("GetCacheIndexPath: %v", err)
	}

	wantDir := filepath.Join(xdgCache, "ratarmount")
	if filepath.Dir(got) != wantDir {
		t.Errorf("cache path dir = %q, want %q", filepath.Dir(got), wantDir)
	}
	base := filepath.Base(got)
	if strings.ContainsAny(base, ":/\\") {
		t.Errorf("sanitized filename %q still contains a path separator or colon", base)
	}
	if strings.HasPrefix(base, "_") {
		t.Errorf("sanitized filename %q still has a leading underscore", base)
	}
	if !strings.HasSuffix(base, ".sqlite") {
		t.Errorf("sanitized filename %q does not end in .sqlite", base)
	}

	if fi, err := os.Stat(wantDir); err != nil || !fi.IsDir() {
		t.Errorf("expected cache directory %q to have been created", wantDir)
	}
}
