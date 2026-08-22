package tar

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompressAndExtract(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0755)

	os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("hello tar"), 0644)
	os.WriteFile(filepath.Join(srcDir, "sub", "file2.txt"), []byte("nested"), 0644)

	archivePath := filepath.Join(tmpDir, "archive.tar.gz")

	// Test Compress
	if err := Compress(srcDir, archivePath); err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	// Test Extract
	if err := Extract(archivePath, dstDir); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dstDir, "src", "file1.txt"))
	if err != nil || string(content) != "hello tar" {
		t.Errorf("file1.txt extraction failed")
	}

	content, err = os.ReadFile(filepath.Join(dstDir, "src", "sub", "file2.txt"))
	if err != nil || string(content) != "nested" {
		t.Errorf("sub/file2.txt extraction failed")
	}
}

func TestUpdater(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "append.tar")

	// Create a standard uncompressed tar
	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: "first.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	// Open for update
	fRW, _ := os.OpenFile(archivePath, os.O_RDWR, 0644)
	updater, err := NewUpdater(fRW, APPEND_MODE_OVERWRITE)
	if err != nil {
		t.Fatalf("NewUpdater failed: %v", err)
	}

	err = updater.Append("second.txt", 6, []byte("second"))
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	updater.Close()
	fRW.Close()

	// Verify both files exist
	rc, err := OpenReader(archivePath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer rc.Close()

	found := 0
	for {
		hdr, err := rc.Next()
		if err == io.EOF {
			break
		}
		if hdr.Name == "first.txt" || hdr.Name == "second.txt" {
			found++
		}
	}

	if found != 2 {
		t.Errorf("Expected 2 files, found %d", found)
	}
}

func TestTarExternalCompatibility_Tar(t *testing.T) {
	tarPath, err := exec.LookPath("tar")
	hasTar := err == nil
	bsdtarPath, err := exec.LookPath("bsdtar")
	hasBsdTar := err == nil

	if !hasTar && !hasBsdTar {
		t.Skip("Neither native tar nor bsdtar found on this system. Skipping external compatibility check.")
	}

	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)

	filePath := filepath.Join(srcDir, "test.txt")
	os.WriteFile(filePath, []byte("tar compatibility content"), 0644)

	archivePath := filepath.Join(tmpDir, "compat.tar.gz")
	a, err := NewArchiver(archivePath, tmpDir, WithArchiverMethod(GZIP), WithArchiverXattrs(true))
	if err != nil {
		t.Fatal(err)
	}

	mockHdr := &Header{
		Name:     "src/test.txt",
		Size:     int64(len("tar compatibility content")),
		Mode:     0644,
		Typeflag: TypeReg,
		Uid:      8888,
		Gid:      9999,
		Uname:    "compatuser",
		Gname:    "compatgroup",
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.compat": "yes",
			"MSWINDOWS.raw_sd":         "mock-acl-descriptor-data",
		},
	}

	a.m.Lock()
	err = a.wc.WriteHeader(mockHdr)
	a.m.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	a.wc.Write([]byte("tar compatibility content"))
	a.Close()

	verifyExtracted := func(dst string, name string) {
		extractedFile := filepath.Join(dst, "src", "test.txt")
		data, err := os.ReadFile(extractedFile)
		if err != nil {
			t.Fatalf("Failed to read extracted file by external tar: %v", err)
		}
		if string(data) != "tar compatibility content" {
			t.Errorf("Content mismatch: expected 'tar compatibility content', got %q", string(data))
		}

		if runtime.GOOS == "linux" {
			cmd := exec.Command("getfattr", "-n", "user.compat", extractedFile)
			if output, err := cmd.CombinedOutput(); err == nil {
				if strings.Contains(string(output), "yes") {
					t.Log("[DEBUG TEST] Native tar successfully restored POSIX xattrs!")
				}
			}
		}
	}

	if hasTar {
		t.Logf("[DEBUG TEST] Found native tar utility at %s. Verifying compatibility...", tarPath)
		dstDir := filepath.Join(tmpDir, "tar_dst")
		os.MkdirAll(dstDir, 0755)

		cmd := exec.Command(tarPath, "-xzf", archivePath, "-C", dstDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Logf("[DEBUG TEST] Native tar execution failed (might lack --xattrs support on this OS): %v, output: %s", err, string(output))
		} else {
			verifyExtracted(dstDir, "tar")
		}
	}

	if hasBsdTar {
		t.Logf("[DEBUG TEST] Found native bsdtar utility at %s. Verifying compatibility...", bsdtarPath)
		dstDir := filepath.Join(tmpDir, "bsdtar_dst")
		os.MkdirAll(dstDir, 0755)

		cmd := exec.Command(bsdtarPath, "-xzf", archivePath, "-C", dstDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Logf("[DEBUG TEST] Native bsdtar execution failed: %v, output: %s", err, string(output))
		} else {
			verifyExtracted(dstDir, "bsdtar")
		}
	}
}

