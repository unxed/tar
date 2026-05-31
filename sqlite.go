package tar

import (
	"database/sql"
	"path"
	"time"
	"fmt"
	"bytes"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"
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
	Xattrs         string
	Acl            string
}

type Index struct {
	db *sql.DB
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

// OpenIndex opens or creates a ratarmount-compatible SQLite index.
func OpenIndex(dsn string) (*Index, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Ratarmount compatibility schema
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS "files" (
		"path"           VARCHAR(65535) NOT NULL,
		"name"           VARCHAR(65535) NOT NULL,
		"offsetheader"   INTEGER,
		"offset"         INTEGER,
		"size"           INTEGER,
		"mtime"          REAL,
		"mode"           INTEGER,
		"type"           INTEGER,
		"linkname"       VARCHAR(65535),
		"uid"            INTEGER,
		"gid"            INTEGER,
		"istar"          BOOL,
		"issparse"       BOOL,
		"isgenerated"    BOOL,
		"recursiondepth" INTEGER,
		"xattrs"         TEXT,
		"acl"            TEXT,
		PRIMARY KEY ("path","name","offsetheader")
	);
	CREATE INDEX IF NOT EXISTS files_offsetheader_index ON files(offsetheader);

	CREATE TABLE IF NOT EXISTS metadata (
		key VARCHAR(65535) NOT NULL PRIMARY KEY,
		value VARCHAR(65535) NOT NULL
	);
	CREATE TABLE IF NOT EXISTS versions (
		name VARCHAR(65535) NOT NULL PRIMARY KEY,
		version VARCHAR(65535) NOT NULL,
		major INTEGER, minor INTEGER, patch INTEGER
	);

	-- Ratarmount-compatible compression index schemas
	CREATE TABLE IF NOT EXISTS "zstdblocks" (
		"blockoffset" INTEGER PRIMARY KEY,
		"dataoffset" INTEGER
	);
	CREATE TABLE IF NOT EXISTS "bzip2blocks" (
		"blockoffset" INTEGER PRIMARY KEY,
		"dataoffset" INTEGER
	);
	CREATE TABLE IF NOT EXISTS "gzipindexes" (
		"data" BLOB
	);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &Index{db: db}, nil
}

func (idx *Index) Close() error {
	return idx.db.Close()
}

func (idx *Index) Insert(nodes []FileNode) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`
		INSERT INTO files (
			path, name, offsetheader, offset, size, mtime, mode, type,
			linkname, uid, gid, istar, issparse, isgenerated, recursiondepth, xattrs, acl
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, n := range nodes {
		_, err = stmt.Exec(
			n.Path, n.Name, n.OffsetHeader, n.Offset, n.Size, float64(n.ModTime.UnixNano())/1e9,
			n.Mode, n.Type, n.LinkName, n.Uid, n.Gid, n.IsTar, n.IsSparse, n.IsGenerated, n.RecursionDepth,
			n.Xattrs, n.Acl,
		)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func (idx *Index) Lookup(p string) (*FileNode, error) {
	dir, name := normalizePath(p)

	if dir == "/" && name == "" {
		return &FileNode{
			Path: "/", Name: "", OffsetHeader: 0, Offset: 0,
			Mode: 0755 | 040000, Type: TypeDir, IsGenerated: true,
		}, nil
	}

	row := idx.db.QueryRow(`
		SELECT offsetheader, offset, size, mtime, mode, type, linkname, uid, gid, istar, issparse, isgenerated, recursiondepth, xattrs, acl
		FROM files
		WHERE path = ? AND name = ?
		ORDER BY offset DESC LIMIT 1
	`, dir, name)

	var n FileNode
	var mtime float64
	n.Path = dir
	n.Name = name

	err := row.Scan(
		&n.OffsetHeader, &n.Offset, &n.Size, &mtime, &n.Mode, &n.Type,
		&n.LinkName, &n.Uid, &n.Gid, &n.IsTar, &n.IsSparse, &n.IsGenerated, &n.RecursionDepth,
		&n.Xattrs, &n.Acl,
	)
	if err != nil {
		return nil, err
	}
	n.ModTime = time.Unix(0, int64(mtime*1e9))
	return &n, nil
}

func (idx *Index) List(p string) ([]FileNode, error) {
	dir, name := normalizePath(p)
	fullPath := dir
	if name != "" {
		if dir == "/" {
			fullPath = "/" + name
		} else {
			fullPath = dir + "/" + name
		}
	}

	// We group by name to avoid returning older versions of appended files
	rows, err := idx.db.Query(`
		SELECT name, offsetheader, offset, size, mtime, mode, type, linkname, uid, gid, istar, issparse, isgenerated, recursiondepth, xattrs, acl
		FROM files
		WHERE path = ?
		GROUP BY name
		HAVING offset = MAX(offset)
	`, fullPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []FileNode
	for rows.Next() {
		var n FileNode
		var mtime float64
		n.Path = fullPath
		err := rows.Scan(
			&n.Name, &n.OffsetHeader, &n.Offset, &n.Size, &mtime, &n.Mode, &n.Type,
			&n.LinkName, &n.Uid, &n.Gid, &n.IsTar, &n.IsSparse, &n.IsGenerated, &n.RecursionDepth,
			&n.Xattrs, &n.Acl,
		)
		if err != nil {
			return nil, err
		}
		n.ModTime = time.Unix(0, int64(mtime*1e9))
		res = append(res, n)
	}
	return res, nil
}

// RecursiveSize calculates the cumulative size of all files inside the specified
// directory path recursively, without reading the archive. It respects appended duplicates.
func (idx *Index) RecursiveSize(p string) (int64, error) {
	dir, name := normalizePath(p)
	fullPath := dir
	if name != "" {
		if dir == "/" {
			fullPath = "/" + name
		} else {
			fullPath = dir + "/" + name
		}
	}

	if fullPath == "/" {
		var size int64
		err := idx.db.QueryRow(`
			WITH latest_files AS (
				SELECT size
				FROM files
				GROUP BY path, name
				HAVING offsetheader = MAX(offsetheader)
			)
			SELECT COALESCE(SUM(size), 0) FROM latest_files
		`).Scan(&size)
		return size, err
	}

	var size int64
	err := idx.db.QueryRow(`
		WITH latest_files AS (
			SELECT size
			FROM files
			WHERE path = ? OR path LIKE ?
			GROUP BY path, name
			HAVING offsetheader = MAX(offsetheader)
		)
		SELECT COALESCE(SUM(size), 0) FROM latest_files
	`, fullPath, fullPath+"/%").Scan(&size)
	return size, err
}

type BlockOffset struct {
	BlockOffset int64 // Compressed block offset (bit or byte depending on format)
	DataOffset  int64 // Uncompressed data offset (byte)
}

// InsertBlockOffsets bulk-inserts block boundaries for formats like ZSTD and BZIP2.
func (idx *Index) InsertBlockOffsets(table string, offsets []BlockOffset) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(fmt.Sprintf(`INSERT OR REPLACE INTO "%s" (blockoffset, dataoffset) VALUES (?, ?)`, table))
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, o := range offsets {
		_, err = stmt.Exec(o.BlockOffset, o.DataOffset)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// GetClosestBlockOffset finds the nearest block start position for O(1) seeking.
func (idx *Index) GetClosestBlockOffset(table string, targetDataOffset int64) (*BlockOffset, error) {
	row := idx.db.QueryRow(fmt.Sprintf(`
		SELECT blockoffset, dataoffset
		FROM "%s"
		WHERE dataoffset <= ?
		ORDER BY dataoffset DESC LIMIT 1
	`, table), targetDataOffset)
	var bo BlockOffset
	err := row.Scan(&bo.BlockOffset, &bo.DataOffset)
	if err != nil {
		return nil, err
	}
	return &bo, nil
}

// GetGzipIndex loads and concatenates chunked GZIP indexes (compatible with ratarmount's chunking).
func (idx *Index) GetGzipIndex() ([]byte, error) {
	rows, err := idx.db.Query(`SELECT data FROM gzipindexes ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buf bytes.Buffer
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, err
		}
		buf.Write(chunk)
	}
	return buf.Bytes(), nil
}

// SaveGzipIndex clears and saves a new GZIP index in chunked format (max 256MB per row).
func (idx *Index) SaveGzipIndex(data []byte) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	_, _ = tx.Exec(`DELETE FROM gzipindexes`)

	const maxChunk = 256 * 1024 * 1024 // 256MB limit per SQLite BLOB
	for i := 0; i < len(data); i += maxChunk {
		end := i + maxChunk
		if end > len(data) {
			end = len(data)
		}
		_, err = tx.Exec(`INSERT INTO gzipindexes (data) VALUES (?)`, data[i:end])
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
