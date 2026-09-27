package tar

// ArcidxIndex is a sqlite-free, mmap-friendly alternative implementation of
// the file index used by IndexArchive/OpenIndex (see sqlite_enabled.go and
// sqlite_disabled.go). It stores the same logical information (a
// ratarmount-compatible mapping from archive paths to header/data offsets,
// metadata and xattrs) but serializes it as a single FlatBuffers message
// instead of a SQLite database, so a read-only open can be a plain mmap/
// ReadFile plus zero-copy accessor calls instead of a database connection.
//
// Motivation and background: f4#1178 (see
// https://github.com/unxed/f4/issues/1178#issuecomment-5851433383) asked for
// an sqlite-free "lite" build of f4, and index.sqlite (created by
// sqlite_enabled.go) is the last remaining sqlite dependency in that path.
// This file implements the first step ("1 of 11") of the plan referenced in
// that comment: a new, additional index backend in unxed/tar, not wired into
// zipper/f4 yet, and not a replacement for sqlite_enabled.go (the owner has
// stated that SQLite stays the default backend at least until tar/zipper/f4
// 2.0 - see the same f4#1178 thread).
//
// The on-disk format is NOT invented here. It is the FlatBuffers schema
// that was standardized upstream, together with ratarmount's maintainer, in
// https://github.com/mxmlnkn/ratarmount/issues/192 (see the "Here is the
// revised schema incorporating all of your feedback" comment for the final,
// agreed-upon version). The schema is reproduced verbatim in
// arcidx/schema.fbs, and documented as the `.tarext/ratarmount/index.arcidx`
// sidecar format in f4tar.md. Keeping this schema byte-for-byte identical to
// the upstream one (rather than a look-alike of our own) is the whole point:
// it lets a future Python/ratarmount reader, or any other language binding,
// consume files written by this Go implementation without a shim.
//
// Zero-copy design: `files` in the FlatBuffers table is kept sorted by
// (path_id, name, offset_header) ascending, exactly like the mock benchmark
// in arcidx_bench/arcidx_sqlite_test.go (MockFlatBufferIndex) simulates.
// OpenArcidxIndex on an *existing* file therefore does not decode anything
// up front: it just validates the buffer once, and Lookup/List/RecursiveSize
// binary-search directly over the generated flatbuffers accessors. Writing
// (Insert/InsertBlockOffsets/SaveGzipIndex/InitMetadata) needs a mutable
// representation, so the first write call "materializes" the loaded buffer
// into plain Go slices/maps; Close() then serializes the whole index back
// into one FlatBuffers message and atomically replaces the file. This
// mirrors how IndexArchive actually drives an Index today: it always builds
// the file from scratch (indexer_enabled.go removes any existing index
// before calling OpenIndex), so the "always rebuild on Close" write model
// costs nothing in the one caller that exists so far.
//
// Known gaps versus sqlite_enabled.go (documented rather than silently
// dropped, since this is only step 1 of the 11-step plan):
//   - sqlite's PRIMARY KEY is (path, name, offsetheader), so every historical
//     occurrence of a path (e.g. a file appended twice to the same tar) is
//     kept forever; Lookup/List/RecursiveSize then pick the row with the
//     largest offset. ArcidxIndex reproduces the same "latest offset wins"
//     query semantics, and also keeps every occurrence on disk (sorted by
//     offset within a (path_id, name) run), so history is not lost either.
//   - sqlite's InsertBlockOffsets keys rows by blockoffset and supports
//     incremental INSERT OR REPLACE across many calls; the schema here only
//     has one (compression_format, compression_blob) pair for the whole
//     index. IndexArchive only ever calls InsertBlockOffsets once per run,
//     so this backend implements it as "replace the whole table", not a
//     per-key merge. If a future caller needs incremental block-offset
//     updates across multiple calls, that needs another step of this plan.
//   - InitMetadata does not carry sqlite's free-form (name, version) rows;
//     it only fills ArchiveIndex.backend_name/version, which is what the
//     schema actually has room for.
//   - GetClosestBlockOffset/GetGzipIndex/InsertBlockOffsets/SaveGzipIndex all
//     share the single compression_format/compression_blob pair, matching
//     the schema; a given archive is only ever compressed with one method at
//     a time in the current callers, so this is not a practical limitation
//     today, but it is a real difference from sqlite's separate tables.
//
// This backend is NOT registered anywhere else in the codebase: callers that
// want it must call NewArcidxIndex/OpenArcidxIndex explicitly. Wiring it
// into zipper/f4 is left to later steps of the plan.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	arcidxfb "github.com/unxed/tar/arcidx"
)

// ErrArcidxNotFound is returned by ArcidxIndex.Lookup and
// ArcidxIndex.GetClosestBlockOffset when there is no matching entry.
var ErrArcidxNotFound = errors.New("arcidx: not found")

var errArcidxClosed = errors.New("arcidx: index is closed")

