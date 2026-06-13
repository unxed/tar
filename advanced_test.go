package tar

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	// Directory with write restricted (0500)
	dirHdr := &Header{
		Name:     "restricted_dir/",
		Typeflag: TypeDir,
		Mode:     0500,
	}
	if err := tw.WriteHeader(dirHdr); err != nil {
		t.Fatal(err)
	}

	// File inside this directory
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

	// Read the first 2 entries
	entries, err := rdf.ReadDir(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(entries))
	}

	// Request 3 entries (but only 2 remain)
	entries2, err := rdf.ReadDir(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 2 {
		t.Errorf("Expected remaining 2 entries, got %d", len(entries2))
	}

	// Subsequent request should return io.EOF
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
func TestExtractor_LargeFileHybrid(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "large.tar")
	dstDir := filepath.Join(tmpDir, "extract")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewWriter(f)

	// 17MB file to trigger the synchronous direct-to-disk branch (> 16MB threshold)
	size := int64(17 * 1024 * 1024)
	hdr := &Header{Name: "large.txt", Size: size, Mode: 0644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}

	// Write dummy data in chunks efficiently
	chunk := bytes.Repeat([]byte("A"), 1024*1024)
	for i := 0; i < 17; i++ {
		tw.Write(chunk)
	}
	tw.Close()
	f.Close()

	// Use error handler to ignore chown issues on non-root environments
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
		t.Fatalf("Extraction failed for large file: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dstDir, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Errorf("Expected size %d, got %d", size, fi.Size())
	}
}

func TestTolerantMode_Tar(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "corrupt.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "good1.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("fine"))

	// Create file header for which data will not be fully written
	tw.WriteHeader(&Header{Name: "bad.txt", Size: 1000, Mode: 0644})
	tw.Write([]byte("too short"))
	// Do not close TW or write trailing zeros, just terminate the file
	f.Close()

	// Extract with TolerantMode(true)
	e, _ := NewExtractor(archivePath, dstDir, WithExtractorTolerant(true))
	err := e.Extract(context.Background())
	// In TAR, an error will come from the Next() loop if the structure is completely broken,
	// but if Next() succeeded and Copy() failed, tolerant mode will save it.
	_ = err

	if _, err := os.Stat(filepath.Join(dstDir, "good1.txt")); err != nil {
		t.Error("good1.txt missing")
	}
}

func TestExtractor_ConcurrencyIntegrity(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "stress.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("file_%d.txt", i)
		tw.WriteHeader(&Header{Name: name, Size: int64(len(name)), Mode: 0644})
		tw.Write([]byte(name))
	}
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error {
		return nil
	})
	e, _ := NewExtractor(archivePath, dstDir, WithExtractorConcurrency(20), ignoreChown)
	if err := e.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Close()

	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("file_%d.txt", i)
		data, _ := os.ReadFile(filepath.Join(dstDir, name))
		if string(data) != name {
			t.Errorf("Integrity breach at %s: expected %q, got %q", name, name, string(data))
		}
	}
}

// TestEmbeddedIndex_Roundtrip verifies that embedded indexes (F4SS) can be written and cleanly restored.
func TestEmbeddedIndex_Roundtrip(t *testing.T) {
	methods := []uint16{Store, GZIP, ZSTD}

	for _, method := range methods {
		t.Run(fmt.Sprintf("Method_%d", method), func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			os.MkdirAll(srcDir, 0755)

			filePath := filepath.Join(srcDir, "test.txt")
			os.WriteFile(filePath, []byte("embedded index verification data"), 0644)

			archivePath := filepath.Join(tmpDir, "archive.tar")
			switch method {
			case GZIP:
				archivePath += ".gz"
			case ZSTD:
				archivePath += ".zst"
			}

			indexPath := filepath.Join(tmpDir, "index.sqlite")

			// 1. Archive with Embedded Index (F4SS)
			a, err := NewArchiver(archivePath, tmpDir,
				WithArchiverMethod(method),
				WithArchiverIndex(indexPath),
				WithArchiverEmbeddedIndex(true),
			)
			if err != nil {
				t.Fatal(err)
			}

			fi, _ := os.Stat(filePath)
			files := map[string]os.FileInfo{filePath: fi}

			if err := a.Archive(context.Background(), files); err != nil {
				t.Fatal(err)
			}
			a.Close()

			// 2. Open archive with a completely fresh, non-existent index path
			// to force the reader to extract the embedded index.
			freshIndexPath := filepath.Join(tmpDir, "fresh_index.sqlite")
			tfs, err := NewFS(archivePath, freshIndexPath)
			if err != nil {
				t.Fatalf("Failed to open TarFS using embedded index: %v", err)
			}
			defer tfs.Close()

			// Ensure it marked the index as temporary (since we recovered it from shadow stream)
			if !tfs.isTemporaryIndex {
				t.Errorf("Expected isTemporaryIndex to be true")
			}

			// 3. Read back file and verify contents
			data, err := fs.ReadFile(tfs, "src/test.txt")
			if err != nil {
				t.Fatalf("Failed to read file from embedded index TarFS: %v", err)
			}

			if string(data) != "embedded index verification data" {
				t.Errorf("Content mismatch: expected 'embedded index verification data', got %q", string(data))
			}
		})
	}
}

