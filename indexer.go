package tar

import (
	"io"
	"strings"
)

type trackingReader struct {
	r   io.Reader
	pos int64
}

func (t *trackingReader) Read(p []byte) (n int, err error) {
	n, err = t.r.Read(p)
	t.pos += int64(n)
	return n, err
}

func insertParentFolders(p string, batch *[]FileNode, seen map[string]bool) {
	dir, name := normalizePath(p)
	if dir == "/" && name == "" {
		return
	}

	curr := dir
	for curr != "/" && curr != "" {
		if seen[curr] {
			break // If we've seen this folder, we definitely processed all its parents already
		}
		seen[curr] = true

		// Highly optimized string slicing instead of recursive normalizePath calls
		lastSlash := strings.LastIndexByte(curr, '/')
		var pDir, pName string
		if lastSlash == 0 {
			pDir = "/"
			pName = curr[1:]
		} else if lastSlash > 0 {
			pDir = curr[:lastSlash]
			pName = curr[lastSlash+1:]
		} else {
			break
		}

		*batch = append(*batch, FileNode{
			Path:        pDir,
			Name:        pName,
			Type:        TypeDir,
			Mode:        0755 | 040000,
			IsGenerated: true,
		})
		curr = pDir
	}
}

// IndexArchive implementation moved to indexer_enabled.go and indexer_disabled.go