const (
	arcidxSchemaVersion uint8  = 1
	arcidxBackendName   string = "unxed/tar-arcidx"
	arcidxGzipFormat    string = "gzidx"
)

// arcidxMetaKey is the deduplication key for MetadataTuple entries: any two
// FileNode records with identical mode/uid/gid/recursion depth/type/flags
// share a single MetadataTuple.
type arcidxMetaKey struct {
	Mode           uint32
	Uid            uint32
	Gid            uint32
	RecursionDepth uint32
	TypeFlag       byte
	IsTar          bool
	IsSparse       bool
	IsGenerated    bool
}

type arcidxXattrRec struct {
	KeyID uint32
	Value []byte
}

type arcidxFileRec struct {
	PathID       uint32
	Name         string
	OffsetHeader uint64
	OffsetData   uint64
	Size         uint64
	Mtime        int64 // nanoseconds since Unix epoch
	MetadataID   uint32
	Xattrs       []arcidxXattrRec
	Acl          []byte
	LinkName     string
}

// arcidxFileKey mirrors sqlite's (path, name, offsetheader) primary key, used
// to give Insert the same "ON CONFLICT DO NOTHING" idempotency.
type arcidxFileKey struct {
	Path         string
	Name         string
	OffsetHeader int64
}

// ArcidxIndex is the FlatBuffers-backed counterpart of the sqlite Index type.
// It implements the same set of methods (Close, InitMetadata, Insert,
// Lookup, List, RecursiveSize, InsertBlockOffsets, GetClosestBlockOffset,
// GetGzipIndex, SaveGzipIndex) with the same signatures, using the shared
// FileNode/BlockOffset domain types from sqlite.go, so calling code can
// choose either backend without depending on sqlite-specific types.
type ArcidxIndex struct {
	path string

	mu     sync.Mutex
	closed bool

	// Zero-copy read state: valid whenever materialized is false. root
	// points directly into raw; every Lookup/List/RecursiveSize/
	// GetGzipIndex/GetClosestBlockOffset call reads through it without any
	// upfront decoding.
	raw  []byte
	root *arcidxfb.ArchiveIndex

	// Write-buffered state: valid whenever materialized is true. Populated
	// either directly (NewArcidxIndex) or lazily out of raw/root on the
	// first write call (see materialize).
	materialized bool

	backendName string

	pathIDs map[string]uint32
	paths   []string

	xattrKeyIDs map[string]uint32
	xattrKeys   []string

	metaIDs map[arcidxMetaKey]uint32
	metas   []arcidxMetaKey

	files     []arcidxFileRec
	seenFiles map[arcidxFileKey]struct{}

	compressionFormat string
	compressionBlob   []byte
}

// NewArcidxIndex creates a brand-new, empty, in-memory index that will be
// serialized to path on Close. Any existing file at path is left untouched
// until Close overwrites it; callers that want a guaranteed-fresh file (like
// indexer_enabled.go does for sqlite) should remove the old file themselves
// first.
func NewArcidxIndex(path string) (*ArcidxIndex, error) {
	return newEmptyArcidxIndex(path), nil
}

// OpenArcidxIndex opens path for reading (and, once a write method is
// called, for read-modify-write access). If path does not exist or is
// empty, it behaves like NewArcidxIndex: this mirrors OpenIndex's
// "CREATE TABLE IF NOT EXISTS" semantics for a brand-new sqlite file. If
// path exists but its contents cannot be parsed as a valid ArchiveIndex
// FlatBuffers message, an error is returned instead of silently starting
// from empty, so callers can tell "no index yet" apart from "corrupted
// index".
func OpenArcidxIndex(path string) (*ArcidxIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newEmptyArcidxIndex(path), nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return newEmptyArcidxIndex(path), nil
	}

	root, err := safeGetRootAsArchiveIndex(data)
	if err != nil {
		return nil, fmt.Errorf("arcidx: corrupted index %q: %w", path, err)
	}
	if err := validateArchiveIndex(root); err != nil {
		return nil, fmt.Errorf("arcidx: corrupted index %q: %w", path, err)
	}

	return &ArcidxIndex{path: path, raw: data, root: root}, nil
}

func newEmptyArcidxIndex(path string) *ArcidxIndex {
	return &ArcidxIndex{
		path:         path,
		materialized: true,
		backendName:  arcidxBackendName,
		pathIDs:      make(map[string]uint32),
		xattrKeyIDs:  make(map[string]uint32),
		metaIDs:      make(map[arcidxMetaKey]uint32),
		seenFiles:    make(map[arcidxFileKey]struct{}),
	}
}

// safeGetRootAsArchiveIndex wraps arcidxfb.GetRootAsArchiveIndex with a
// panic guard: the generated flatbuffers accessors trust the buffer layout
// and can panic (slice out of range) on arbitrary/garbage bytes instead of
// returning an error.
func safeGetRootAsArchiveIndex(data []byte) (root *arcidxfb.ArchiveIndex, err error) {
	defer func() {
		if r := recover(); r != nil {
			root, err = nil, fmt.Errorf("invalid flatbuffers buffer: %v", r)
		}
	}()
	if len(data) < 8 {
		return nil, errors.New("buffer too short to be a flatbuffers message")
	}
	return arcidxfb.GetRootAsArchiveIndex(data, 0), nil
}

