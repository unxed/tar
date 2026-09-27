//go:build (freebsd || openbsd || netbsd || dragonfly || solaris || illumos) && !tarindex_simple

// This exercises sqlite_disabled.go's all-errors Index stub specifically
// (via a bare &Index{} literal and errNoSqlite comparisons), so it excludes
// tarindex_simple exactly like sqlite_disabled.go itself does: under that
// tag Index is ArcidxIndex, a working implementation, not this stub.

package tar

import "testing"

func TestDisabledSqlite(t *testing.T) {
	if err := IndexArchive("a", "b"); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	_, err := OpenIndex("b")
	if err != errNoSqlite {
		t.Errorf("got %v", err)
	}

	// Создаем экземпляр вручную для проверки обработки вызовов на nil-подобном объекте
	idx := &Index{}
	if err := idx.Close(); err != nil {
		t.Errorf("got %v", err)
	}
	if err := idx.InitMetadata(); err != nil {
		t.Errorf("got %v", err)
	}
	if err := idx.Insert(nil); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if _, err := idx.Lookup("a"); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if _, err := idx.List("a"); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if _, err := idx.RecursiveSize("a"); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if err := idx.InsertBlockOffsets("a", nil); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if _, err := idx.GetClosestBlockOffset("a", 0); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if _, err := idx.GetGzipIndex(); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
	if err := idx.SaveGzipIndex(nil); err != errNoSqlite {
		t.Errorf("got %v", err)
	}
}
