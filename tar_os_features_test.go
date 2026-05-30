package tar

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractor_KeepOldFiles(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "keepold.tar")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(dstDir, 0755)

	// Create existing file
	targetPath := filepath.Join(dstDir, "test.txt")
	os.WriteFile(targetPath, []byte("ORIGINAL"), 0644)

	// Create archive containing the same file
	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "test.txt", Size: 3, Mode: 0644})
	tw.Write([]byte("NEW"))
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error {
		return nil
	})

	// Extract with KeepOldFiles flag (-k)
	e, err := NewExtractor(archivePath, dstDir, WithExtractorKeepOldFiles(true), ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Verify content is NOT overwritten
	data, _ := os.ReadFile(targetPath)
	if string(data) != "ORIGINAL" {
		t.Errorf("Expected ORIGINAL, got %s", string(data))
	}
}

func TestExtractor_KeepNewerFiles(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "keepnewer.tar")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(dstDir, 0755)

	targetPath := filepath.Join(dstDir, "test.txt")
	os.WriteFile(targetPath, []byte("NEWER_DISK"), 0644)

	// Make disk file obviously newer
	newerTime := time.Now().Add(1 * time.Hour)
	os.Chtimes(targetPath, newerTime, newerTime)

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	// Older time in archive
	tw.WriteHeader(&Header{Name: "test.txt", Size: 7, Mode: 0644, ModTime: time.Now().Add(-1 * time.Hour)})
	tw.Write([]byte("ARCHIVE"))
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error {
		return nil
	})

	e, err := NewExtractor(archivePath, dstDir, WithExtractorKeepNewerFiles(true), ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(targetPath)
	if string(data) != "NEWER_DISK" {
		t.Errorf("Expected NEWER_DISK, got %s", string(data))
	}
}

func TestExtractor_IgnoreVolumeHeaderAndGlobalMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "vol.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "My Volume Label", Typeflag: TypeVol})
	tw.WriteHeader(&Header{Name: "GlobalPaxHeader", Typeflag: TypeXGlobalHeader})
	tw.WriteHeader(&Header{Name: "file.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
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

	if err := e.Extract(context.Background()); err != nil {
		t.Fatalf("Extraction failed: %v", err)
	}

	// Ensure "My Volume Label" and Global Headers weren't created as real files on disk
	if _, err := os.Stat(filepath.Join(dstDir, "My Volume Label")); !os.IsNotExist(err) {
		t.Errorf("Volume label header was erroneously extracted to disk as a file")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "GlobalPaxHeader")); !os.IsNotExist(err) {
		t.Errorf("Global Pax header was erroneously extracted to disk as a file")
	}
}

func TestExtractor_NoTimes(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "notimes.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	// Set very old ModTime
	oldTime := time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC)
	tw.WriteHeader(&Header{Name: "oldfile.txt", Size: 4, Mode: 0644, ModTime: oldTime})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })
	e, err := NewExtractor(archivePath, dstDir, WithExtractorNoTimes(true), ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dstDir, "oldfile.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// The modification time must NOT be the old 1999 timestamp
	if fi.ModTime().Equal(oldTime) {
		t.Errorf("Modification time was restored despite WithExtractorNoTimes(true)")
	}
}

func TestExtractor_StripComponents(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "strip.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "level1/level2/target.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.WriteHeader(&Header{Name: "short.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })
	// Strip 1 component: "level1/level2/target.txt" -> "level2/target.txt", "short.txt" -> skipped
	e, err := NewExtractor(archivePath, dstDir, WithExtractorStripComponents(1), ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Verify nested file is stripped correctly
	if _, err := os.Stat(filepath.Join(dstDir, "level2", "target.txt")); err != nil {
		t.Errorf("Expected stripped nested file not found: %v", err)
	}

	// Verify short.txt is skipped
	if _, err := os.Stat(filepath.Join(dstDir, "short.txt")); !os.IsNotExist(err) {
		t.Errorf("Expected short.txt to be skipped, but it was extracted")
	}
}

func TestExtractor_IgnoreSpecialGNUHeaders(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "special_gnu.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "DumpDirData", Typeflag: TypeGNUDumpDir})
	tw.WriteHeader(&Header{Name: "MultiVolData", Typeflag: TypeGNUMultiVol})
	tw.WriteHeader(&Header{Name: "normal.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })
	e, err := NewExtractor(archivePath, dstDir, ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatalf("Extraction failed with special GNU headers: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dstDir, "normal.txt")); err != nil {
		t.Errorf("Expected normal.txt to be extracted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "DumpDirData")); !os.IsNotExist(err) {
		t.Errorf("Special GNU header was erroneously extracted as a file")
	}
}
