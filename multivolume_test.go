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

func TestMultiVolumeWriter_Roundtrip(t *testing.T) {
	tmp := t.TempDir()
	mainPath := filepath.Join(tmp, "test_write.tar")
	splitSize := int64(10) // 10 bytes per volume

	mvw, err := NewMultiVolumeWriter(mainPath, splitSize)
	if err != nil {
		t.Fatalf("failed to create MultiVolumeWriter: %v", err)
	}

	data := []byte("abcdefghijklmnopqrstuvwxyz") // 26 bytes -> 001(10), 002(10), 003(6)
	if _, err := mvw.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := mvw.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	if _, err := os.Stat(mainPath + ".001"); err != nil {
		t.Errorf("missing volume .001")
	}
	if _, err := os.Stat(mainPath + ".002"); err != nil {
		t.Errorf("missing volume .002")
	}
	if _, err := os.Stat(mainPath + ".003"); err != nil {
		t.Errorf("missing volume .003")
	}

	mvr, totalSize, err := OpenMultiVolume(mainPath, os.O_RDONLY)
	if err != nil {
		t.Fatalf("failed to open multi-volume reader: %v", err)
	}
	defer mvr.Close()

	if totalSize != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), totalSize)
	}

	buf := make([]byte, len(data))
	if _, err := mvr.ReadAt(buf, 0); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(buf) != string(data) {
		t.Errorf("content mismatch: got %q, want %q", string(buf), string(data))
	}
}
