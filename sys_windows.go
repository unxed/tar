//go:build windows
// +build windows

package tar

import (
	"archive/tar"
	"os"
	"time"
)

func sysHeader(fi os.FileInfo, hdr *tar.Header) {
	// Windows doesn't map cleanly to Unix UID/GID in standard tar
}

func lchown(name string, uid, gid int) error {
	// Not supported/needed for basic Windows extraction
	return nil
}

func lchtimes(name string, atime, mtime time.Time) error {
	// Windows doesn't easily support Lutimes without deep API calls.
	// Fallback to normal Chtimes (which follows symlinks).
	fi, err := os.Lstat(name)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil // Skip symlink times on Windows
	}
	return os.Chtimes(name, atime, mtime)
}

func mknod(name string, mode uint32, dev int) error {
	// Not supported
	return nil
}

func extractSpecialFile(path string, hdr *tar.Header) error {
	return nil
}

func getHardLinkTarget(fi os.FileInfo, seen map[string]string) string {
	return ""
}

func rememberHardLink(fi os.FileInfo, relPath string, seen map[string]string) {}