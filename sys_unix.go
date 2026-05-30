//go:build !windows
// +build !windows

package tar

import (
	"fmt"
	"os"
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
func resolveIds(hdr *Header, numericOwner bool) (int, int) {
	uid, gid := hdr.Uid, hdr.Gid
	if !numericOwner {
		if hdr.Uname != "" {
			if u, err := lookupUser(hdr.Uname); err == nil {
				uid = u
			}
		}
		if hdr.Gname != "" {
			if g, err := lookupGroup(hdr.Gname); err == nil {
				gid = g
			}
		}
	}
	return uid, gid
}