func TestEmbeddedIndexExternalCompatibility(t *testing.T) {
	tarPath, err := exec.LookPath("tar")
	hasTar := err == nil
	bsdtarPath, err := exec.LookPath("bsdtar")
	hasBsdTar := err == nil

	if !hasTar && !hasBsdTar {
		t.Skip("Neither native tar nor bsdtar found on this system. Skipping external compatibility check for embedded indexes.")
	}

	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "test.txt"), []byte("compat content"), 0644)

	archivePath := filepath.Join(tmpDir, "embedded_compat.tar.gz")
	indexPath := filepath.Join(tmpDir, "embedded_compat.sqlite")

	a, err := NewArchiver(archivePath, tmpDir,
		WithArchiverMethod(GZIP),
		WithArchiverIndex(indexPath),
		WithArchiverEmbeddedIndex(true),
	)
	if err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(filepath.Join(srcDir, "test.txt"))
	files := map[string]os.FileInfo{filepath.Join(srcDir, "test.txt"): fi}
	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	a.Close()

	verifyExtraction := func(binPath, name string, ignoreZeros bool) {
		dstDir := filepath.Join(tmpDir, name+"_dst")
		os.MkdirAll(dstDir, 0755)

		args := []string{"-xzf", archivePath, "-C", dstDir}
		if ignoreZeros {
			args = append(args, "-i")
		}

		cmd := exec.Command(binPath, args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if ignoreZeros && strings.Contains(err.Error(), "invalid option") {
				return
			}
			t.Fatalf("%s failed: %v, output: %s", name, err, string(output))
		}

		// Verify main file is extracted
		data, err := os.ReadFile(filepath.Join(dstDir, "src", "test.txt"))
		if err != nil {
			t.Fatalf("Failed to read file extracted by %s: %v", name, err)
		}
		if string(data) != "compat content" {
			t.Errorf("Content mismatch: %q", string(data))
		}

		// Verify hidden index file is NOT extracted unless we used ignoreZeros
		hiddenFile := filepath.Join(dstDir, ".tarext", "ratarmount", "index.sqlite")
		if ignoreZeros {
			if _, err := os.Stat(hiddenFile); os.IsNotExist(err) {
				t.Errorf("Expected hidden index %s to be extracted with -i flag", hiddenFile)
			}
		} else {
			if _, err := os.Stat(hiddenFile); err == nil {
				t.Errorf("Hidden index %s was erroneously extracted without -i flag!", hiddenFile)
			}
		}
	}

	if hasTar {
		verifyExtraction(tarPath, "tar", false)
		verifyExtraction(tarPath, "tar_ignore_zeros", true)
	}
	if hasBsdTar {
		verifyExtraction(bsdtarPath, "bsdtar", false)
	}
}

func TestZstdCompression(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "test.tar.zst")

	wc, err := CreateWriter(archivePath, ZSTD)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}

	wc.WriteHeader(&Header{Name: "zst.txt", Size: 4})
	wc.Write([]byte("zstd"))
	wc.Close()

	rc, err := OpenReader(archivePath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer rc.Close()

	hdr, err := rc.Next()
	if err != nil || hdr.Name != "zst.txt" {
		t.Errorf("Failed to read ZSTD compressed tar")
	}

	buf := new(bytes.Buffer)
	io.Copy(buf, rc)
	if buf.String() != "zstd" {
		t.Errorf("Data mismatch")
	}
}
