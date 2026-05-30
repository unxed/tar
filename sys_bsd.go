//go:build freebsd || darwin
// +build freebsd darwin

package tar

import (
	"archive/tar"
	"fmt"
	"os"
	"os/user"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func sysHeader(fi os.FileInfo, hdr *tar.Header) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}

	hdr.Uid = int(sys.Uid)
	hdr.Gid = int(sys.Gid)

	if u, err := user.LookupId(fmt.Sprint(sys.Uid)); err == nil {
		hdr.Uname = u.Username
	}
	if g, err := user.LookupGroupId(fmt.Sprint(sys.Gid)); err == nil {
		hdr.Gname = g.Name
	}

	hdr.AccessTime = time.Unix(sys.Atimespec.Sec, sys.Atimespec.Nsec)
	hdr.ChangeTime = time.Unix(sys.Ctimespec.Sec, sys.Ctimespec.Nsec)

	switch fi.Mode() & os.ModeType {
	case os.ModeDevice | os.ModeCharDevice:
		hdr.Typeflag = tar.TypeChar
		hdr.Devmajor = int64(unix.Major(uint64(sys.Rdev)))
		hdr.Devminor = int64(unix.Minor(uint64(sys.Rdev)))
	case os.ModeDevice:
		hdr.Typeflag = tar.TypeBlock
		hdr.Devmajor = int64(unix.Major(uint64(sys.Rdev)))
		hdr.Devminor = int64(unix.Minor(uint64(sys.Rdev)))
	case os.ModeNamedPipe:
		hdr.Typeflag = tar.TypeFifo
	}
}

func extractSpecialFile(path string, hdr *tar.Header) error {
	os.Remove(path) // Ignore error
	mode := uint32(hdr.Mode) & 0777
	if hdr.Typeflag == tar.TypeChar {
		mode |= unix.S_IFCHR
	} else if hdr.Typeflag == tar.TypeBlock {
		mode |= unix.S_IFBLK
	} else if hdr.Typeflag == tar.TypeFifo {
		mode |= unix.S_IFIFO
	}
	dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
	return mknod(path, mode, dev)
}

