package tar

import (
	"database/sql"
	"path"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

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
}

type Index struct {
	db *sql.DB
}

func normalizePath(p string) (dir, name string) {
	p = "/" + strings.TrimPrefix(p, "/")
	p = path.Clean(p)
	if p == "/" {
		return "/", ""
	}
	dir, name = path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "/"
	}
	return dir, name
}

// OpenIndex opens or creates a ratarmount-compatible SQLite index.
func OpenIndex(dsn string) (*Index, error) {
	db, err := sql.Open("sqlite3", dsn)
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
			linkname, uid, gid, istar, issparse, isgenerated, recursiondepth
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		SELECT offsetheader, offset, size, mtime, mode, type, linkname, uid, gid, istar, issparse, isgenerated, recursiondepth
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
		SELECT name, offsetheader, offset, size, mtime, mode, type, linkname, uid, gid, istar, issparse, isgenerated, recursiondepth
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
		)
		if err != nil {
			return nil, err
		}
		n.ModTime = time.Unix(0, int64(mtime*1e9))
		res = append(res, n)
	}
	return res, nil
}