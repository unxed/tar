//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos

package tar

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestF4Crypt_Roundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)

	secretData := []byte("top secret classification 1")
	os.WriteFile(filepath.Join(srcDir, "secret.txt"), secretData, 0644)

	archivePath := filepath.Join(tmpDir, "encrypted.tar")

	// 1. Archive with password
	a, err := NewArchiver(archivePath, tmpDir, WithArchiverPassword("my_pass"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(srcDir, "secret.txt"))
	files := map[string]os.FileInfo{filepath.Join(srcDir, "secret.txt"): fi}
	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// 2. Extract without password (legacy mode) - should extract README stub
	legacyDir := filepath.Join(tmpDir, "legacy_dst")
	eLegacy, err := NewExtractor(archivePath, legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	eLegacy.Extract(context.Background())
	eLegacy.Close()

	stub, err := os.ReadFile(filepath.Join(legacyDir, "README_ENCRYPTED.txt"))
	if err != nil || len(stub) == 0 {
		t.Fatalf("Legacy extraction failed to produce README stub")
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "src", "secret.txt")); err == nil {
		t.Fatalf("Legacy extraction erroneously extracted secret data!")
	}

	// 3. Extract with incorrect password - should fail
	eWrong, err := NewExtractor(archivePath, tmpDir, WithExtractorPassword("wrong"))
	if err == nil {
		eWrong.Extract(context.Background())
		eWrong.Close()
	}
	// Depending on timing, error might occur during NewExtractor or Extract, but data should not exist.
	if _, err := os.Stat(filepath.Join(tmpDir, "wrong_dst", "src", "secret.txt")); err == nil {
		t.Fatalf("Wrong password erroneously extracted data!")
	}

	// 4. Extract with correct password
	dstDir := filepath.Join(tmpDir, "valid_dst")
	eValid, err := NewExtractor(archivePath, dstDir, WithExtractorPassword("my_pass"))
	if err != nil {
		t.Fatalf("NewExtractor with password failed: %v", err)
	}
	err = eValid.Extract(context.Background())
	if err != nil {
		t.Fatalf("Valid extraction failed: %v", err)
	}
	eValid.Close()

	data, err := os.ReadFile(filepath.Join(dstDir, "src", "secret.txt"))
	if err != nil || string(data) != string(secretData) {
		t.Fatalf("Valid extraction data mismatch: %v", string(data))
	}
}

func TestF4Crypt_WithEmbeddedIndex(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "fast.txt"), []byte("instant random access encrypted"), 0644)

	archivePath := filepath.Join(tmpDir, "encrypted_indexed.tar")
	indexPath := filepath.Join(tmpDir, "temp_index.sqlite")

	// Create archive with BOTH encryption and embedded F4SS index
	a, err := NewArchiver(archivePath, tmpDir,
		WithArchiverPassword("super_secure"),
		WithArchiverEmbeddedIndex(true),
		WithArchiverIndex(indexPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(srcDir, "fast.txt"))
	files := map[string]os.FileInfo{filepath.Join(srcDir, "fast.txt"): fi}
	a.Archive(context.Background(), files)
	a.Close()

	// Use TarFS to open the encrypted archive seamlessly
	tfs, err := NewFS(archivePath, "", WithFSPassword("super_secure"))
	if err != nil {
		t.Fatalf("TarFS failed to open encrypted archive: %v", err)
	}
	defer tfs.Close()

	// Direct O(1) random access inside encrypted, indexed payload
	data, err := fs.ReadFile(tfs, "src/fast.txt")
	if err != nil {
		t.Fatalf("TarFS ReadFile failed: %v", err)
	}
	if string(data) != "instant random access encrypted" {
		t.Fatalf("Content mismatch: %s", string(data))
	}
}
