package tar

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndexingOptimization_Threshold(t *testing.T) {
	// Временно подменяем os.Args, чтобы убрать "-test." и активировать порог 4МБ
	oldArgs := os.Args
	os.Args = []string{"zipper"}
	defer func() { os.Args = oldArgs }()

	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0755)

	// 1. Тест: Маленький архив (1МБ) -> Индекса быть НЕ должно
	smallFile := filepath.Join(srcDir, "small.txt")
	os.WriteFile(smallFile, bytes.Repeat([]byte("A"), 1024*1024), 0644)

	arcSmall := filepath.Join(tmpDir, "small.tar.zst")
	a1, _ := NewArchiver(arcSmall, tmpDir, WithArchiverMethod(ZSTD), WithArchiverEmbeddedIndex(true))
	fi1, _ := os.Stat(smallFile)
	a1.Archive(context.Background(), map[string]os.FileInfo{smallFile: fi1})
	a1.Close()

	f1, _ := os.Open(arcSmall)
	stat1, _ := f1.Stat()
	start1, size1, _ := LocateShadowStream(f1, stat1.Size(), ZSTD)
	f1.Close()
	if start1 != 0 || size1 != 0 {
		t.Errorf("Expected no embedded index for 1MB archive, but found one at %d (size %d)", start1, size1)
	}

	// 2. Тест: Большой архив (5МБ) -> Индекс ДОЛЖЕН быть
	largeFile := filepath.Join(srcDir, "large.txt")
	os.WriteFile(largeFile, bytes.Repeat([]byte("B"), 5*1024*1024), 0644)

	arcLarge := filepath.Join(tmpDir, "large.tar.zst")
	a2, _ := NewArchiver(arcLarge, tmpDir, WithArchiverMethod(ZSTD), WithArchiverEmbeddedIndex(true))
	fi2, _ := os.Stat(largeFile)
	a2.Archive(context.Background(), map[string]os.FileInfo{largeFile: fi2})
	a2.Close()

	f2, _ := os.Open(arcLarge)
	stat2, _ := f2.Stat()
	start, size, err2 := LocateShadowStream(f2, stat2.Size(), ZSTD)
	f2.Close()
	if err2 != nil || start == 0 || size == 0 {
		t.Errorf("Expected embedded index for 5MB archive, but it was missing or corrupted: %v", err2)
	}
}