// validateArchiveIndex walks every FileNode once and checks that every
// path_id/metadata_id/xattr key_id it references is actually in range. This
// both catches truncated/bit-flipped files and gives OpenArcidxIndex a
// single place to guard against accessor panics on malformed input.
func validateArchiveIndex(root *arcidxfb.ArchiveIndex) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("invalid flatbuffers structure: %v", r)
		}
	}()

	numPaths := root.PathsLength()
	numKeys := root.XattrKeysLength()
	numMetas := root.MetadataTuplesLength()
	numFiles := root.FilesLength()

	var fn arcidxfb.FileNode
	var xa arcidxfb.Xattr
	for i := 0; i < numFiles; i++ {
		if !root.Files(&fn, i) {
			return fmt.Errorf("file entry %d is missing", i)
		}
		if int(fn.PathId()) >= numPaths {
			return fmt.Errorf("file %d: path_id %d out of range (have %d paths)", i, fn.PathId(), numPaths)
		}
		if int(fn.MetadataId()) >= numMetas {
			return fmt.Errorf("file %d: metadata_id %d out of range (have %d metadata tuples)", i, fn.MetadataId(), numMetas)
		}
		for j := 0; j < fn.XattrsLength(); j++ {
			if !fn.Xattrs(&xa, j) {
				return fmt.Errorf("file %d: xattr %d is missing", i, j)
			}
			if int(xa.KeyId()) >= numKeys {
				return fmt.Errorf("file %d: xattr %d: key_id %d out of range (have %d xattr keys)", i, j, xa.KeyId(), numKeys)
			}
		}
	}
	return nil
}

// materialize lazily decodes the zero-copy buffer into mutable Go
// slices/maps so that Insert/InitMetadata/InsertBlockOffsets/SaveGzipIndex
// can mutate it. Called with idx.mu held.
func (idx *ArcidxIndex) materialize() {
	if idx.materialized {
		return
	}

	idx.pathIDs = make(map[string]uint32)
	idx.xattrKeyIDs = make(map[string]uint32)
	idx.metaIDs = make(map[arcidxMetaKey]uint32)
	idx.seenFiles = make(map[arcidxFileKey]struct{})

	if idx.root != nil {
		root := idx.root

		n := root.PathsLength()
		idx.paths = make([]string, n)
		for i := 0; i < n; i++ {
			s := string(root.Paths(i))
			idx.paths[i] = s
			idx.pathIDs[s] = uint32(i)
		}

		n = root.XattrKeysLength()
		idx.xattrKeys = make([]string, n)
		for i := 0; i < n; i++ {
			s := string(root.XattrKeys(i))
			idx.xattrKeys[i] = s
			idx.xattrKeyIDs[s] = uint32(i)
		}

		n = root.MetadataTuplesLength()
		idx.metas = make([]arcidxMetaKey, n)
		var mt arcidxfb.MetadataTuple
		for i := 0; i < n; i++ {
			root.MetadataTuples(&mt, i)
			k := arcidxMetaKey{
				Mode: mt.Mode(), Uid: mt.Uid(), Gid: mt.Gid(),
				RecursionDepth: mt.RecursionDepth(), TypeFlag: mt.TypeFlag(),
				IsTar: mt.IsTar(), IsSparse: mt.IsSparse(), IsGenerated: mt.IsGenerated(),
			}
			idx.metas[i] = k
			idx.metaIDs[k] = uint32(i)
		}

		n = root.FilesLength()
		idx.files = make([]arcidxFileRec, n)
		var fn arcidxfb.FileNode
		var xa arcidxfb.Xattr
		for i := 0; i < n; i++ {
			root.Files(&fn, i)
			rec := arcidxFileRec{
				PathID:       fn.PathId(),
				Name:         string(fn.Name()),
				OffsetHeader: fn.OffsetHeader(),
				OffsetData:   fn.OffsetData(),
				Size:         fn.Size(),
				Mtime:        fn.Mtime(),
				MetadataID:   fn.MetadataId(),
				LinkName:     string(fn.LinkName()),
			}
			if al := fn.AclLength(); al > 0 {
				rec.Acl = append([]byte(nil), fn.AclBytes()...)
			}
			if xl := fn.XattrsLength(); xl > 0 {
				rec.Xattrs = make([]arcidxXattrRec, xl)
				for j := 0; j < xl; j++ {
					fn.Xattrs(&xa, j)
					rec.Xattrs[j] = arcidxXattrRec{KeyID: xa.KeyId(), Value: append([]byte(nil), xa.ValueBytes()...)}
				}
			}
			idx.files[i] = rec
			idx.seenFiles[arcidxFileKey{idx.paths[rec.PathID], rec.Name, int64(rec.OffsetHeader)}] = struct{}{}
		}

		idx.backendName = string(root.BackendName())
		idx.compressionFormat = string(root.CompressionFormat())
		if cl := root.CompressionBlobLength(); cl > 0 {
			idx.compressionBlob = append([]byte(nil), root.CompressionBlobBytes()...)
		}
	}

	if idx.backendName == "" {
		idx.backendName = arcidxBackendName
	}

	idx.root = nil
	idx.raw = nil
	idx.materialized = true
}

