//go:build !windows
// +build !windows

package tar

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type hardlinkKey struct {
	dev uint64
	ino uint64
}

func getHardLinkTarget(fi os.FileInfo, seen map[hardlinkKey]string) string {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink <= 1 {
		return ""
	}
	key := hardlinkKey{dev: uint64(sys.Dev), ino: uint64(sys.Ino)}
	if target, exists := seen[key]; exists {
		return target
	}
	return ""
}

func rememberHardLink(fi os.FileInfo, relPath string, seen map[hardlinkKey]string) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink <= 1 {
		return
	}
	key := hardlinkKey{dev: uint64(sys.Dev), ino: uint64(sys.Ino)}
	if _, exists := seen[key]; !exists {
		seen[key] = relPath
	}
}

func lchown(name string, uid, gid int) error {
	return os.Lchown(name, uid, gid)
}
func getFileSecurity(path string) ([]byte, error) {
	return nil, nil
}

func applyNtfsAcl(path string, acl []byte) error {
	return nil
}

func getAlternativeDataStreams(path string) ([]string, error) {
	return nil, nil
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
func lookupUser(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(u.Uid)
}

func lookupGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(g.Gid)
}
func createWindowsSymlink(target, link string, isDir bool) error {
	return nil // No-op on Unix, never called due to runtime.GOOS check
}
