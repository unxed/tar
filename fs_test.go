package tar

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestTarFS(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "folder"), 0755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello random access"), 0644)
	os.WriteFile(filepath.Join(srcDir, "folder", "sub.txt"), []byte("ratarmount is awesome"), 0644)

	tarPath := filepath.Join(tmpDir, "test.tar.zst")
	err := Compress(srcDir, tarPath)
	if err != nil {
		t.Fatal(err)
	}

	// Mount it
	idxPath := filepath.Join(tmpDir, "test.tar.zst.index.sqlite")
	tfs, err := NewFS(tarPath, idxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tfs.Close()

	// Test Random Access / ReadFile
	b, err := fs.ReadFile(tfs, "src/file.txt")
	if err != nil || string(b) != "hello random access" {
		t.Errorf("ReadFile file.txt failed: %v, %s", err, string(b))
	}

	b, err = fs.ReadFile(tfs, "src/folder/sub.txt")
	if err != nil || string(b) != "ratarmount is awesome" {
		t.Errorf("ReadFile folder/sub.txt failed: %v, %s", err, string(b))
	}

	// Test WalkDir
	var paths []string
	err = fs.WalkDir(tfs, ".", func(path string, d fs.DirEntry, err error) error {
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, p := range paths {
		if p == "src/folder/sub.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("WalkDir didn't find folder/sub.txt, got %v", paths)
	}
}