func (idx *ArcidxIndex) internPath(p string) uint32 {
	if id, ok := idx.pathIDs[p]; ok {
		return id
	}
	id := uint32(len(idx.paths))
	idx.paths = append(idx.paths, p)
	idx.pathIDs[p] = id
	return id
}

func (idx *ArcidxIndex) internXattrKey(k string) uint32 {
	if id, ok := idx.xattrKeyIDs[k]; ok {
		return id
	}
	id := uint32(len(idx.xattrKeys))
	idx.xattrKeys = append(idx.xattrKeys, k)
	idx.xattrKeyIDs[k] = id
	return id
}

func (idx *ArcidxIndex) internMeta(k arcidxMetaKey) uint32 {
	if id, ok := idx.metaIDs[k]; ok {
		return id
	}
	id := uint32(len(idx.metas))
	idx.metas = append(idx.metas, k)
	idx.metaIDs[k] = id
	return id
}

// Close flushes any buffered writes to disk (as a single FlatBuffers
// ArchiveIndex message, written atomically) and marks the index unusable.
// Calling Close on an index that was only ever read from (never
// materialized) is a cheap no-op, matching sql.DB.Close's idempotency.
func (idx *ArcidxIndex) Close() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return nil
	}
	idx.closed = true

	if !idx.materialized {
		idx.root = nil
		idx.raw = nil
		return nil
	}
	return idx.flush()
}

func (idx *ArcidxIndex) flush() error {
	buf, err := idx.build()
	if err != nil {
		return err
	}
	return writeFileAtomic(idx.path, buf)
}

// build serializes the current in-memory (materialized) state into a single
// FlatBuffers ArchiveIndex message, matching arcidx/schema.fbs.
func (idx *ArcidxIndex) build() ([]byte, error) {
	files := make([]arcidxFileRec, len(idx.files))
	copy(files, idx.files)
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if a.PathID != b.PathID {
			return a.PathID < b.PathID
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.OffsetHeader < b.OffsetHeader
	})

	b := flatbuffers.NewBuilder(1024 + len(files)*64)

	var backendNameOff flatbuffers.UOffsetT
	if idx.backendName != "" {
		backendNameOff = b.CreateString(idx.backendName)
	}

	pathOffs := make([]flatbuffers.UOffsetT, len(idx.paths))
	for i, p := range idx.paths {
		pathOffs[i] = b.CreateString(p)
	}
	pathsVec := buildOffsetVector(b, arcidxfb.ArchiveIndexStartPathsVector, pathOffs)

	keyOffs := make([]flatbuffers.UOffsetT, len(idx.xattrKeys))
	for i, k := range idx.xattrKeys {
		keyOffs[i] = b.CreateString(k)
	}
	xattrKeysVec := buildOffsetVector(b, arcidxfb.ArchiveIndexStartXattrKeysVector, keyOffs)

	arcidxfb.ArchiveIndexStartMetadataTuplesVector(b, len(idx.metas))
	for i := len(idx.metas) - 1; i >= 0; i-- {
		m := idx.metas[i]
		arcidxfb.CreateMetadataTuple(b, m.Mode, m.Uid, m.Gid, m.RecursionDepth, m.TypeFlag, m.IsTar, m.IsSparse, m.IsGenerated)
	}
	metasVec := b.EndVector(len(idx.metas))

	fileOffs := make([]flatbuffers.UOffsetT, len(files))
	for i, f := range files {
		var xattrsVec flatbuffers.UOffsetT
		if len(f.Xattrs) > 0 {
			xoffs := make([]flatbuffers.UOffsetT, len(f.Xattrs))
			for j, x := range f.Xattrs {
				var valOff flatbuffers.UOffsetT
				if len(x.Value) > 0 {
					valOff = b.CreateByteVector(x.Value)
				}
				arcidxfb.XattrStart(b)
				arcidxfb.XattrAddKeyId(b, x.KeyID)
				if valOff != 0 {
					arcidxfb.XattrAddValue(b, valOff)
				}
				xoffs[j] = arcidxfb.XattrEnd(b)
			}
			xattrsVec = buildOffsetVector(b, arcidxfb.FileNodeStartXattrsVector, xoffs)
		}

		var aclVec flatbuffers.UOffsetT
		if len(f.Acl) > 0 {
			aclVec = b.CreateByteVector(f.Acl)
		}
		var linkOff flatbuffers.UOffsetT
		if f.LinkName != "" {
			linkOff = b.CreateString(f.LinkName)
		}
		nameOff := b.CreateString(f.Name)

		arcidxfb.FileNodeStart(b)
		arcidxfb.FileNodeAddPathId(b, f.PathID)
		arcidxfb.FileNodeAddName(b, nameOff)
		arcidxfb.FileNodeAddOffsetHeader(b, f.OffsetHeader)
		arcidxfb.FileNodeAddOffsetData(b, f.OffsetData)
		arcidxfb.FileNodeAddSize(b, f.Size)
		arcidxfb.FileNodeAddMtime(b, f.Mtime)
		arcidxfb.FileNodeAddMetadataId(b, f.MetadataID)
		if xattrsVec != 0 {
			arcidxfb.FileNodeAddXattrs(b, xattrsVec)
		}
		if aclVec != 0 {
			arcidxfb.FileNodeAddAcl(b, aclVec)
		}
		if linkOff != 0 {
			arcidxfb.FileNodeAddLinkName(b, linkOff)
		}
		fileOffs[i] = arcidxfb.FileNodeEnd(b)
	}
	filesVec := buildOffsetVector(b, arcidxfb.ArchiveIndexStartFilesVector, fileOffs)

	var compFormatOff flatbuffers.UOffsetT
	if idx.compressionFormat != "" {
		compFormatOff = b.CreateString(idx.compressionFormat)
	}
	var compBlobVec flatbuffers.UOffsetT
	if len(idx.compressionBlob) > 0 {
		compBlobVec = b.CreateByteVector(idx.compressionBlob)
	}

	arcidxfb.ArchiveIndexStart(b)
	arcidxfb.ArchiveIndexAddVersion(b, arcidxSchemaVersion)
	if backendNameOff != 0 {
		arcidxfb.ArchiveIndexAddBackendName(b, backendNameOff)
	}
	arcidxfb.ArchiveIndexAddPaths(b, pathsVec)
	arcidxfb.ArchiveIndexAddXattrKeys(b, xattrKeysVec)
	arcidxfb.ArchiveIndexAddMetadataTuples(b, metasVec)
	arcidxfb.ArchiveIndexAddFiles(b, filesVec)
	if compFormatOff != 0 {
		arcidxfb.ArchiveIndexAddCompressionFormat(b, compFormatOff)
	}
	if compBlobVec != 0 {
		arcidxfb.ArchiveIndexAddCompressionBlob(b, compBlobVec)
	}
	root := arcidxfb.ArchiveIndexEnd(b)
	b.Finish(root)

	return b.FinishedBytes(), nil
}

