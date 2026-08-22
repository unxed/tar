//go:build !linux && !windows
// +build !linux,!windows

package tar

import "os"

func preallocate(f *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	return f.Truncate(size)
}
