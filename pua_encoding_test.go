package tar

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPUA_EncodingPreservation(t *testing.T) {
	tmpDir := t.TempDir()
	// Имя файла с бинарной "грязью" (невалидный UTF-8)
	rawName := "bad_utf8_\xff\xfe_name.txt"
	archivePath := filepath.Join(tmpDir, "pua.tar")
	dstDir := filepath.Join(tmpDir, "extract")

	f, _ := os.Create(archivePath)
	tw := NewWriter(f)
	tw.WriteHeader(&Header{Name: rawName, Size: 4})
	tw.Write([]byte("data"))
	tw.Close()
	f.Close()

	// Извлекаем
	e, _ := NewExtractor(archivePath, dstDir)
	err := e.Extract(context.Background())
	if err != nil {
		t.Fatalf("Extraction failed: %v", err)
	}

	// Проверяем, что на диске имя восстановилось в исходные байты
	expectedPath := filepath.Join(dstDir, rawName)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("Filename not restored correctly: %v", err)
		// Проверим, что там на самом деле лежит
		files, _ := os.ReadDir(dstDir)
		for _, fl := range files {
			t.Logf("Actual file on disk: %q", fl.Name())
		}
	}
}