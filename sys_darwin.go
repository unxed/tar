//go:build darwin
// +build darwin

package tar

import (
	"archive/tar"
	"strings"
)

func mknod(name string, mode uint32, dev uint64) error {
	return unix.Mknod(name, mode, int(dev))
}

func sysXattrs(path string, hdr *tar.Header) error {
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
		if err != nil || valSz <= 0 {
			continue
		}
		val := make([]byte, valSz)
		sz, err = unix.Lgetxattr(path, key, val)
		if err == nil {
			hdr.PAXRecords["SCHILY.xattr."+key] = string(val[:sz])
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
			unix.Lsetxattr(path, attrName, []byte(v), 0)
		} else if strings.HasPrefix(k, "LIBARCHIVE.xattr.") {
			attrName := strings.TrimPrefix(k, "LIBARCHIVE.xattr.")
			unix.Lsetxattr(path, attrName, []byte(v), 0)
		}
	}
	return nil
}