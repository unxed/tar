//go:build !linux && !freebsd && !darwin && !windows
// +build !linux,!freebsd,!darwin,!windows

package tar

import (
	"archive/tar"
	"os"
)

func sysHeader(fi os.FileInfo, hdr *tar.Header) {
	// Fallback stub for other platforms
}

func extractSpecialFile(path string, hdr *tar.Header) error {
	return nil
}

func sysXattrs(path string, hdr *tar.Header) error {
	return nil
}

func applyXattrs(path string, hdr *tar.Header) error {
	return nil
}

func createWindowsSymlink(target, link string, isDir bool) error {
	return nil // No-op, never called on non-Windows
}
