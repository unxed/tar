package tar

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
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

func TestExtractor_GNU_MultiVol_And_DumpDir(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "special_gnu.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)

	// GNU DumpDir acts as a directory entry
	tw.WriteHeader(&Header{Name: "DumpDirData", Typeflag: TypeGNUDumpDir, Mode: 0755})

	// MultiVol chunk appending to normal.txt
	tw.WriteHeader(&Header{Name: "normal.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.WriteHeader(&Header{Name: "normal.txt", Size: 5, Mode: 0644, Typeflag: TypeGNUMultiVol})
	tw.Write([]byte("_more"))
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

	data, err := os.ReadFile(filepath.Join(dstDir, "normal.txt"))
	if err != nil {
		t.Errorf("Expected normal.txt to be extracted: %v", err)
	} else if string(data) != "data_more" {
		t.Errorf("Multi-volume chunk was not appended correctly. Expected 'data_more', got %q", string(data))
	}

	fi, err := os.Stat(filepath.Join(dstDir, "DumpDirData"))
	if err != nil || !fi.IsDir() {
		t.Errorf("DumpDir GNU header should have been created as a directory, got %v", err)
	}
}

func TestExtractor_IncrementalDumpDir(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "incr.tar")
	dstDir := filepath.Join(tmpDir, "dst")

	// Pre-populate extraction directory with files that existed before backup
	os.MkdirAll(filepath.Join(dstDir, "myfolder"), 0755)
	os.WriteFile(filepath.Join(dstDir, "myfolder", "stay.txt"), []byte("i stay"), 0644)
	os.WriteFile(filepath.Join(dstDir, "myfolder", "deleted.txt"), []byte("i should be deleted"), 0644)

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)

	// Create a GNU Dumpdir entry that only lists "stay.txt" as existing.
	// Format: [tag][filename]\0 ... \0
	dumpdirData := []byte("Ystay.txt\x00\x00")

	tw.WriteHeader(&Header{Name: "myfolder", Size: int64(len(dumpdirData)), Typeflag: TypeGNUDumpDir, Mode: 0755})
	tw.Write(dumpdirData)
	tw.Close()
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })
	e, err := NewExtractor(archivePath, dstDir, WithExtractorIncremental(true), ignoreChown)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Extract(context.Background()); err != nil {
		t.Fatalf("Incremental extraction failed: %v", err)
	}

	// Verify "stay.txt" is untouched
	if _, err := os.Stat(filepath.Join(dstDir, "myfolder", "stay.txt")); err != nil {
		t.Errorf("File 'stay.txt' was wrongly deleted")
	}

	// Verify "deleted.txt" was removed because it was absent from the GNU dumpdir list
	if _, err := os.Stat(filepath.Join(dstDir, "myfolder", "deleted.txt")); !os.IsNotExist(err) {
		t.Errorf("File 'deleted.txt' was not deleted during incremental restore")
	}
}
func TestNtfsAclAndAds_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows-specific test")
	}

	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "test.txt")
	err := os.WriteFile(filePath, []byte("main data"), 0644)
	if err != nil {
		t.Fatalf("failed to write main file: %v", err)
	}

	// Write an alternate data stream
	adsPath := filePath + ":my_stream"
	err = os.WriteFile(adsPath, []byte("stream data"), 0644)
	if err != nil {
		t.Fatalf("failed to write alternate data stream: %v", err)
	}

	// Verify getAlternativeDataStreams
	streams, err := getAlternativeDataStreams(filePath)
	if err != nil {
		t.Fatalf("getAlternativeDataStreams failed: %v", err)
	}
	found := false
	for _, s := range streams {
		if s == ":my_stream" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected to find stream ':my_stream', got %v", streams)
	}

	// Verify getFileSecurity
	acl, err := getFileSecurity(filePath)
	if err != nil {
		t.Fatalf("getFileSecurity failed: %v", err)
	}
	if len(acl) == 0 {
		t.Error("expected non-empty security descriptor")
	}

	// Verify applyNtfsAcl
	err = applyNtfsAcl(filePath, acl)
	if err != nil {
		t.Errorf("applyNtfsAcl failed: %v", err)
	}
}

