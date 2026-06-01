package tar

import (
	"archive/tar"
	"io"
	"os"
	"strings"
	"unicode/utf8"
	"encoding/base64"
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

// IndexArchive scans the archive and creates a ratarmount-compatible SQLite index.
func IndexArchive(archivePath, indexPath string) error {
	ra, size, closer, err := openMultiVolume(archivePath)
	if err != nil {
		return err
	}
	defer closer.Close()

	method, err := DetectFormat(ra)
	if err != nil {
		return err
	}

	var rd io.Reader = io.NewSectionReader(ra, 0, size)
	if method != Store {
		di, ok := decompressors.Load(method)
		if !ok {
			return ErrAlgorithm
		}
		dcomp, err := di.(Decompressor).Decompress(rd)
		if err != nil {
			return err
		}
		defer dcomp.Close()
		rd = dcomp
	}

	tr := &trackingReader{r: rd}
	trd := tar.NewReader(tr)

	os.Remove(indexPath)
	idx, err := OpenIndex(indexPath)
	if err != nil {
		return err
	}
	defer idx.Close()

	// Write ratarmount metadata
	idx.db.Exec(`INSERT INTO versions (name, version, major, minor, patch) VALUES ('ratarmount', '1.3.0', 1, 3, 0)`)
	idx.db.Exec(`INSERT INTO versions (name, version, major, minor, patch) VALUES ('index', '0.7.0', 0, 7, 0)`)

	var batch []FileNode
	seenParents := make(map[string]bool)

	var headerOffset int64
	for {
		headerOffset = tr.pos
		hdr, err := trd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		hdrName := hdr.Name
		if !utf8.ValidString(hdrName) {
			hdrName = decodeUTF8OrMap([]byte(hdrName))
		}
		hdrLinkname := hdr.Linkname
		if !utf8.ValidString(hdrLinkname) {
			hdrLinkname = decodeUTF8OrMap([]byte(hdrLinkname))
		}

		insertParentFolders(hdrName, &batch, seenParents)
		dir, name := normalizePath(hdrName)

		var xattrs map[string][]byte
		var acl []byte
		for k, v := range hdr.PAXRecords {
			if strings.HasPrefix(k, "SCHILY.xattr.") {
				if xattrs == nil { xattrs = make(map[string][]byte) }
				xattrs[strings.TrimPrefix(k, "SCHILY.xattr.")] = []byte(v)
			} else if strings.HasPrefix(k, "LIBARCHIVE.xattr.") {
				if xattrs == nil { xattrs = make(map[string][]byte) }
				xattrs[strings.TrimPrefix(k, "LIBARCHIVE.xattr.")] = []byte(v)
			} else if k == "MSWINDOWS.raw_sd" {
				dec, err := base64.StdEncoding.DecodeString(v)
				if err == nil {
					acl = dec
				} else {
					acl = []byte(v)
				}
			}
		}

		node := FileNode{
			Path:         dir,
			Name:         name,
			OffsetHeader: headerOffset,
			Offset:       tr.pos, // Stream position is precisely at the data block start now
			Size:         hdr.Size,
			Mode:         int64(hdr.Mode),
			ModTime:      hdr.ModTime,
			Type:         hdr.Typeflag,
			LinkName:     hdrLinkname,
			Uid:          hdr.Uid,
			Gid:          hdr.Gid,
			IsSparse:     hdr.Typeflag == TypeGNUSparse || hdr.Typeflag == 'S',
			IsTar:        true,
			Xattrs:       xattrs,
			Acl:          acl,
		}
		batch = append(batch, node)

		if len(batch) >= 5000 {
			if err := idx.Insert(batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}

	if len(batch) > 0 {
		if err := idx.Insert(batch); err != nil {
			return err
		}
	}

	// Save compression indices at the end of indexing (O(1) database writes)
	if exporter, ok := rd.(BlockOffsetExporter); ok {
		table := ""
		if method == ZSTD {
			table = "zstdblocks"
		} else if method == BZIP2 {
			table = "bzip2blocks"
		}
		if table != "" {
			idx.InsertBlockOffsets(table, exporter.ExportBlockOffsets())
		}
	}

	if exporter, ok := rd.(GzipIndexExporter); ok {
		if data, err := exporter.ExportGzipIndex(); err == nil {
			idx.SaveGzipIndex(data)
		}
	}

	return nil
}