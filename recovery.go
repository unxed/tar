package tar

import (
	"encoding/binary"
	"io"
	"os"

	"github.com/unxed/par2"
)

var magicF4Recovery = []byte("F4RECOVERY\x00\x00\x00\x00\x00\x00")

// checkF4Recovery проверяет наличие F4RECOVERY футера в конце файла и виртуально обрезает границы
// ридера для низкоуровневых парсеров, чтобы они не видели избыточные данные.
func checkF4Recovery(ra io.ReaderAt, size int64) (io.ReaderAt, int64, error) {
	if size < 32 {
		return ra, size, nil
	}
	var footer [32]byte
	if _, err := ra.ReadAt(footer[:], size-32); err != nil {
		return ra, size, nil
	}
	if string(footer[16:32]) == string(magicF4Recovery) {
		origSize := int64(binary.LittleEndian.Uint64(footer[8:16]))
		if origSize < 0 || origSize > size {
			return ra, size, nil
		}
		return io.NewSectionReader(ra, 0, origSize), origSize, nil
	}
	return ra, size, nil
}

// AppendTarRecoveryRecord генерирует PAR2 для TAR-архива и дописывает его в хвост с F4-футером
func AppendTarRecoveryRecord(filename string, pct int) error {
	parData, err := par2.GeneratePAR2Data(filename, pct)
	if err != nil || len(parData) == 0 {
		return err
	}
	fi, err := os.Stat(filename)
	if err != nil {
		return err
	}
	origSize := fi.Size()

	out, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.Write(parData); err != nil {
		return err
	}

	footer := make([]byte, 32)
	binary.LittleEndian.PutUint64(footer[0:8], uint64(len(parData)))
	binary.LittleEndian.PutUint64(footer[8:16], uint64(origSize))
	copy(footer[16:32], magicF4Recovery)

	_, err = out.Write(footer)
	return err
}