// buildOffsetVector prepends off in reverse order between a generated
// StartXxxVector call and EndVector, which is the standard flatbuffers Go
// pattern for building a vector of offsets (strings or tables).
func buildOffsetVector(b *flatbuffers.Builder, start func(*flatbuffers.Builder, int) flatbuffers.UOffsetT, offs []flatbuffers.UOffsetT) flatbuffers.UOffsetT {
	start(b, len(offs))
	for i := len(offs) - 1; i >= 0; i-- {
		b.PrependUOffsetT(offs[i])
	}
	return b.EndVector(len(offs))
}

// writeFileAtomic writes data to a temp file in the same directory as path
// and renames it into place, so a crash or a concurrent reader never sees a
// partially-written index.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	done = true
	return nil
}

// InitMetadata records the backend identity in the index. Unlike sqlite's
// InitMetadata, which inserts free-form (name, version) rows into a
// "versions" table, this only has room for ArchiveIndex.backend_name /
// .version, which is what it sets.
func (idx *ArcidxIndex) InitMetadata() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return errArcidxClosed
	}
	idx.materialize()
	if idx.backendName == "" {
		idx.backendName = arcidxBackendName
	}
	return nil
}

// Insert adds nodes to the index, deduplicating paths, xattr keys and
// metadata tuples, and skipping any (path, name, offsetheader) triple
// already present (mirroring sqlite_enabled.go's `ON CONFLICT DO NOTHING`).
func (idx *ArcidxIndex) Insert(nodes []FileNode) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return errArcidxClosed
	}
	idx.materialize()

	for _, n := range nodes {
		key := arcidxFileKey{Path: n.Path, Name: n.Name, OffsetHeader: n.OffsetHeader}
		if _, dup := idx.seenFiles[key]; dup {
			continue
		}
		idx.seenFiles[key] = struct{}{}

		pathID := idx.internPath(n.Path)
		metaID := idx.internMeta(arcidxMetaKey{
			Mode:           uint32(n.Mode),
			Uid:            uint32(n.Uid),
			Gid:            uint32(n.Gid),
			RecursionDepth: uint32(n.RecursionDepth),
			TypeFlag:       n.Type,
			IsTar:          n.IsTar,
			IsSparse:       n.IsSparse,
			IsGenerated:    n.IsGenerated,
		})

		var xattrs []arcidxXattrRec
		if len(n.Xattrs) > 0 {
			xattrs = make([]arcidxXattrRec, 0, len(n.Xattrs))
			for k, v := range n.Xattrs {
				xattrs = append(xattrs, arcidxXattrRec{KeyID: idx.internXattrKey(k), Value: v})
			}
			sort.Slice(xattrs, func(i, j int) bool { return xattrs[i].KeyID < xattrs[j].KeyID })
		}

		idx.files = append(idx.files, arcidxFileRec{
			PathID:       pathID,
			Name:         n.Name,
			OffsetHeader: uint64(n.OffsetHeader),
			OffsetData:   uint64(n.Offset),
			Size:         uint64(n.Size),
			Mtime:        n.ModTime.UnixNano(),
			MetadataID:   metaID,
			Xattrs:       xattrs,
			Acl:          n.Acl,
			LinkName:     n.LinkName,
		})
	}
	return nil
}

