package tar

import (
	"archive/tar"
	"io"
	"os"
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
		pDir, pName := normalizePath(curr)
		if !seen[curr] {
			seen[curr] = true
			*batch = append(*batch, FileNode{
				Path:        pDir,
				Name:        pName,
				Type:        TypeDir,
				Mode:        0755 | 040000,
				IsGenerated: true,
			})
		}
		curr = pDir
	}
}

// IndexArchive scans the archive and creates a ratarmount-compatible SQLite index.
func IndexArchive(archivePath, indexPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	method, err := DetectFormat(f)
	if err != nil {
		return err
	}
	f.Seek(0, io.SeekStart)

	var rd io.Reader = f
	if method != Store {
		di, ok := decompressors.Load(method)
		if !ok {
			return ErrAlgorithm
		}
		dcomp, err := di.(Decompressor).Decompress(f)
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

		insertParentFolders(hdr.Name, &batch, seenParents)
		dir, name := normalizePath(hdr.Name)

		node := FileNode{
			Path:         dir,
			Name:         name,
			OffsetHeader: headerOffset,
			Offset:       tr.pos, // Stream position is precisely at the data block start now
			Size:         hdr.Size,
			Mode:         int64(hdr.Mode),
			ModTime:      hdr.ModTime,
			Type:         hdr.Typeflag,
			LinkName:     hdr.Linkname,
			Uid:          hdr.Uid,
			Gid:          hdr.Gid,
			IsSparse:     hdr.Typeflag == TypeGNUSparse || hdr.Typeflag == 'S',
			IsTar:        true,
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