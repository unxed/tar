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
	"sync"

	"golang.org/x/sys/unix"
)

var (
	userCache   = make(map[uint32]string)
	groupCache  = make(map[uint32]string)
	idCacheLock sync.RWMutex
)

func getUsername(uid uint32) string {
	idCacheLock.RLock()
	name, ok := userCache[uid]
	idCacheLock.RUnlock()
	if ok {
		return name
	}
	idCacheLock.Lock()
	defer idCacheLock.Unlock()
	if name, ok := userCache[uid]; ok {
		return name
	}
	if u, err := user.LookupId(fmt.Sprint(uid)); err == nil {
		userCache[uid] = u.Username
		return u.Username
	}
	userCache[uid] = ""
	return ""
}

func getGroupname(gid uint32) string {
	idCacheLock.RLock()
	name, ok := groupCache[gid]
	idCacheLock.RUnlock()
	if ok {
		return name
	}
	idCacheLock.Lock()
	defer idCacheLock.Unlock()
	if name, ok := groupCache[gid]; ok {
		return name
	}
	if g, err := user.LookupGroupId(fmt.Sprint(gid)); err == nil {
		groupCache[gid] = g.Name
		return g.Name
	}
	groupCache[gid] = ""
	return ""
}
func sysHeader(fi os.FileInfo, hdr *tar.Header) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}

	hdr.Uid = int(sys.Uid)
	hdr.Gid = int(sys.Gid)

	if uname := getUsername(sys.Uid); uname != "" {
		hdr.Uname = uname
	}
	if gname := getGroupname(sys.Gid); gname != "" {
		hdr.Gname = gname
	}

	hdr.AccessTime = time.Unix(int64(sys.Atimespec.Sec), int64(sys.Atimespec.Nsec))
	hdr.ChangeTime = time.Unix(int64(sys.Ctimespec.Sec), int64(sys.Ctimespec.Nsec))

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