func TestNtfsAclAndAds_Mocked(t *testing.T) {
	// Save original functions
	origGetFileSecurity := getFileSecurityFunc
	origApplyNtfsAcl := applyNtfsAclFunc
	origGetAlternativeDataStreams := getAlternativeDataStreamsFunc

	defer func() {
		getFileSecurityFunc = origGetFileSecurity
		applyNtfsAclFunc = origApplyNtfsAcl
		getAlternativeDataStreamsFunc = origGetAlternativeDataStreams
	}()

	mockAcl := []byte("mock-security-descriptor-data")
	mockStreams := []string{":Zone.Identifier", ":custom_stream"}

	// Setup mocks
	getFileSecurityFunc = func(path string) ([]byte, error) {
		return mockAcl, nil
	}

	appliedAcl := []byte{}
	applyNtfsAclFunc = func(path string, acl []byte) error {
		appliedAcl = acl
		return nil
	}

	getAlternativeDataStreamsFunc = func(path string) ([]string, error) {
		return mockStreams, nil
	}

	// Test archiver with mocks
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "test_file.txt")
	os.WriteFile(filePath, []byte("some content"), 0644)

	os.WriteFile(filePath+":Zone.Identifier", []byte("zone data"), 0644)
	os.WriteFile(filePath+":custom_stream", []byte("custom data"), 0644)

	tarPath := filepath.Join(tmp, "archive.tar")
	a, err := NewArchiver(tarPath, tmp, WithArchiverXattrs(true))
	if err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(filePath)
	files := map[string]os.FileInfo{filePath: info}

	err = a.Archive(context.Background(), files)
	if err != nil {
		t.Fatalf("Archiving with mocks failed: %v", err)
	}
	a.Close()

	// Extract and verify
	dstDir := filepath.Join(tmp, "extracted")
	e, err := NewExtractor(tarPath, dstDir, WithExtractorXattrs(true))
	if err != nil {
		t.Fatal(err)
	}

	err = e.Extract(context.Background())
	if err != nil {
		t.Fatalf("Extraction with mocks failed: %v", err)
	}
	e.Close()

	// Verify applied ACL
	if !bytes.Equal(appliedAcl, mockAcl) {
		t.Errorf("expected applied ACL %q, got %q", string(mockAcl), string(appliedAcl))
	}
}

func TestExtractor_LinksToDirs(t *testing.T) {
	tmp := t.TempDir()
	tarPath := filepath.Join(tmp, "links_to_dirs.tar")
	dstDir := filepath.Join(tmp, "extract")

	f, _ := os.Create(tarPath)
	zw := NewWriter(f)
	zw.WriteHeader(&Header{Name: "sub/file.txt", Size: 9, Mode: 0644})
	zw.Write([]byte("file-data"))
	zw.Close()
	f.Close()

	trap := filepath.Join(tmp, "trap")
	os.Mkdir(trap, 0755)
	os.Mkdir(dstDir, 0755)
	os.Symlink(trap, filepath.Join(dstDir, "sub"))

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })
	e, _ := NewExtractor(tarPath, dstDir, ignoreChown)
	err := e.Extract(context.Background())
	if err != nil {
		t.Fatalf("Extraction failed: %v", err)
	}
	e.Close()

	fi, err := os.Lstat(filepath.Join(dstDir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("expected symlink 'sub' to be deleted and replaced with a physical directory")
	}

	if _, err := os.Stat(filepath.Join(trap, "file.txt")); err == nil {
		t.Error("Security violation! File extracted through symlink")
	}
}

func TestExtractor_SanitizeMOTW(t *testing.T) {
	tmp := t.TempDir()
	tarPath := filepath.Join(tmp, "motw.tar")
	dstDir := filepath.Join(tmp, "extract")

	f, _ := os.Create(tarPath)
	zw := NewWriter(f)
	data := []byte("[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=http://evil.com/leak\r\nHostUrl=http://evil.com/file\r\n")
	zw.WriteHeader(&Header{Name: "test.txt:Zone.Identifier", Size: int64(len(data)), Mode: 0644})
	zw.Write(data)
	zw.Close()
	f.Close()

	e, _ := NewExtractor(tarPath, dstDir)
	e.Extract(context.Background())
	e.Close()

	out, err := os.ReadFile(filepath.Join(dstDir, "test.txt:Zone.Identifier"))
	if err != nil {
		t.Fatal(err)
	}
	expected := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if string(out) != expected {
		t.Errorf("expected sanitized MOTW %q, got %q", expected, string(out))
	}
}

func TestExtractor_KeepBroken(t *testing.T) {
	tmp := t.TempDir()
	tarPath := filepath.Join(tmp, "broken.tar")
	dstDir := filepath.Join(tmp, "extract")

	f, _ := os.Create(tarPath)
	zw := NewWriter(f)
	// Declare a large file size (17MB) to force the streaming large-file branch
	size := int64(17 * 1024 * 1024)
	zw.WriteHeader(&Header{Name: "file.txt", Size: size, Mode: 0644})
	zw.Write(make([]byte, 1024*1024)) // write only 1MB
	zw.Flush()

	// Truncate the physical archive file to 512KB to force an unexpected EOF during read
	f.Truncate(512 * 1024)
	f.Close()

	ignoreChown := WithExtractorChownErrorHandler(func(name string, err error) error { return nil })

	// 1. Extraction without KeepBroken (default): file should be cleaned up (deleted)
	e, _ := NewExtractor(tarPath, dstDir, ignoreChown)
	err := e.Extract(context.Background())
	e.Close()
	if err == nil {
		t.Error("expected extraction to fail due to corruption")
	}
	if _, serr := os.Stat(filepath.Join(dstDir, "file.txt")); serr == nil {
		t.Error("expected corrupted file to be deleted by default")
	}

	// 2. Extraction with KeepBroken: file should be preserved
	os.RemoveAll(dstDir)
	e2, _ := NewExtractor(tarPath, dstDir, WithExtractorKeepBroken(true), ignoreChown)
	err2 := e2.Extract(context.Background())
	e2.Close()
	if err2 == nil {
		t.Error("expected extraction to fail")
	}
	if _, serr := os.Stat(filepath.Join(dstDir, "file.txt")); serr != nil {
		t.Error("expected corrupted file to be preserved when KeepBroken is enabled")
	}
}
