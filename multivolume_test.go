package tar

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMultiVolumeReader_Tar(t *testing.T) {
	tmp := t.TempDir()

	// Создаем два тома: .001 и .002
	vol1Path := filepath.Join(tmp, "test.tar.001")
	vol2Path := filepath.Join(tmp, "test.tar.002")

	os.WriteFile(vol1Path, []byte("PART1"), 0644)
	os.WriteFile(vol2Path, []byte("PART2"), 0644)

	// Открываем через базовый путь (без суффикса номера)
	ra, size, err := OpenMultiVolume(filepath.Join(tmp, "test.tar"), os.O_RDONLY)
	if err != nil {
		t.Fatalf("failed to open multivolume tar: %v", err)
	}
	defer ra.Close()

	if size != 10 {
		t.Errorf("expected size 10, got %d", size)
	}

	buf := make([]byte, 10)
	n, err := ra.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt error: %v", err)
	}
	if n != 10 || string(buf) != "PART1PART2" {
		t.Errorf("multivolume read failed: got %q", string(buf))
	}
}