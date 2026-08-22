package tar

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdater_Compressed_Append(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "comp.tar.zst")

	// 1. Create compressed F4SS archive with shadow index
	a, err := NewArchiver(archivePath, tmpDir, WithArchiverMethod(ZSTD), WithArchiverEmbeddedIndex(true))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tmpDir, "first.txt"), []byte("first-content"), 0644)
	fi, _ := os.Stat(filepath.Join(tmpDir, "first.txt"))
	err = a.Archive(context.Background(), map[string]os.FileInfo{filepath.Join(tmpDir, "first.txt"): fi})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	fiArch, _ := os.Stat(archivePath)
	t.Logf("[DIAG-UPD-TAR-TEST] Physical archive size after initial creation: %d\n", fiArch.Size())

	// 2. Open with Updater (should automatically detect ZSTD & shadow stream, then truncate Stream 2)
	f, err := os.OpenFile(archivePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	updater, err := NewUpdater(f, APPEND_MODE_OVERWRITE)
	if err != nil {
		f.Close()
		t.Fatalf("failed to init compressed updater: %v", err)
	}

	// Append second file
	err = updater.Append("second.txt", 14, []byte("second-content"))
	if err != nil {
		updater.Close()
		f.Close()
		t.Fatalf("failed to append to compressed archive: %v", err)
	}

	errClose := updater.Close()
	t.Logf("[DIAG-UPD-TAR-TEST] updater.Close() returned error: %v\n", errClose)
	f.Close()

	fiArch2, _ := os.Stat(archivePath)
	t.Logf("[DIAG-UPD-TAR-TEST] Physical archive size after append and close: %d\n", fiArch2.Size())

	// Raw TAR Reader trace
	fRaw, _ := os.Open(archivePath)
	defer fRaw.Close()
	methodRaw, _ := DetectFormat(fRaw)
	shadowStartRaw, shadowSizeRaw, _ := LocateShadowStream(fRaw, fiArch2.Size(), methodRaw)
	t.Logf("[DIAG-UPD-TAR-TEST] Post-append ShadowStart: %d, ShadowSize: %d", shadowStartRaw, shadowSizeRaw)

	fRaw.Seek(0, 0)
	ci, _ := decompressors.Load(methodRaw)
	decRaw, _ := ci.(Decompressor).Decompress(fRaw)
	trRaw := NewReader(decRaw)
	t.Logf("[DIAG-UPD-TAR-TEST] Raw reading Stream 1:")
	for {
		hdr, err := trRaw.Next()
		if err == io.EOF {
			t.Logf("[DIAG-UPD-TAR-TEST] Hit EOF")
			break
		}
		if err != nil {
			t.Logf("[DIAG-UPD-TAR-TEST] Hit Error: %v", err)
			break
		}
		t.Logf("[DIAG-UPD-TAR-TEST] Found header: %s", hdr.Name)
	}
	decRaw.Close()

	// 3. Verify both files are readable via TarFS (which parses the newly generated shadow index)
	tfs, err := NewFS(archivePath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	b1, err := fs.ReadFile(tfs, "first.txt")
	if err != nil || string(b1) != "first-content" {
		t.Errorf("failed to read first file: %v", err)
	}

	b2, err := fs.ReadFile(tfs, "second.txt")
	if err != nil || string(b2) != "second-content" {
		t.Errorf("failed to read appended second file: %v", err)
	}
}

func TestTar_Lock_Archive(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "locked.tar.zst")

	// 1. Create with Lock
	a, _ := NewArchiver(archivePath, tmpDir, WithArchiverMethod(ZSTD), WithArchiverLock(true), WithArchiverEmbeddedIndex(true))
	os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("data"), 0644)
	fi, _ := os.Stat(filepath.Join(tmpDir, "test.txt"))
	a.Archive(context.Background(), map[string]os.FileInfo{filepath.Join(tmpDir, "test.txt"): fi})
	a.Close()

	// 2. Try to update
	f, _ := os.OpenFile(archivePath, os.O_RDWR, 0644)
	defer f.Close()

	_, err := NewUpdater(f, APPEND_MODE_OVERWRITE)
	if err != ErrArchiveLocked {
		t.Errorf("expected ErrArchiveLocked, got: %v", err)
	}
}

func TestTar_Global_Comments(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "comment.tar.zst")

	a, _ := NewArchiver(archivePath, tmpDir, WithArchiverMethod(ZSTD), WithArchiverEmbeddedIndex(true))
	a.SetComment("F4SS global comment test")
	os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("data"), 0644)
	fi, _ := os.Stat(filepath.Join(tmpDir, "test.txt"))
	a.Archive(context.Background(), map[string]os.FileInfo{filepath.Join(tmpDir, "test.txt"): fi})
	a.Close()

	tfs, err := NewFS(archivePath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	comment := tfs.GetComment()
	if comment != "F4SS global comment test" {
		t.Errorf("expected comment 'F4SS global comment test', got %q", comment)
	}
}

func TestTarExtractor_SynthesizeDirConflict(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "conflict.tar")
	dstDir := filepath.Join(tmpDir, "extract")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	// Directory a/b/file.txt but a/b is historically blocked by a file
	tw.WriteHeader(&Header{Name: "a/b/file.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	os.MkdirAll(dstDir, 0755)
	// Pre-create a/b as a FILE instead of a directory
	os.MkdirAll(filepath.Join(dstDir, "a"), 0755)
	os.WriteFile(filepath.Join(dstDir, "a", "b"), []byte("blocking-file"), 0644)

	e, _ := NewExtractor(archivePath, dstDir)
	err := e.Extract(context.Background())
	if err != nil {
		t.Fatalf("extraction failed under path conflict: %v", err)
	}
	e.Close()

	data, err := os.ReadFile(filepath.Join(dstDir, "a/b/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data" {
		t.Errorf("expected 'data', got %q", string(data))
	}
}
