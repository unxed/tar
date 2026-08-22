//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos

package tar

import (
	"archive/tar"
	"bufio"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

func IndexArchive(archivePath, indexPath string) error {
	ra, size, err := OpenMultiVolume(archivePath, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer ra.Close()
	method, err := DetectFormat(ra)
	if err != nil {
		return err
	}
	var rd io.Reader = io.NewSectionReader(ra, 0, size)
	if method != Store {
		if method == GZIP {
			gtr, err := NewGzipIndexTrackingReader(rd)
			if err != nil {
				return err
			}
			defer gtr.Close()
			rd = gtr
		} else {
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
	}

	// ОПТИМИЗАЦИЯ: Буферизируем поток перед передачей в TAR-индексатор.
	// Без этого индексатор будет читать заголовки файлов (512 байт) напрямую из декомпрессора.
	bufferedRd := bufio.NewReaderSize(rd, 1024*1024)
	tr := &trackingReader{r: bufferedRd}
	trd := tar.NewReader(tr)
	os.Remove(indexPath)
	idx, err := OpenIndex(indexPath)
	if err != nil {
		return err
	}
	defer idx.Close()
	idx.InitMetadata()
	var batch []FileNode
	seenParents := make(map[string]bool)
	var headerOffset int64
	for {
		headerOffset = tr.pos
		hdr, err := trd.Next()
		if err == io.EOF {
			// Check if there's another tar archive appended after double-zero blocks
			newReader := tar.NewReader(tr)
			newHdr, newErr := newReader.Next()
			if newErr == nil {
				trd = newReader
				hdr = newHdr
			} else {
				break
			}
		} else if err != nil {
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
				if xattrs == nil {
					xattrs = make(map[string][]byte)
				}
				xattrs[strings.TrimPrefix(k, "SCHILY.xattr.")] = []byte(v)
			} else if strings.HasPrefix(k, "LIBARCHIVE.xattr.") {
				if xattrs == nil {
					xattrs = make(map[string][]byte)
				}
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
			Path: dir, Name: name, OffsetHeader: headerOffset, Offset: tr.pos, Size: hdr.Size,
			Mode: int64(hdr.Mode), ModTime: hdr.ModTime, Type: hdr.Typeflag, LinkName: hdrLinkname,
			Uid: hdr.Uid, Gid: hdr.Gid, IsSparse: hdr.Typeflag == TypeGNUSparse || hdr.Typeflag == 'S',
			IsTar: true, Xattrs: xattrs, Acl: acl,
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
