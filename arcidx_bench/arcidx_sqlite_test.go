package arcidx_bench

import (
	"database/sql"
	"fmt"
	"sort"
	"testing"

	_ "modernc.org/sqlite"
)

// MockFlatBufferIndex simulates a zero-copy memory-mapped FlatBuffer structure
// using standard Go slices for benchmarking the performance delta vs SQLite.
type MockFlatBufferIndex struct {
	Paths          []string
	MetadataTuples []MockMetadata
	Files          []MockFileNode
}

type MockMetadata struct {
	Mode  uint32
	Uid   uint32
	Gid   uint32
	Mtime int64
}

type MockFileNode struct {
	PathID       uint32
	Name         string
	OffsetHeader int64
	OffsetData   int64
	Size         int64
	TypeFlag     byte
	MetadataID   uint32
}

func setupSQLiteDB(b testing.TB, numFiles int) *sql.DB {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		b.Fatal(err)
	}

	_, err = db.Exec(`
		CREATE TABLE files (
			path VARCHAR NOT NULL,
			name VARCHAR NOT NULL,
			offsetheader INTEGER,
			offset INTEGER,
			size INTEGER,
			mtime REAL,
			mode INTEGER,
			type INTEGER,
			uid INTEGER,
			gid INTEGER,
			PRIMARY KEY (path, name)
		);
	`)
	if err != nil {
		b.Fatal(err)
	}

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT INTO files (path, name, offsetheader, offset, size, mtime, mode, type, uid, gid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	for i := 0; i < numFiles; i++ {
		path := fmt.Sprintf("/folder_%d", i%100)
		name := fmt.Sprintf("file_%d.txt", i)
		stmt.Exec(path, name, int64(i*512), int64(i*512+512), int64(1024), float64(1600000000), 0644, 0, 1000, 1000)
	}
	tx.Commit()
	return db
}

func setupMockFlatBuffer(b testing.TB, numFiles int) *MockFlatBufferIndex {
	idx := &MockFlatBufferIndex{
		Paths:          make([]string, 100),
		MetadataTuples: []MockMetadata{{Mode: 0644, Uid: 1000, Gid: 1000, Mtime: 1600000000}},
		Files:          make([]MockFileNode, numFiles),
	}

	for i := 0; i < 100; i++ {
		idx.Paths[i] = fmt.Sprintf("/folder_%d", i)
	}

	for i := 0; i < numFiles; i++ {
		idx.Files[i] = MockFileNode{
			PathID:       uint32(i % 100),
			Name:         fmt.Sprintf("file_%d.txt", i),
			OffsetHeader: int64(i * 512),
			OffsetData:   int64(i*512 + 512),
			Size:         1024,
			TypeFlag:     0,
			MetadataID:   0,
		}
	}

	// Sort files for binary search (PathID then Name)
	sort.Slice(idx.Files, func(i, j int) bool {
		if idx.Files[i].PathID == idx.Files[j].PathID {
			return idx.Files[i].Name < idx.Files[j].Name
		}
		return idx.Files[i].PathID < idx.Files[j].PathID
	})

	return idx
}

func TestLookup_Correctness(t *testing.T) {
	numFiles := 100
	db := setupSQLiteDB(t, numFiles)
	defer db.Close()

	fb := setupMockFlatBuffer(t, numFiles)

	for i := 0; i < numFiles; i++ {
		targetName := fmt.Sprintf("file_%d.txt", i)
		targetPathID := uint32(i % 100)
		targetPath := fmt.Sprintf("/folder_%d", targetPathID)

		// Query SQLite
		var sqlSize int64
		err := db.QueryRow(`SELECT size FROM files WHERE path = ? AND name = ?`, targetPath, targetName).Scan(&sqlSize)
		if err != nil {
			t.Fatalf("SQLite lookup failed for %s: %v", targetName, err)
		}

		// Query Mock FB
		idx := sort.Search(len(fb.Files), func(j int) bool {
			if fb.Files[j].PathID == targetPathID {
				return fb.Files[j].Name >= targetName
			}
			return fb.Files[j].PathID >= targetPathID
		})

		if idx >= len(fb.Files) || fb.Files[idx].Name != targetName || fb.Files[idx].PathID != targetPathID {
			t.Fatalf("MockFB lookup failed to find %s", targetName)
		}

		if fb.Files[idx].Size != sqlSize {
			t.Errorf("Size mismatch for %s: SQLite=%d, MockFB=%d", targetName, sqlSize, fb.Files[idx].Size)
		}
	}
}

func BenchmarkSQLite_Lookup(b *testing.B) {
	numFiles := 100000
	db := setupSQLiteDB(b, numFiles)
	defer db.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		targetName := fmt.Sprintf("file_%d.txt", i%numFiles)
		targetPath := fmt.Sprintf("/folder_%d", (i%numFiles)%100)

		var size int64
		err := db.QueryRow(`SELECT size FROM files WHERE path = ? AND name = ?`, targetPath, targetName).Scan(&size)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFlatBuffer_Lookup(b *testing.B) {
	numFiles := 100000
	fb := setupMockFlatBuffer(b, numFiles)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		targetName := fmt.Sprintf("file_%d.txt", i%numFiles)
		targetPathID := uint32((i % numFiles) % 100)

		// Zero-copy binary search
		idx := sort.Search(len(fb.Files), func(j int) bool {
			if fb.Files[j].PathID == targetPathID {
				return fb.Files[j].Name >= targetName
			}
			return fb.Files[j].PathID >= targetPathID
		})

		if idx < len(fb.Files) && fb.Files[idx].Name == targetName && fb.Files[idx].PathID == targetPathID {
			_ = fb.Files[idx].Size
		} else {
			b.Fatal("not found")
		}
	}
}

func BenchmarkSQLite_ReadDir(b *testing.B) {
	numFiles := 100000
	db := setupSQLiteDB(b, numFiles)
	defer db.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		targetPath := fmt.Sprintf("/folder_%d", i%100)
		rows, err := db.Query(`SELECT name, size FROM files WHERE path = ?`, targetPath)
		if err != nil {
			b.Fatal(err)
		}
		
		count := 0
		for rows.Next() {
			var name string
			var size int64
			rows.Scan(&name, &size)
			count++
		}
		rows.Close()
	}
}

func BenchmarkFlatBuffer_ReadDir(b *testing.B) {
	numFiles := 100000
	fb := setupMockFlatBuffer(b, numFiles)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		targetPathID := uint32(i % 100)

		// Find start
		startIdx := sort.Search(len(fb.Files), func(j int) bool {
			return fb.Files[j].PathID >= targetPathID
		})

		// Iterate contiguous slice memory
		count := 0
		for j := startIdx; j < len(fb.Files) && fb.Files[j].PathID == targetPathID; j++ {
			_ = fb.Files[j].Name
			_ = fb.Files[j].Size
			count++
		}
	}
}
