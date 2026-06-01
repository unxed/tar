package tar

import (
	"os"
	"testing"
	"time"
)

func TestIndexXattrsAndAcl(t *testing.T) {
	idxPath := "test_index_xattrs.sqlite"
	os.Remove(idxPath)
	defer os.Remove(idxPath)

	idx, err := OpenIndex(idxPath)
	if err != nil {
		t.Fatalf("OpenIndex failed: %v", err)
	}
	defer idx.Close()

	dir, name := normalizePath("dir/file.txt")
	node := FileNode{
		Path:         dir,
		Name:         name,
		OffsetHeader: 123,
		ModTime:      time.Now(),
		Xattrs: map[string][]byte{
			"user.test": []byte("value"),
		},
		Acl: []byte{0x01, 0x02, 0x03},
	}

	if err := idx.Insert([]FileNode{node}); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	res, err := idx.Lookup("dir/file.txt")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	if string(res.Xattrs["user.test"]) != "value" {
		t.Errorf("Xattrs mismatch: got %v", res.Xattrs)
	}
	if len(res.Acl) != 3 || res.Acl[0] != 0x01 {
		t.Errorf("Acl mismatch: got %v", res.Acl)
	}
}