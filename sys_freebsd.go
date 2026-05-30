//go:build freebsd
// +build freebsd

package tar

import "golang.org/x/sys/unix"

import (
	"archive/tar"
	"strings"

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
		sz, err := unix.ExtattrListLink(n.ns, path, nil)
		if err != nil || sz <= 0 {
			continue
		}
		buf := make([]byte, sz)
		sz, err = unix.ExtattrListLink(n.ns, path, buf)
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

			valSz, err := unix.ExtattrGetLink(n.ns, path, key, nil)
			if err != nil || valSz <= 0 {
				continue
			}
			val := make([]byte, valSz)
			valSz, err = unix.ExtattrGetLink(n.ns, path, key, val)
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

			unix.ExtattrSetLink(ns, path, attrName, []byte(v))
		}
	}
	return nil
}