//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !illumos
package tar

import (
	"database/sql"
	"time"
	"fmt"
	"bytes"

	_ "github.com/ncruces/go-sqlite3/driver"
)

type Index struct {
	db *sql.DB
}

func OpenIndex(dsn string) (*Index, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
	PRAGMA journal_mode = WAL;
	PRAGMA synchronous = NORMAL;
	PRAGMA temp_store = MEMORY;

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
		PRIMARY KEY ("path","name","offsetheader")
	);
	CREATE INDEX IF NOT EXISTS files_offsetheader_index ON files(offsetheader);

	CREATE TABLE IF NOT EXISTS xattrs (
		"offsetheader" INTEGER,
		"key"          TEXT,
		"value"        BLOB
	);
	CREATE INDEX IF NOT EXISTS xattrs_offsetheader_index ON xattrs(offsetheader);

	CREATE TABLE IF NOT EXISTS acls (
		"offsetheader" INTEGER,
		"acl"          BLOB
	);
	CREATE INDEX IF NOT EXISTS acls_offsetheader_index ON acls(offsetheader);

	CREATE TABLE IF NOT EXISTS metadata (
		key VARCHAR(65535) NOT NULL PRIMARY KEY,
		value VARCHAR(65535) NOT NULL
	);
	CREATE TABLE IF NOT EXISTS versions (
		name VARCHAR(65535) NOT NULL PRIMARY KEY,
		version VARCHAR(65535) NOT NULL,
		major INTEGER, minor INTEGER, patch INTEGER
	);

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

func (idx *Index) InitMetadata() error {
	_, err1 := idx.db.Exec(`INSERT OR IGNORE INTO versions (name, version, major, minor, patch) VALUES ('ratarmount', '1.3.0', 1, 3, 0)`)
	_, err2 := idx.db.Exec(`INSERT OR IGNORE INTO versions (name, version, major, minor, patch) VALUES ('index', '0.7.0', 0, 7, 0)`)
	if err1 != nil { return err1 }
	return err2
}

func (idx *Index) Insert(nodes []FileNode) error {
	tx, err := idx.db.Begin()
	if err != nil { return err }

	stmt, err := tx.Prepare(`
		INSERT INTO files (
			path, name, offsetheader, offset, size, mtime, mode, type,
			linkname, uid, gid, istar, issparse, isgenerated, recursiondepth
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`)
	if err != nil { tx.Rollback(); return err }
	defer stmt.Close()

	stmtXattr, err := tx.Prepare(`INSERT INTO xattrs (offsetheader, key, value) VALUES (?, ?, ?)`)
	if err != nil { tx.Rollback(); return err }
	defer stmtXattr.Close()

	stmtAcl, err := tx.Prepare(`INSERT INTO acls (offsetheader, acl) VALUES (?, ?)`)
	if err != nil { tx.Rollback(); return err }
	defer stmtAcl.Close()

	for _, n := range nodes {
		res, err := stmt.Exec(
			n.Path, n.Name, n.OffsetHeader, n.Offset, n.Size, float64(n.ModTime.UnixNano())/1e9,
			n.Mode, n.Type, n.LinkName, n.Uid, n.Gid, n.IsTar, n.IsSparse, n.IsGenerated, n.RecursionDepth,
		)
		if err != nil { tx.Rollback(); return err }
		aff, _ := res.RowsAffected()
		if aff > 0 {
			for k, v := range n.Xattrs {
				_, err = stmtXattr.Exec(n.OffsetHeader, k, v)
				if err != nil { tx.Rollback(); return err }
			}
			if len(n.Acl) > 0 {
				_, err = stmtAcl.Exec(n.OffsetHeader, n.Acl)
				if err != nil { tx.Rollback(); return err }
			}
		}
	}
	return tx.Commit()
}

func (idx *Index) Lookup(p string) (*FileNode, error) {
	dir, name := normalizePath(p)
	if dir == "/" && name == "" {
		return &FileNode{Path: "/", Name: "", OffsetHeader: 0, Offset: 0, Mode: 0755 | 040000, Type: TypeDir, IsGenerated: true}, nil
	}
	row := idx.db.QueryRow(`SELECT offsetheader, offset, size, mtime, mode, type, linkname, uid, gid, istar, issparse, isgenerated, recursiondepth FROM files WHERE path = ? AND name = ? ORDER BY offset DESC LIMIT 1`, dir, name)
	var n FileNode
	var mtime float64
	n.Path, n.Name = dir, name
	err := row.Scan(&n.OffsetHeader, &n.Offset, &n.Size, &mtime, &n.Mode, &n.Type, &n.LinkName, &n.Uid, &n.Gid, &n.IsTar, &n.IsSparse, &n.IsGenerated, &n.RecursionDepth)
	if err != nil { return nil, err }
	n.Xattrs = make(map[string][]byte)
	xRows, err := idx.db.Query(`SELECT key, value FROM xattrs WHERE offsetheader = ?`, n.OffsetHeader)
	if err == nil {
		defer xRows.Close()
		for xRows.Next() {
			var k string; var v []byte
			if xRows.Scan(&k, &v) == nil { n.Xattrs[k] = v }
		}
	}
	aRow := idx.db.QueryRow(`SELECT acl FROM acls WHERE offsetheader = ?`, n.OffsetHeader)
	aRow.Scan(&n.Acl)
	n.ModTime = time.Unix(0, int64(mtime*1e9))
	return &n, nil
}