// TestEmbeddedShadowExtraPayloads verifies that standard index payloads (GZIDX and DZIDX)
// are successfully written to Stream 2 (the shadow stream) of the archive.
func TestEmbeddedShadowExtraPayloads(t *testing.T) {
	methods := []uint16{GZIP, ZSTD}

	for _, method := range methods {
		t.Run(fmt.Sprintf("Method_%d", method), func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			os.MkdirAll(srcDir, 0755)

			filePath := filepath.Join(srcDir, "test.txt")
			// Create a >4MB file to trigger the flush threshold and generate multiple frames/seek-points
			largeData := bytes.Repeat([]byte("highly compressible repeated chunk payload data "), 250000)
			os.WriteFile(filePath, largeData, 0644)

			archivePath := filepath.Join(tmpDir, "archive.tar")
			if method == GZIP {
				archivePath += ".gz"
			} else {
				archivePath += ".zst"
			}

			indexPath := filepath.Join(tmpDir, "index.sqlite")

			a, err := NewArchiver(archivePath, tmpDir,
				WithArchiverMethod(method),
				WithArchiverIndex(indexPath),
				WithArchiverEmbeddedIndex(true),
			)
			if err != nil {
				t.Fatal(err)
			}

			fi, _ := os.Stat(filePath)
			files := map[string]os.FileInfo{filePath: fi}

			if err := a.Archive(context.Background(), files); err != nil {
				t.Fatal(err)
			}
			a.Close()

			f, err := os.Open(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			fiArch, _ := f.Stat()
			shadowStart, shadowSize, err := LocateShadowStream(f, fiArch.Size(), method)
			if err != nil {
				t.Fatalf("Failed to locate shadow stream: %v", err)
			}
			if shadowSize == 0 {
				t.Fatal("Shadow stream size is zero")
			}

			sr := io.NewSectionReader(f, shadowStart, shadowSize)
			var rd io.Reader = sr

			di, ok := decompressors.Load(method)
			if !ok {
				t.Fatal("Failed to load decompressor")
			}
			dcomp, err := di.(Decompressor).Decompress(rd)
			if err != nil {
				t.Fatalf("Decompress shadow stream failed: %v", err)
			}
			defer dcomp.Close()

			tr := NewReader(dcomp)
			foundIndexFile := false
			expectedName := ".tarext/GZIDX/index.gzidx"
			if method == ZSTD {
				expectedName = ".tarext/dictzip/index.dzidx"
			}

			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Tar next failed: %v", err)
				}
				if hdr.Name == expectedName {
					foundIndexFile = true
					data, err := io.ReadAll(tr)
					if err != nil {
						t.Fatal(err)
					}
					if len(data) == 0 {
						t.Errorf("Expected index payload %s to be non-empty", expectedName)
					}
					if method == GZIP {
						gr, err := gzip.NewReader(bytes.NewReader(data))
						if err == nil {
							defer gr.Close()
							decomp, _ := io.ReadAll(gr)
							if len(decomp) < 5 || string(decomp[:5]) != "GZIDX" {
								t.Errorf("Invalid GZIDX magic in decompressed payload")
							}
						} else {
							t.Errorf("GZIDX shadow payload is not a valid gzip file: %v", err)
						}
					} else if method == ZSTD {
						if len(data) < 5 || string(data[:5]) != "DZIDX" {
							t.Errorf("Expected DZIDX signature in ZSTD shadow payload, got %q", string(data))
						}
					}
				}
			}

			if !foundIndexFile {
				t.Errorf("Failed to find standard index file %s in shadow stream", expectedName)
			}
		})
	}
}

// TestExportDZIDX_EdgeCases verifies bounds and structure of DZIDX format under different offset conditions.
func TestExportDZIDX_EdgeCases(t *testing.T) {
	// Case 1: Empty offsets should return nil
	if res := exportDZIDX(nil, 100, 100); res != nil {
		t.Errorf("Expected nil for empty offsets, got %v", res)
	}

	// Case 2: Single block offset
	offsets := []BlockOffset{{BlockOffset: 0, DataOffset: 0}}
	res := exportDZIDX(offsets, 100, 100)
	if len(res) == 0 {
		t.Fatal("Expected non-empty DZIDX for single offset")
	}
	if string(res[:5]) != "DZIDX" {
		t.Errorf("Expected DZIDX magic, got %q", res[:5])
	}

	// Case 3: Multiple offsets (calculating accurate contiguous chunks)
	offsets = []BlockOffset{
		{BlockOffset: 0, DataOffset: 0},
		{BlockOffset: 500, DataOffset: 1000},
	}
	res2 := exportDZIDX(offsets, 1200, 3000)
	if len(res2) == 0 {
		t.Fatal("Expected non-empty DZIDX for multiple offsets")
	}
	if len(res2) < 32 {
		t.Errorf("Expected DZIDX header to be at least 32 bytes, got %d", len(res2))
	}
}
