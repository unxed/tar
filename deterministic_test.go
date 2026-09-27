package tar

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// archiveDeterministicOnce walks srcDir into the {path: FileInfo} map
// Archive expects (the same pattern Compress uses), archives it with Store
// (no compression) and no embedded index - so the only thing under test is
// the plain .tar byte stream itself - and returns the resulting file's raw
// bytes.
func archiveDeterministicOnce(t *testing.T, chroot, srcDir, archivePath string) []byte {
	t.Helper()

	files := make(map[string]os.FileInfo)
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[path] = info
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir failed: %v", err)
	}

	a, err := NewArchiver(archivePath, chroot,
		WithArchiverMethod(Store),
		WithArchiverEmbeddedIndex(false),
		WithArchiverDeterministic(true),
	)
	if err != nil {
		t.Fatalf("NewArchiver failed: %v", err)
	}

	if err := a.Archive(context.Background(), files); err != nil {
		a.Close()
		t.Fatalf("Archive failed: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("ReadFile of %s failed: %v", archivePath, err)
	}
	return data
}

// TestDeterministicArchiveIsByteIdentical archives the same file tree
// twice, with a real delay in between during which every file is rewritten
// with byte-identical content (so each gets a brand-new, real mtime from
// the filesystem), and checks that WithArchiverDeterministic makes the two
// resulting .tar files byte-for-byte identical.
//
// Without the fix, this is expected to fail: FileInfoHeader captures each
// file's real (and now different) mtime, so the same content regenerated
// at a different time - exactly the reproducible-builds scenario the
// torrentzip-style "torrenttar" mode targets - would produce a different
// archive both times.
func TestDeterministicArchiveIsByteIdentical(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")

	writeTree := func() {
		if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0755); err != nil {
			t.Fatalf("MkdirAll failed: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(srcDir, "empty"), 0755); err != nil {
			t.Fatalf("MkdirAll failed: %v", err)
		}
		mustWrite := func(rel, content string) {
			p := filepath.Join(srcDir, rel)
			if err := os.WriteFile(p, []byte(content), 0644); err != nil {
				t.Fatalf("WriteFile(%s) failed: %v", rel, err)
			}
		}
		// Written out of alphabetical order on purpose: entry order in the
		// resulting archive must not depend on this creation order (nor on
		// filesystem/map traversal order), only on the sort Archive does.
		mustWrite("zebra.txt", "zebra content")
		mustWrite(filepath.Join("sub", "beta.txt"), "beta content")
		mustWrite("alpha.txt", "alpha content")
	}

	writeTree()
	archive1 := archiveDeterministicOnce(t, tmpDir, srcDir, filepath.Join(tmpDir, "out1.tar"))

	// A real delay, then rewrite every file with the exact same bytes but a
	// brand-new real mtime, so any real timestamp read from disk during
	// archiving is guaranteed to differ between the two runs unless
	// deterministic mode actually normalizes it away.
	time.Sleep(1200 * time.Millisecond)
	writeTree()
	archive2 := archiveDeterministicOnce(t, tmpDir, srcDir, filepath.Join(tmpDir, "out2.tar"))

	if !bytes.Equal(archive1, archive2) {
		t.Fatalf("deterministic archives differ even though the source content is identical: len1=%d len2=%d", len(archive1), len(archive2))
	}
}

// TestApplyDeterministicHeaderNormalizesMetadata unit-tests the header
// normalization applied by WithArchiverDeterministic directly: real
// timestamps, real uid/gid/owner names and platform-specific extended
// (xattr/ACL) PAX records must be cleared, while unrelated PAX records are
// left untouched.
func TestApplyDeterministicHeaderNormalizesMetadata(t *testing.T) {
	hdr := &Header{
		Name:       "example.txt",
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
		Uid:        1234,
		Gid:        5678,
		Uname:      "someone",
		Gname:      "somegroup",
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.foo":     "bar",
			"LIBARCHIVE.xattr.user.baz": "qux",
			"MSWINDOWS.raw_sd":          "acl-blob",
			"keep.me":                   "yes",
		},
	}

	applyDeterministicHeader(hdr, time.Time{})

	if !hdr.ModTime.Equal(time.Unix(0, 0).UTC()) {
		t.Errorf("ModTime not normalized to the Unix epoch: got %v", hdr.ModTime)
	}
	if !hdr.AccessTime.IsZero() || !hdr.ChangeTime.IsZero() {
		t.Errorf("AccessTime/ChangeTime not cleared: %v / %v", hdr.AccessTime, hdr.ChangeTime)
	}
	if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
		t.Errorf("owner metadata not cleared: uid=%d gid=%d uname=%q gname=%q", hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
	}
	for _, k := range []string{"SCHILY.xattr.user.foo", "LIBARCHIVE.xattr.user.baz", "MSWINDOWS.raw_sd"} {
		if _, ok := hdr.PAXRecords[k]; ok {
			t.Errorf("platform-specific PAX record %q was not stripped", k)
		}
	}
	if v, ok := hdr.PAXRecords["keep.me"]; !ok || v != "yes" {
		t.Errorf("unrelated PAX record was dropped: %v", hdr.PAXRecords)
	}

	// A caller-supplied fixed time must be honored instead of the epoch
	// default (e.g. a build's SOURCE_DATE_EPOCH).
	custom := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	hdr2 := &Header{}
	applyDeterministicHeader(hdr2, custom)
	if !hdr2.ModTime.Equal(custom) {
		t.Errorf("custom fixed time not applied: got %v want %v", hdr2.ModTime, custom)
	}
}