func joinDirName(dir, name string) string {
	if name == "" {
		return dir
	}
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

func arcidxSyntheticRoot() *FileNode {
	return &FileNode{Path: "/", Name: "", OffsetHeader: 0, Offset: 0, Mode: 0755 | 040000, Type: TypeDir, IsGenerated: true}
}

// Lookup returns the latest (highest offset_header) entry for path p.
func (idx *ArcidxIndex) Lookup(p string) (*FileNode, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return nil, errArcidxClosed
	}
	dir, name := normalizePath(p)
	if dir == "/" && name == "" {
		return arcidxSyntheticRoot(), nil
	}
	if idx.materialized {
		return idx.lookupMaterialized(dir, name)
	}
	return idx.lookupZeroCopy(dir, name)
}

func (idx *ArcidxIndex) lookupMaterialized(dir, name string) (*FileNode, error) {
	pathID, ok := idx.pathIDs[dir]
	if !ok {
		return nil, ErrArcidxNotFound
	}
	var best *arcidxFileRec
	for i := range idx.files {
		f := &idx.files[i]
		if f.PathID == pathID && f.Name == name {
			if best == nil || f.OffsetHeader > best.OffsetHeader {
				best = f
			}
		}
	}
	if best == nil {
		return nil, ErrArcidxNotFound
	}
	return idx.toFileNode(dir, name, best), nil
}

func (idx *ArcidxIndex) lookupZeroCopy(dir, name string) (*FileNode, error) {
	root := idx.root
	pathID, ok := idx.findPathID(dir)
	if !ok {
		return nil, ErrArcidxNotFound
	}

	n := root.FilesLength()
	needle := []byte(name)
	var fn arcidxfb.FileNode
	lo := sort.Search(n, func(i int) bool {
		root.Files(&fn, i)
		if fn.PathId() != pathID {
			return fn.PathId() >= pathID
		}
		return bytes.Compare(fn.Name(), needle) >= 0
	})

	var best arcidxfb.FileNode
	found := false
	var bestOffset uint64
	for i := lo; i < n; i++ {
		root.Files(&fn, i)
		if fn.PathId() != pathID || !bytes.Equal(fn.Name(), needle) {
			break
		}
		if !found || fn.OffsetHeader() > bestOffset {
			best = fn
			bestOffset = fn.OffsetHeader()
			found = true
		}
	}
	if !found {
		return nil, ErrArcidxNotFound
	}
	return idx.fbToFileNode(dir, name, &best), nil
}

// findPathID does a linear scan of the (deduplicated, so typically much
// smaller than the file count) paths vector. It compares raw bytes to avoid
// allocating a Go string per candidate.
func (idx *ArcidxIndex) findPathID(dir string) (uint32, bool) {
	needle := []byte(dir)
	n := idx.root.PathsLength()
	for i := 0; i < n; i++ {
		if bytes.Equal(idx.root.Paths(i), needle) {
			return uint32(i), true
		}
	}
	return 0, false
}

// List returns the latest entry for every distinct name directly under p
// (a ReadDir-style listing).
func (idx *ArcidxIndex) List(p string) ([]FileNode, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return nil, errArcidxClosed
	}
	dir, name := normalizePath(p)
	fullPath := joinDirName(dir, name)
	if idx.materialized {
		return idx.listMaterialized(fullPath)
	}
	return idx.listZeroCopy(fullPath)
}

