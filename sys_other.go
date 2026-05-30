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
func getFileSecurity(path string) ([]byte, error) {
	return nil, nil
}

func applyNtfsAcl(path string, acl []byte) error {
	return nil
}

func getAlternativeDataStreams(path string) ([]string, error) {
	return nil, nil
}

func sysXattrs(path string, hdr *tar.Header) error {
	return nil
}

func applyXattrs(path string, hdr *tar.Header) error {
	return nil
}
