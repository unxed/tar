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