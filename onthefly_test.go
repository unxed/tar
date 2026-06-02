//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos
package tar

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateWriter_OnTheFlyIndex verifies that CreateWriter correctly generates
// a seekable index while writing a compressed archive.
func TestCreateWriter_OnTheFlyIndex(t *testing.T) {
	methods := []uint16{GZIP, ZSTD}

	for _, method := range methods {
		t.Run(methodName(method), func(t *testing.T) {
			tmpDir := t.TempDir()
			archivePath := filepath.Join(tmpDir, "test.tar.gz")
			indexPath := filepath.Join(tmpDir, "test.sqlite")

			if method == ZSTD {
				archivePath = filepath.Join(tmpDir, "test.tar.zst")
			}

			wc, err := CreateWriter(archivePath, method, WithWriterIndex(indexPath))
			if err != nil {
				t.Fatalf("CreateWriter failed: %v", err)
			}

			// Write enough data to trigger periodic flushes (4MB threshold)
			// Total data: ~9MB
			file1Data := bytes.Repeat([]byte("A"), 5*1024*1024)
			hdr1 := &Header{Name: "large1.txt", Size: int64(len(file1Data)), Mode: 0644}
			if err := wc.WriteHeader(hdr1); err != nil {
				t.Fatal(err)
			}
			wc.Write(file1Data)

			file2Data := bytes.Repeat([]byte("B"), 4*1024*1024)
			hdr2 := &Header{Name: "large2.txt", Size: int64(len(file2Data)), Mode: 0644}
			if err := wc.WriteHeader(hdr2); err != nil {
				t.Fatal(err)
			}
			wc.Write(file2Data)

			wc.Close()

			// 1. Verify index file existence
			if _, err := os.Stat(indexPath); err != nil {
				t.Fatal("Index file was not created")
			}

			// 2. Verify seek points were created
			idx, err := OpenIndex(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			defer idx.Close()

			if method == GZIP {
				indexBytes, err := idx.GetGzipIndex()
				if err != nil || len(indexBytes) == 0 {
					t.Fatal("GZIP index blob missing or empty")
				}
				// We expect at least 3 points: 0, ~4MB, ~8MB
				// But GZIP uses concatenated streams, so we check logic in TarFS
			} else if method == ZSTD {
				var count int
				idx.db.QueryRow("SELECT COUNT(*) FROM zstdblocks").Scan(&count)
				if count < 2 {
					t.Errorf("Expected at least 2 ZSTD seek points, got %d", count)
				}
			}

			// 3. Verify random access works via TarFS
			tfs, err := NewFS(archivePath, indexPath)
			if err != nil {
				t.Fatal(err)
			}
			defer tfs.Close()

			// Read file2 (which starts after the first flush point)
			data, err := fs.ReadFile(tfs, "large2.txt")
			if err != nil {
				t.Fatalf("ReadFile failed: %v", err)
			}
			if len(data) != len(file2Data) || data[0] != 'B' {
				t.Error("Data corruption in indexed archive")
			}
		})
	}
}

// TestArchiver_OnTheFlyIndex verifies that the high-level Archiver correctly
// passes indexing options to the writer.
func TestArchiver_OnTheFlyIndex(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hello on the fly"), 0644)

	archivePath := filepath.Join(tmpDir, "archiver.tar.gz")
	indexPath := filepath.Join(tmpDir, "archiver.sqlite")

	a, err := NewArchiver(archivePath, filepath.Dir(srcDir),
		WithArchiverMethod(GZIP),
		WithArchiverIndex(indexPath),
	)
	if err != nil {
		t.Fatal(err)
	}

	files := make(map[string]os.FileInfo)
	filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			files[path] = info
		}
		return nil
	})

	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Verify index
	tfs, err := NewFS(archivePath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	data, err := fs.ReadFile(tfs, "src/hello.txt")
	if err != nil || string(data) != "hello on the fly" {
		t.Errorf("Archiver on-the-fly indexing failed: %v", err)
	}
}

func methodName(m uint16) string {
	switch m {
	case Store: return "Store"
	case GZIP:  return "GZIP"
	case BZIP2: return "BZIP2"
	case XZ:    return "XZ"
	case ZSTD:  return "ZSTD"
	default:    return "Unknown"
	}
}

// TestIndexing_MetadataAccuracy verifies that on-the-fly indexing correctly
// populates ratarmount-specific fields like offsetheader and isgenerated.
func TestIndexing_MetadataAccuracy(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "meta.tar")
	indexPath := filepath.Join(tmpDir, "meta.sqlite")

	wc, err := CreateWriter(archivePath, Store, WithWriterIndex(indexPath))
	if err != nil {
		t.Fatal(err)
	}

	// Create a nested file: this should trigger parent directory synthesis
	data := []byte("content")
	hdr := &Header{Name: "dir1/dir2/file.txt", Size: int64(len(data)), Mode: 0644}
	wc.WriteHeader(hdr)
	wc.Write(data)
	wc.Close()

	idx, err := OpenIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// 1. Check synthesized directories
	var count int
	idx.db.QueryRow("SELECT COUNT(*) FROM files WHERE isgenerated = 1").Scan(&count)
	if count != 2 {
		t.Errorf("Expected 2 synthesized directories (dir1, dir1/dir2), got %d", count)
	}

	// 2. Verify offsetheader vs offset
	node, err := idx.Lookup("dir1/dir2/file.txt")
	if err != nil {
		t.Fatal(err)
	}

	// In a standard TAR, header is 512 bytes.
	// Since it's the first file, header is at 0, data is at 512.
	if node.OffsetHeader != 0 {
		t.Errorf("Expected OffsetHeader 0, got %d", node.OffsetHeader)
	}
	if node.Offset != 512 {
		t.Errorf("Expected Data Offset 512, got %d", node.Offset)
	}

	// 3. Test RecursiveSize
	size, _ := idx.RecursiveSize("dir1")
	if size != int64(len(data)) {
		t.Errorf("RecursiveSize mismatch: expected %d, got %d", len(data), size)
	}
}

// TestIndexing_AllMethods verifies that metadata indexing works for every supported format.
func TestIndexing_AllMethods(t *testing.T) {
	// Note: BZIP2 is omitted because we only support BZIP2 decompression.
	methods := []uint16{Store, GZIP, XZ, ZSTD}

	for _, method := range methods {
		t.Run(methodName(method), func(t *testing.T) {
			tmpDir := t.TempDir()
			archivePath := filepath.Join(tmpDir, "test.archive")
			indexPath := filepath.Join(tmpDir, "test.sqlite")

			wc, err := CreateWriter(archivePath, method, WithWriterIndex(indexPath))
			if err != nil {
				t.Fatalf("Failed to create indexed writer for %s: %v", methodName(method), err)
			}
			wc.WriteHeader(&Header{Name: "test.txt", Size: 4})
			wc.Write([]byte("data"))
			wc.Close()

			idx, err := OpenIndex(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			defer idx.Close()

			node, err := idx.Lookup("test.txt")
			if err != nil || node.Name != "test.txt" {
				t.Errorf("Metadata not found in index for method %s", methodName(method))
			}
		})
	}
}