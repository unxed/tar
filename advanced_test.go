package tar

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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