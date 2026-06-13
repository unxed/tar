//go:build freebsd || openbsd || netbsd || dragonfly || solaris || illumos
// +build freebsd openbsd netbsd dragonfly solaris illumos

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