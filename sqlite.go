package tar

import (
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const MappedStringMark = '\uFFFE'
const MappedStringMarkStr = "\uFFFE"

func decodeUTF8OrMap(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.WriteRune(MappedStringMark)
	for _, c := range b {
		sb.WriteRune(rune(0xE000) + rune(c))
	}
	return sb.String()
}

func encodeMappedString(s string) []byte {
	runes := []rune(s)
	if len(runes) > 0 && runes[0] == MappedStringMark {
		b := make([]byte, len(runes)-1)
		for i, r := range runes[1:] {
			b[i] = byte(r - 0xE000)
		}
		return b
	}
	return []byte(s)
}

type FileNode struct {
	Path           string
	Name           string
	OffsetHeader   int64
	Offset         int64
	Size           int64
	Mode           int64
	ModTime        time.Time
	Type           byte
	LinkName       string
	Uid            int
	Gid            int
	IsTar          bool
	IsSparse       bool
	IsGenerated    bool
	RecursionDepth int
	Xattrs         map[string][]byte
	Acl            []byte
}

func normalizePath(p string) (dir, name string) {
	if p == "" {
		return "/", ""
	}
	var cleanPath string
	if p[0] == '/' {
		cleanPath = path.Clean(p)
	} else {
		cleanPath = path.Clean("/" + p)
	}
	if cleanPath == "/" {
		return "/", ""
	}
	dir, name = path.Split(cleanPath)
	if len(dir) > 1 {
		dir = dir[:len(dir)-1] // Fast, zero-allocation slicing instead of strings.TrimSuffix
	} else {
		dir = "/"
	}
	return dir, name
}

type BlockOffset struct {
	BlockOffset int64 // Compressed block offset (bit or byte depending on format)
	DataOffset  int64 // Uncompressed data offset (byte)
}
