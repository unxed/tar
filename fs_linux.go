//go:build linux
// +build linux

package tar

import (
	"golang.org/x/sys/unix"
	"os"
)

func preallocate(f *os.File, size int64) error {
	if size <= 1024*1024 {
		return nil
	}
	return unix.Fallocate(int(f.Fd()), 0, 0, size)
}
