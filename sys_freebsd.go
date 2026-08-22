//go:build freebsd
// +build freebsd

package tar

import (
	"archive/tar"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func mknod(name string, mode uint32, dev uint64) error {
	return unix.Mknod(name, mode, dev)
}

func sysXattrs(path string, hdr *tar.Header) error {
	namespaces := []struct {
		ns     int
		prefix string
	}{
		{unix.EXTATTR_NAMESPACE_USER, "user."},
		{unix.EXTATTR_NAMESPACE_SYSTEM, "system."},
	}

	for _, n := range namespaces {
		// 1. Query size of list (passing 0 and 0 under FreeBSD API)
		sz, err := unix.ExtattrListLink(path, n.ns, 0, 0)
		if err != nil || sz <= 0 {
			continue
		}

		buf := make([]byte, sz)
		sz, err = unix.ExtattrListLink(path, n.ns, uintptr(unsafe.Pointer(&buf[0])), len(buf))
		if err != nil {
			continue
		}

		if hdr.PAXRecords == nil {
			hdr.PAXRecords = make(map[string]string)
		}

		// FreeBSD extattr_list_link format: [length byte][name], not null-terminated
		for i := 0; i < sz; {
			l := int(buf[i])
			i++
			if i+l > sz {
				break
			}
			key := string(buf[i : i+l])
			i += l

			// 2. Query size of attribute value
			valSz, err := unix.ExtattrGetLink(path, n.ns, key, 0, 0)
			if err != nil || valSz <= 0 {
				continue
			}

			val := make([]byte, valSz)
			valSz, err = unix.ExtattrGetLink(path, n.ns, key, uintptr(unsafe.Pointer(&val[0])), len(val))
			if err == nil {
				hdr.PAXRecords["SCHILY.xattr."+n.prefix+key] = string(val[:valSz])
			}
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

			ns := unix.EXTATTR_NAMESPACE_USER
			if strings.HasPrefix(attrName, "system.") {
				ns = unix.EXTATTR_NAMESPACE_SYSTEM
				attrName = strings.TrimPrefix(attrName, "system.")
			} else if strings.HasPrefix(attrName, "user.") {
				attrName = strings.TrimPrefix(attrName, "user.")
			}

			var ptr uintptr
			if len(v) > 0 {
				bytesVal := []byte(v)
				ptr = uintptr(unsafe.Pointer(&bytesVal[0]))
			}

			unix.ExtattrSetLink(path, ns, attrName, ptr, len(v))
		}
	}
	return nil
}
