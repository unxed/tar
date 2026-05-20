package tar

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"strings"
	"time"
)

// TestMultiCompressions verifies round-trip compression and extraction for all supported methods.
func TestMultiCompressions(t *testing.T) {
	methods := []uint16{Store, GZIP, XZ, ZSTD}

	for _, method := range methods {
		t.Run(fmt.Sprintf("Method_%d", method), func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			os.MkdirAll(srcDir, 0755)
			os.WriteFile(filepath.Join(srcDir, "test.txt"), []byte(fmt.Sprintf("data_%d", method)), 0644)

			archivePath := filepath.Join(tmpDir, "archive.tar")
			switch method {
			case GZIP:
				archivePath += ".gz"
			case BZIP2:
				archivePath += ".bz2"
			case XZ:
				archivePath += ".xz"
			case ZSTD:
				archivePath += ".zst"
			}

			a, err := NewArchiver(archivePath, filepath.Dir(srcDir), WithArchiverMethod(method))
			if err != nil {
				t.Fatal(err)
			}

			fi, _ := os.Lstat(srcDir)
			fi2, _ := os.Lstat(filepath.Join(srcDir, "test.txt"))
			files := map[string]os.FileInfo{
				srcDir:                            fi,
				filepath.Join(srcDir, "test.txt"): fi2,
			}

			if err := a.Archive(context.Background(), files); err != nil {
				t.Fatal(err)
			}
			a.Close()

			// Extract
			dstDir := filepath.Join(tmpDir, "dst")
			e, err := NewExtractor(archivePath, dstDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Extract(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.Close()

			data, err := os.ReadFile(filepath.Join(dstDir, "src", "test.txt"))
			if err != nil || string(data) != fmt.Sprintf("data_%d", method) {
				t.Errorf("Roundtrip failed for method %d: got %s, error: %v", method, string(data), err)
			}
		})
	}
}

// TestTarFS_DuplicatesAndSynthesizedDirs tests that TarFS properly synthesizes parent
// directories if they are missing from the archive and always returns the latest
// appended version of duplicate files.
func TestTarFS_DuplicatesAndSynthesizedDirs(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "dup.tar")

	// 1. Manually construct a TAR with:
	//    - "a/b/file.txt" (no directory "a" or "a/b" explicitly defined in TAR headers)
	//    - An appended newer version of "a/b/file.txt"
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	// First version
	hdr := &Header{Name: "a/b/file.txt", Size: 5, Mode: 0644, ModTime: time.Now()}
	tw.WriteHeader(hdr)
	tw.Write([]byte("first"))

	// Second version (appended later)
	hdr2 := &Header{Name: "a/b/file.txt", Size: 6, Mode: 0644, ModTime: time.Now().Add(time.Hour)}
	tw.WriteHeader(hdr2)
	tw.Write([]byte("second"))

	tw.Close()
	f.Close()

	// 2. Index & Open via TarFS
	indexPath := filepath.Join(tmpDir, "dup.tar.index.sqlite")
	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// 3. Verify directory "a" is synthesized as a directory
	entries, err := fs.ReadDir(tfs, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a" || !entries[0].IsDir() {
		t.Errorf("Expected synthesized directory 'a' in root")
	}

	// 4. Verify directory "a/b" is synthesized
	subEntries, err := fs.ReadDir(tfs, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(subEntries) != 1 || subEntries[0].Name() != "file.txt" || subEntries[0].IsDir() {
		t.Errorf("Expected 'file.txt' inside synthesized 'a/b'")
	}

	// 5. Verify the LATEST (second) version of "file.txt" is retrieved
	data, err := fs.ReadFile(tfs, "a/b/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Errorf("Expected to read latest version 'second', got %q", string(data))
	}
}

// TestUnixLinks verifies that symbolic and hard links are correctly preserved and restored.
func TestUnixLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks and hardlinks are not fully supported on Windows")
	}

	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)

	// Create target, symlink, and hardlink
	targetPath := filepath.Join(srcDir, "target.txt")
	if err := os.WriteFile(targetPath, []byte("link_target"), 0644); err != nil {
		t.Fatalf("Failed to create target file: %v", err)
	}

	symPath := filepath.Join(srcDir, "sym.txt")
	if err := os.Symlink("target.txt", symPath); err != nil {
		t.Fatalf("Failed to create symlink: %v", err)
	}

	hardPath := filepath.Join(srcDir, "hard.txt")
	if err := os.Link(targetPath, hardPath); err != nil {
		t.Fatalf("Failed to create hardlink: %v", err)
	}

	archivePath := filepath.Join(tmpDir, "links.tar")
	err := Compress(srcDir, archivePath)
	if err != nil {
		t.Fatal(err)
	}

	// Extract
	dstDir := filepath.Join(tmpDir, "dst")
	err = Extract(archivePath, dstDir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify Symlink
	symInfo, err := os.Lstat(filepath.Join(dstDir, "src", "sym.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if symInfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("sym.txt is not a symlink")
	}
	target, _ := os.Readlink(filepath.Join(dstDir, "src", "sym.txt"))
	if target != "target.txt" {
		t.Errorf("Symlink points to wrong target: %s", target)
	}

	// Verify Hardlink (cross-platform verification via os.SameFile)
	targetStat, _ := os.Stat(filepath.Join(dstDir, "src", "target.txt"))
	hardStat, _ := os.Stat(filepath.Join(dstDir, "src", "hard.txt"))
	if !os.SameFile(targetStat, hardStat) {
		t.Errorf("Hardlink does not point to the same physical file on disk")
	}
}

func TestArchiver_ChrootViolation(t *testing.T) {
	tmpDir := t.TempDir()
	chroot := filepath.Join(tmpDir, "chroot")
	os.MkdirAll(chroot, 0755)

	outsideFile := filepath.Join(tmpDir, "outside.txt")
	os.WriteFile(outsideFile, []byte("outside"), 0644)

	archivePath := filepath.Join(tmpDir, "archive.tar")
	a, err := NewArchiver(archivePath, chroot)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	fi, err := os.Stat(outsideFile)
	if err != nil {
		t.Fatal(err)
	}

	files := map[string]os.FileInfo{
		outsideFile: fi,
	}

	err = a.Archive(context.Background(), files)
	if err == nil {
		t.Error("Expected error when archiving file outside chroot, got nil")
	} else if !strings.Contains(err.Error(), "cannot be archived from outside of chroot") {
		t.Errorf("Unexpected error: %v", err)
	}
}


func TestExtractor_DirectoryPermissionOrdering(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "readonly_dir.tar")
	dstDir := filepath.Join(tmpDir, "extract")
	defer os.Chmod(filepath.Join(dstDir, "restricted_dir"), 0755)

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	// Директория с запретом на запись (0500)
	dirHdr := &Header{
		Name:     "restricted_dir/",
		Typeflag: TypeDir,
		Mode:     0500,
	}
	if err := tw.WriteHeader(dirHdr); err != nil {
		t.Fatal(err)
	}

	// Файл внутри этой директории
	fileHdr := &Header{
		Name:     "restricted_dir/file.txt",
		Typeflag: TypeReg,
		Size:     11,
		Mode:     0644,
	}
	if err := tw.WriteHeader(fileHdr); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("inside file"))

	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error {
		return nil
	})
	e, err := NewExtractor(archivePath, dstDir, ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	err = e.Extract(context.Background())
	if err != nil {
		t.Fatalf("Extraction failed for restricted directory: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "restricted_dir/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "inside file" {
		t.Errorf("Unexpected content: %q", string(data))
	}
}

func TestExtractor_Fifo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO is not supported on Windows")
	}

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "fifo.tar")
	dstDir := filepath.Join(tmpDir, "extract")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	hdr := &Header{
		Name:     "my_fifo",
		Typeflag: TypeFifo,
		Mode:     0600,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error {
		return nil
	})
	e, err := NewExtractor(archivePath, dstDir, ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	err = e.Extract(context.Background())
	if err != nil {
		t.Fatalf("Failed to extract FIFO: %v", err)
	}

	fi, err := os.Lstat(filepath.Join(dstDir, "my_fifo"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Error("Expected extracted file to be a Named Pipe / FIFO")
	}
}

func TestTarFS_ReadDirIncremental(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "paging.tar")
	indexPath := filepath.Join(tmpDir, "paging.sqlite")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	files := []string{"dir/a.txt", "dir/b.txt", "dir/c.txt", "dir/d.txt"}
	for _, name := range files {
		hdr := &Header{Name: name, Size: 4, Mode: 0644}
		tw.WriteHeader(hdr)
		tw.Write([]byte("data"))
	}
	tw.Close()
	f.Close()

	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	dFile, err := tfs.Open("dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dFile.Close()

	rdf, ok := dFile.(fs.ReadDirFile)
	if !ok {
		t.Fatal("Directory file does not implement fs.ReadDirFile")
	}

	// Считываем первые 2 элемента
	entries, err := rdf.ReadDir(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(entries))
	}

	// Запрашиваем 3 элемента (но осталось всего 2)
	entries2, err := rdf.ReadDir(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 2 {
		t.Errorf("Expected remaining 2 entries, got %d", len(entries2))
	}

	// Дальнейший запрос должен возвращать io.EOF
	_, err = rdf.ReadDir(1)
	if err != io.EOF {
		t.Errorf("Expected io.EOF, got %v", err)
	}
}

func TestUpdater_NoTrailingZeroBlocks(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "no_zeros.tar")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)
	hdr := &Header{Name: "first.txt", Size: 4, Mode: 0644}
	tw.WriteHeader(hdr)
	tw.Write([]byte("data"))
	tw.Flush()
	f.Close()

	fRW, err := os.OpenFile(archivePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	updater, err := NewUpdater(fRW, APPEND_MODE_OVERWRITE)
	if err != nil {
		t.Fatalf("NewUpdater failed: %v", err)
	}

	err = updater.Append("second.txt", 4, []byte("more"))
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	updater.Close()
	fRW.Close()

	rc, err := OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	names := []string{}
	for {
		h, err := rc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}

	if len(names) != 2 || names[0] != "first.txt" || names[1] != "second.txt" {
		t.Errorf("Expected files ['first.txt', 'second.txt'], got %v", names)
	}
}