func (idx *Index) List(p string) ([]FileNode, error) {
	dir, name := normalizePath(p)
	fullPath := dir
	if name != "" { if dir == "/" { fullPath = "/" + name } else { fullPath = dir + "/" + name } }
	rows, err := idx.db.Query(`SELECT name, offsetheader, offset, size, mtime, mode, type, linkname, uid, gid, istar, issparse, isgenerated, recursiondepth FROM files WHERE path = ? GROUP BY name HAVING offset = MAX(offset)`, fullPath)
	if err != nil { return nil, err }
	defer rows.Close()
	var res []FileNode
	for rows.Next() {
		var n FileNode; var mtime float64
		n.Path = fullPath
		err := rows.Scan(&n.Name, &n.OffsetHeader, &n.Offset, &n.Size, &mtime, &n.Mode, &n.Type, &n.LinkName, &n.Uid, &n.Gid, &n.IsTar, &n.IsSparse, &n.IsGenerated, &n.RecursionDepth)
		if err != nil { return nil, err }
		n.ModTime = time.Unix(0, int64(mtime*1e9))
		res = append(res, n)
	}
	for i := range res {
		res[i].Xattrs = make(map[string][]byte)
		xRows, _ := idx.db.Query(`SELECT key, value FROM xattrs WHERE offsetheader = ?`, res[i].OffsetHeader)
		if xRows != nil {
			defer xRows.Close()
			for xRows.Next() {
				var k string; var v []byte
				if xRows.Scan(&k, &v) == nil { res[i].Xattrs[k] = v }
			}
		}
		aRow := idx.db.QueryRow(`SELECT acl FROM acls WHERE offsetheader = ?`, res[i].OffsetHeader)
		aRow.Scan(&res[i].Acl)
	}
	return res, nil
}

func (idx *Index) RecursiveSize(p string) (int64, error) {
	dir, name := normalizePath(p)
	fullPath := dir
	if name != "" { if dir == "/" { fullPath = "/" + name } else { fullPath = dir + "/" + name } }
	query := `WITH latest_files AS (SELECT size FROM files WHERE (path = ? OR path LIKE ?) GROUP BY path, name HAVING offsetheader = MAX(offsetheader)) SELECT COALESCE(SUM(size), 0) FROM latest_files`
	if fullPath == "/" { query = `WITH latest_files AS (SELECT size FROM files GROUP BY path, name HAVING offsetheader = MAX(offsetheader)) SELECT COALESCE(SUM(size), 0) FROM latest_files` }
	var size int64
	var err error
	if fullPath == "/" { err = idx.db.QueryRow(query).Scan(&size) } else { err = idx.db.QueryRow(query, fullPath, fullPath+"/%").Scan(&size) }
	return size, err
}

func (idx *Index) InsertBlockOffsets(table string, offsets []BlockOffset) error {
	tx, err := idx.db.Begin()
	if err != nil { return err }
	stmt, err := tx.Prepare(fmt.Sprintf(`INSERT OR REPLACE INTO "%s" (blockoffset, dataoffset) VALUES (?, ?)`, table))
	if err != nil { tx.Rollback(); return err }
	defer stmt.Close()
	for _, o := range offsets { if _, err = stmt.Exec(o.BlockOffset, o.DataOffset); err != nil { tx.Rollback(); return err } }
	return tx.Commit()
}

func (idx *Index) GetClosestBlockOffset(table string, targetDataOffset int64) (*BlockOffset, error) {
	var bo BlockOffset
	err := idx.db.QueryRow(fmt.Sprintf(`SELECT blockoffset, dataoffset FROM "%s" WHERE dataoffset <= ? ORDER BY dataoffset DESC LIMIT 1`, table), targetDataOffset).Scan(&bo.BlockOffset, &bo.DataOffset)
	if err != nil { return nil, err }
	return &bo, nil
}

func (idx *Index) GetGzipIndex() ([]byte, error) {
	rows, err := idx.db.Query(`SELECT data FROM gzipindexes ORDER BY rowid`)
	if err != nil { return nil, err }
	defer rows.Close()
	var buf bytes.Buffer
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil { return nil, err }
		buf.Write(chunk)
	}
	return buf.Bytes(), nil
}

func (idx *Index) SaveGzipIndex(data []byte) error {
	tx, err := idx.db.Begin()
	if err != nil { return err }
	_, _ = tx.Exec(`DELETE FROM gzipindexes`)
	const maxChunk = 256 * 1024 * 1024
	for i := 0; i < len(data); i += maxChunk {
		end := i + maxChunk
		if end > len(data) { end = len(data) }
		if _, err = tx.Exec(`INSERT INTO gzipindexes (data) VALUES (?)`, data[i:end]); err != nil { tx.Rollback(); return err }
	}
	return tx.Commit()
}