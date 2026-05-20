//go:build darwin
// +build darwin

package tar

import "golang.org/x/sys/unix"

func mknod(name string, mode uint32, dev uint64) error {
	return unix.Mknod(name, mode, int(dev))
}