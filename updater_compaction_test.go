package tar

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdater_Compaction(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "compact.tar")

	// 1. Создаем архив с двумя файлами
	f, _ := os.Create(tarPath)
	tw := NewWriter(f)

	data1 := make([]byte, 10000) // Большой файл
	tw.WriteHeader(&Header{Name: "file1.txt", Size: int64(len(data1))})
	tw.Write(data1)

	data2 := []byte("keep_me") // Второй файл, который должен сдвинуться
	tw.WriteHeader(&Header{Name: "file2.txt", Size: int64(len(data2))})
	tw.Write(data2)
	tw.Close()
	f.Close()

	initialStat, _ := os.Stat(tarPath)
	initialSize := initialStat.Size()

	// 2. Перезаписываем file1.txt маленьким контентом
	fRW, _ := os.OpenFile(tarPath, os.O_RDWR, 0644)
	updater, _ := NewUpdater(fRW, APPEND_MODE_OVERWRITE)
	updater.Append("file1.txt", 5, []byte("short"))
	updater.Close()
	fRW.Close()

	// 3. Проверяем, что размер уменьшился
	finalStat, _ := os.Stat(tarPath)
	if finalStat.Size() >= initialSize {
		t.Errorf("Archive did not shrink: old=%d, new=%d", initialSize, finalStat.Size())
	}

	// 4. Проверяем целостность file2.txt после сдвига
	tr, _ := OpenReader(tarPath)
	defer tr.Close()
	found2 := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF { break }
		if hdr.Name == "file2.txt" {
			found2 = true
			content, _ := io.ReadAll(tr)
			if string(content) != "keep_me" {
				t.Errorf("file2.txt corruption: got %q", string(content))
			}
		}
	}
	if !found2 {
		t.Error("file2.txt lost after compaction")
	}
}