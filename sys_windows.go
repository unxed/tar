//go:build windows
// +build windows

package tar

import (
    "io"
	"archive/tar"
	"encoding/base64"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func sysHeader(fi os.FileInfo, hdr *tar.Header) {
	// Windows doesn't map cleanly to Unix UID/GID in standard tar
}

func lchown(name string, uid, gid int) error {
	// Not supported/needed for basic Windows extraction
	return nil
}
func createWindowsSymlink(target, link string, isDir bool) error {
	targetPath, _ := syscall.UTF16PtrFromString(target)
	linkPath, _ := syscall.UTF16PtrFromString(link)

	if isDir {
		err := windows.CreateSymbolicLink(linkPath, targetPath, windows.SYMBOLIC_LINK_FLAG_DIRECTORY)
		if err != nil {
			return os.MkdirAll(link, 0755)
		}
		return nil
	}

	err := windows.CreateHardLink(linkPath, targetPath, 0)
	if err != nil {
		err = windows.CreateSymbolicLink(linkPath, targetPath, 0)
		if err != nil {
			return copyFileContents(target, link)
		}
	}
	return nil
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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

type hardlinkKey struct{}

func getHardLinkTarget(fi os.FileInfo, seen map[hardlinkKey]string) string {
	return ""
}

func rememberHardLink(fi os.FileInfo, relPath string, seen map[hardlinkKey]string) {}

var (
	modadvapi32                    = syscall.NewLazyDLL("advapi32.dll")
	procGetFileSecurityW           = modadvapi32.NewProc("GetFileSecurityW")
	procSetFileSecurityW           = modadvapi32.NewProc("SetFileSecurityW")
	modkernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procFindFirstStreamW           = modkernel32.NewProc("FindFirstStreamW")
	procFindNextStreamW            = modkernel32.NewProc("FindNextStreamW")
	procFindClose                  = modkernel32.NewProc("FindClose")
	procSetFileInformationByHandle = modkernel32.NewProc("SetFileInformationByHandle")
)

type win32FindStreamData struct {
	StreamSize int64
	StreamName [260 + 36]uint16
}

func getFileSecurity(path string) ([]byte, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const secInfo = 7 // OWNER_SECURITY_INFORMATION | GROUP_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION
	var needed uint32
	r1, _, err := procGetFileSecurityW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(secInfo),
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
	)
	if r1 == 0 {
		if err != windows.ERROR_INSUFFICIENT_BUFFER {
			return nil, err
		}
	}
	if needed == 0 {
		return nil, nil
	}
	buf := make([]byte, needed)
	r1, _, err = procGetFileSecurityW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(secInfo),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
	)
	if r1 == 0 {
		return nil, err
	}
	return buf, nil
}

func applyNtfsAcl(path string, acl []byte) error {
	if len(acl) == 0 {
		return nil
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const secInfo = 7 // OWNER_SECURITY_INFORMATION | GROUP_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION
	r1, _, err := procSetFileSecurityW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(secInfo),
		uintptr(unsafe.Pointer(&acl[0])),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func getAlternativeDataStreams(path string) ([]string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var data win32FindStreamData
	const findStreamInfoStandard = 0
	h, _, err := procFindFirstStreamW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(findStreamInfoStandard),
		uintptr(unsafe.Pointer(&data)),
		0,
	)
	if h == uintptr(syscall.InvalidHandle) {
		return nil, nil
	}
	defer procFindClose.Call(h)

	var streams []string
	for {
		name := syscall.UTF16ToString(data.StreamName[:])
		if name != "::$DATA" && name != "" {
			cleaned := name
			if strings.HasSuffix(cleaned, ":$DATA") {
				cleaned = strings.TrimSuffix(cleaned, ":$DATA")
			}
			streams = append(streams, cleaned)
		}

		r1, _, _ := procFindNextStreamW.Call(
			h,
			uintptr(unsafe.Pointer(&data)),
		)
		if r1 == 0 {
			break
		}
	}
	return streams, nil
}

func sysXattrs(path string, hdr *tar.Header) error {
	acl, err := getFileSecurityFunc(path)
	if err == nil && len(acl) > 0 {
		if hdr.PAXRecords == nil {
			hdr.PAXRecords = make(map[string]string)
		}
		hdr.PAXRecords["MSWINDOWS.raw_sd"] = base64.StdEncoding.EncodeToString(acl)
	}
	return nil
}

func applyXattrs(path string, hdr *tar.Header) error {
	if len(hdr.PAXRecords) > 0 {
		if rawSD, ok := hdr.PAXRecords["MSWINDOWS.raw_sd"]; ok {
			if acl, err := base64.StdEncoding.DecodeString(rawSD); err == nil {
				applyNtfsAclFunc(path, acl)
			}
		}
	}
	return nil
}

func resolveIds(hdr *Header, numericOwner bool) (int, int) {
	return hdr.Uid, hdr.Gid
}

func preallocate(f *os.File, size int64) error {
	if size <= 1024*1024 {
		return nil
	}
	var allocInfo int64 = size
	procSetFileInformationByHandle.Call(
		f.Fd(),
		5, // FileAllocationInfo
		uintptr(unsafe.Pointer(&allocInfo)),
		8, // sizeof(int64)
	)
	return f.Truncate(size)
}
