//go:build linux
// +build linux

package tar

import (
	"archive/tar"
	"fmt"
	"os"
	"os/user"
	"strings"
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

	hdr.AccessTime = time.Unix(sys.Atim.Sec, sys.Atim.Nsec)
	hdr.ChangeTime = time.Unix(sys.Ctim.Sec, sys.Ctim.Nsec)

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

func mknod(name string, mode uint32, dev uint64) error {
	return unix.Mknod(name, mode, int(dev))
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
func sysXattrs(path string, hdr *tar.Header) error {
	// Сначала запрашиваем размер списка xattrs.
	// Большинство файлов в Linux не имеют xattrs. Если их нет, мы выходим за 1 быстрый syscall.
	sz, err := unix.Llistxattr(path, nil)
	if err != nil || sz <= 0 {
		return nil
	}

	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil
	}

	var keys []string
	for i, j := 0, 0; i < sz; i++ {
		if buf[i] == 0 {
			keys = append(keys, string(buf[j:i]))
			j = i + 1
		}
	}

	if len(keys) > 0 && hdr.PAXRecords == nil {
		hdr.PAXRecords = make(map[string]string)
	}

	for _, key := range keys {
		valSz, err := unix.Lgetxattr(path, key, nil)
		if err != nil || valSz < 0 {
			continue
		}
		val := make([]byte, valSz)
		_, err = unix.Lgetxattr(path, key, val)
		if err == nil {
			// SCHILY.xattr is the standard namespace used by GNU tar and bsdtar for POSIX ACLs, SELinux, and user xattrs
			hdr.PAXRecords["SCHILY.xattr."+key] = string(val)
		}
	}
	return nil
}

func applyXattrs(path string, hdr *tar.Header) error {
	if len(hdr.PAXRecords) == 0 {
		return nil
	}
	for k, v := range hdr.PAXRecords {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			attrName := strings.TrimPrefix(k, "SCHILY.xattr.")
			// We use Lsetxattr to properly support symlinks
			unix.Lsetxattr(path, attrName, []byte(v), 0)
		} else if strings.HasPrefix(k, "LIBARCHIVE.xattr.") {
			attrName := strings.TrimPrefix(k, "LIBARCHIVE.xattr.")
			unix.Lsetxattr(path, attrName, []byte(v), 0)
		}
	}
	return nil
}

