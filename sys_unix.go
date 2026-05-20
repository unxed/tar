//go:build !windows
// +build !windows

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

func getHardLinkTarget(fi os.FileInfo, seen map[string]string) string {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink <= 1 {
		return ""
	}
	key := fmt.Sprintf("%d:%d", sys.Dev, sys.Ino)
	if target, exists := seen[key]; exists {
		return target
	}
	return ""
}

func rememberHardLink(fi os.FileInfo, relPath string, seen map[string]string) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink <= 1 {
		return
	}
	key := fmt.Sprintf("%d:%d", sys.Dev, sys.Ino)
	if _, exists := seen[key]; !exists {
		seen[key] = relPath
	}
}

// sysHeader extracts Unix-specific metadata (UID, GID, Uname, Gname, Atime, Ctime, Devmajor/minor).
func sysHeader(fi os.FileInfo, hdr *tar.Header) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}

	hdr.Uid = int(sys.Uid)
	hdr.Gid = int(sys.Gid)

	// User and group names
	if u, err := user.LookupId(fmt.Sprint(sys.Uid)); err == nil {
		hdr.Uname = u.Username
	}
	if g, err := user.LookupGroupId(fmt.Sprint(sys.Gid)); err == nil {
		hdr.Gname = g.Name
	}

	// Extended timestamps
	hdr.AccessTime = time.Unix(sys.Atim.Sec, sys.Atim.Nsec)
	hdr.ChangeTime = time.Unix(sys.Ctim.Sec, sys.Ctim.Nsec)

	// Special files (Devices, FIFOs)
	switch fi.Mode() & os.ModeType {
	case os.ModeDevice | os.ModeCharDevice:
		hdr.Typeflag = tar.TypeChar
		hdr.Devmajor = int64(unix.Major(sys.Rdev))
		hdr.Devminor = int64(unix.Minor(sys.Rdev))
	case os.ModeDevice:
		hdr.Typeflag = tar.TypeBlock
		hdr.Devmajor = int64(unix.Major(sys.Rdev))
		hdr.Devminor = int64(unix.Minor(sys.Rdev))
	case os.ModeNamedPipe:
		hdr.Typeflag = tar.TypeFifo
	}
}

func lchown(name string, uid, gid int) error {
	return os.Lchown(name, uid, gid)
}

func lchtimes(name string, atime, mtime time.Time) error {
	at := unix.NsecToTimeval(atime.UnixNano())
	mt := unix.NsecToTimeval(mtime.UnixNano())
	tv := [2]unix.Timeval{at, mt}
	err := unix.Lutimes(name, tv[:])
	if err != nil {
		return &os.PathError{Op: "lchtimes", Path: name, Err: err}
	}
	return nil
}

func mknod(name string, mode uint32, dev int) error {
	return unix.Mknod(name, mode, dev)
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
	dev := int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))
	return mknod(path, mode, dev)
}