func (idx *ArcidxIndex) listMaterialized(fullPath string) ([]FileNode, error) {
	pathID, ok := idx.pathIDs[fullPath]
	if !ok {
		return nil, nil
	}
	type latestEntry struct {
		rec *arcidxFileRec
	}
	latest := make(map[string]latestEntry)
	for i := range idx.files {
		f := &idx.files[i]
		if f.PathID != pathID {
			continue
		}
		cur, exists := latest[f.Name]
		if !exists || f.OffsetHeader > cur.rec.OffsetHeader {
			latest[f.Name] = latestEntry{rec: f}
		}
	}
	res := make([]FileNode, 0, len(latest))
	for name, e := range latest {
		res = append(res, *idx.toFileNode(fullPath, name, e.rec))
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}

func (idx *ArcidxIndex) listZeroCopy(fullPath string) ([]FileNode, error) {
	root := idx.root
	pathID, ok := idx.findPathID(fullPath)
	if !ok {
		return nil, nil
	}

	n := root.FilesLength()
	var fn arcidxfb.FileNode
	lo := sort.Search(n, func(i int) bool {
		root.Files(&fn, i)
		return fn.PathId() >= pathID
	})

	var res []FileNode
	i := lo
	for i < n {
		root.Files(&fn, i)
		if fn.PathId() != pathID {
			break
		}
		curName := fn.Name()

		var latest arcidxfb.FileNode
		latestSet := false
		var latestOffset uint64
		j := i
		for j < n {
			root.Files(&fn, j)
			if fn.PathId() != pathID || !bytes.Equal(fn.Name(), curName) {
				break
			}
			if !latestSet || fn.OffsetHeader() > latestOffset {
				latest = fn
				latestOffset = fn.OffsetHeader()
				latestSet = true
			}
			j++
		}
		res = append(res, *idx.fbToFileNode(fullPath, string(curName), &latest))
		i = j
	}
	return res, nil
}

// RecursiveSize sums the size of the latest version of every file at or
// below p (p itself included), matching sqlite_enabled.go's semantics.
func (idx *ArcidxIndex) RecursiveSize(p string) (int64, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return 0, errArcidxClosed
	}
	dir, name := normalizePath(p)
	fullPath := joinDirName(dir, name)
	if idx.materialized {
		return idx.recursiveSizeMaterialized(fullPath)
	}
	return idx.recursiveSizeZeroCopy(fullPath)
}

func arcidxPathMatches(fullPath, candidate string) bool {
	if fullPath == "/" {
		return true
	}
	return candidate == fullPath || strings.HasPrefix(candidate, fullPath+"/")
}

func (idx *ArcidxIndex) recursiveSizeMaterialized(fullPath string) (int64, error) {
	type key struct {
		PathID uint32
		Name   string
	}
	type latestSize struct {
		Offset uint64
		Size   uint64
	}
	latest := make(map[key]latestSize)
	for i := range idx.files {
		f := &idx.files[i]
		if !arcidxPathMatches(fullPath, idx.paths[f.PathID]) {
			continue
		}
		k := key{f.PathID, f.Name}
		cur, exists := latest[k]
		if !exists || f.OffsetHeader > cur.Offset {
			latest[k] = latestSize{Offset: f.OffsetHeader, Size: f.Size}
		}
	}
	var total int64
	for _, v := range latest {
		total += int64(v.Size)
	}
	return total, nil
}

func (idx *ArcidxIndex) recursiveSizeZeroCopy(fullPath string) (int64, error) {
	root := idx.root
	n := root.FilesLength()
	if fullPath == "/" {
		return idx.sumRangeLatest(0, n), nil
	}

	numPaths := root.PathsLength()
	var total int64
	var fn arcidxfb.FileNode
	for pid := 0; pid < numPaths; pid++ {
		if !arcidxPathMatches(fullPath, string(root.Paths(pid))) {
			continue
		}
		p32 := uint32(pid)
		lo := sort.Search(n, func(i int) bool {
			root.Files(&fn, i)
			return fn.PathId() >= p32
		})
		hi := sort.Search(n, func(i int) bool {
			root.Files(&fn, i)
			return fn.PathId() > p32
		})
		total += idx.sumRangeLatest(lo, hi)
	}
	return total, nil
}

// sumRangeLatest sums the size of the last (highest offset_header) entry of
// every consecutive (path_id, name) run in files[lo:hi]. It relies on files
// being globally sorted by (path_id, name, offset_header).
func (idx *ArcidxIndex) sumRangeLatest(lo, hi int) int64 {
	root := idx.root
	var fn arcidxfb.FileNode
	var total int64
	i := lo
	for i < hi {
		root.Files(&fn, i)
		pid := fn.PathId()
		name := fn.Name()
		var lastSize uint64
		j := i
		for j < hi {
			root.Files(&fn, j)
			if fn.PathId() != pid || !bytes.Equal(fn.Name(), name) {
				break
			}
			lastSize = fn.Size()
			j++
		}
		total += int64(lastSize)
		i = j
	}
	return total
}

func (idx *ArcidxIndex) toFileNode(dir, name string, f *arcidxFileRec) *FileNode {
	m := idx.metas[f.MetadataID]
	n := &FileNode{
		Path:           dir,
		Name:           name,
		OffsetHeader:   int64(f.OffsetHeader),
		Offset:         int64(f.OffsetData),
		Size:           int64(f.Size),
		Mode:           int64(m.Mode),
		ModTime:        time.Unix(0, f.Mtime),
		Type:           m.TypeFlag,
		LinkName:       f.LinkName,
		Uid:            int(m.Uid),
		Gid:            int(m.Gid),
		IsTar:          m.IsTar,
		IsSparse:       m.IsSparse,
		IsGenerated:    m.IsGenerated,
		RecursionDepth: int(m.RecursionDepth),
		Acl:            f.Acl,
	}
	if len(f.Xattrs) > 0 {
		n.Xattrs = make(map[string][]byte, len(f.Xattrs))
		for _, x := range f.Xattrs {
			n.Xattrs[idx.xattrKeys[x.KeyID]] = x.Value
		}
	}
	return n
}

