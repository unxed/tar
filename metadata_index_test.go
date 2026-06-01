package tar

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataIndexing_XattrsAndACL(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "metadata.tar")
	indexPath := filepath.Join(tmpDir, "metadata.sqlite")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)

	// Создаем заголовок с PAX-записяsми для xattrs и ACL
	hdr := &Header{
		Name:     "meta.txt",
		Size:     4,
		Typeflag: TypeReg,
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.test": "value123",
			"MSWINDOWS.raw_sd":       "base64-acl-data",
		},
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	// Индексируем
	err := IndexArchive(archivePath, indexPath)
	if err != nil {
		t.Fatalf("IndexArchive failed: %v", err)
	}

	idx, _ := OpenIndex(indexPath)
	defer idx.Close()

	node, err := idx.Lookup("meta.txt")
	if err != nil {
		t.Fatal(err)
	}

	if string(node.Xattrs["user.test"]) != "value123" {
		t.Errorf("Xattrs not found in index: got %v", node.Xattrs)
	}
	if string(node.Acl) != "base64-acl-data" {
		t.Errorf("ACL not found in index: got %q", string(node.Acl))
	}
}