func (idx *ArcidxIndex) fbToFileNode(dir, name string, fn *arcidxfb.FileNode) *FileNode {
	var mt arcidxfb.MetadataTuple
	idx.root.MetadataTuples(&mt, int(fn.MetadataId()))
	n := &FileNode{
		Path:           dir,
		Name:           name,
		OffsetHeader:   int64(fn.OffsetHeader()),
		Offset:         int64(fn.OffsetData()),
		Size:           int64(fn.Size()),
		Mode:           int64(mt.Mode()),
		ModTime:        time.Unix(0, fn.Mtime()),
		Type:           mt.TypeFlag(),
		LinkName:       string(fn.LinkName()),
		Uid:            int(mt.Uid()),
		Gid:            int(mt.Gid()),
		IsTar:          mt.IsTar(),
		IsSparse:       mt.IsSparse(),
		IsGenerated:    mt.IsGenerated(),
		RecursionDepth: int(mt.RecursionDepth()),
	}
	if al := fn.AclLength(); al > 0 {
		n.Acl = append([]byte(nil), fn.AclBytes()...)
	}
	if xl := fn.XattrsLength(); xl > 0 {
		n.Xattrs = make(map[string][]byte, xl)
		var xa arcidxfb.Xattr
		for i := 0; i < xl; i++ {
			fn.Xattrs(&xa, i)
			key := string(idx.root.XattrKeys(int(xa.KeyId())))
			n.Xattrs[key] = append([]byte(nil), xa.ValueBytes()...)
		}
	}
	return n
}

// InsertBlockOffsets stores the (blockoffset, dataoffset) pairs used for
// random access into block-based compression formats (ZSTD, BZIP2). Unlike
// sqlite's per-key INSERT OR REPLACE table, this schema only has one
// compression_format/compression_blob pair for the whole index, so this is a
// wholesale replace, not an incremental merge; see the package doc comment.
func (idx *ArcidxIndex) InsertBlockOffsets(table string, offsets []BlockOffset) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return errArcidxClosed
	}
	idx.materialize()

	sorted := make([]BlockOffset, len(offsets))
	copy(sorted, offsets)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DataOffset < sorted[j].DataOffset })

	blob := make([]byte, len(sorted)*16)
	for i, o := range sorted {
		binary.LittleEndian.PutUint64(blob[i*16:], uint64(o.BlockOffset))
		binary.LittleEndian.PutUint64(blob[i*16+8:], uint64(o.DataOffset))
	}
	idx.compressionFormat = table
	idx.compressionBlob = blob
	return nil
}

// GetClosestBlockOffset returns the entry with the largest DataOffset that
// is still <= targetDataOffset, binary-searching the compression_blob.
func (idx *ArcidxIndex) GetClosestBlockOffset(table string, targetDataOffset int64) (*BlockOffset, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return nil, errArcidxClosed
	}
	format, blob := idx.getCompressionBlob()
	if format != table || len(blob) == 0 || len(blob)%16 != 0 {
		return nil, ErrArcidxNotFound
	}

	n := len(blob) / 16
	best := -1
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		do := int64(binary.LittleEndian.Uint64(blob[mid*16+8:]))
		if do <= targetDataOffset {
			best = mid
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if best < 0 {
		return nil, ErrArcidxNotFound
	}
	bo := int64(binary.LittleEndian.Uint64(blob[best*16:]))
	do := int64(binary.LittleEndian.Uint64(blob[best*16+8:]))
	return &BlockOffset{BlockOffset: bo, DataOffset: do}, nil
}

func (idx *ArcidxIndex) getCompressionBlob() (string, []byte) {
	if idx.materialized {
		return idx.compressionFormat, idx.compressionBlob
	}
	if idx.root == nil {
		return "", nil
	}
	return string(idx.root.CompressionFormat()), idx.root.CompressionBlobBytes()
}

// GetGzipIndex returns the raw serialized gzip random-access index
// previously stored with SaveGzipIndex, or (nil, nil) if none was saved.
func (idx *ArcidxIndex) GetGzipIndex() ([]byte, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return nil, errArcidxClosed
	}
	format, blob := idx.getCompressionBlob()
	if format != arcidxGzipFormat || len(blob) == 0 {
		return nil, nil
	}
	return append([]byte(nil), blob...), nil
}

// SaveGzipIndex stores data (as produced by GzipIndexExporter) as the
// index's compression blob.
func (idx *ArcidxIndex) SaveGzipIndex(data []byte) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.closed {
		return errArcidxClosed
	}
	idx.materialize()
	idx.compressionFormat = arcidxGzipFormat
	idx.compressionBlob = append([]byte(nil), data...)
	return nil
}
