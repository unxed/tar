package tar

import (
	"strings"
	"time"
)

// applyDeterministicHeader normalizes every field of hdr that would
// otherwise leak non-reproducible, host- or run-specific information into
// the archive: the source file's real modification/access/change
// timestamps, the numeric uid/gid and resolved user/group names of
// whoever ran the archiver, and platform-specific extended metadata
// (POSIX ACLs, SELinux labels, other xattrs, Windows security
// descriptors) captured via PAX records.
//
// This is the building block behind WithArchiverDeterministic: applied to
// every entry on top of the alphabetical entry order Archive already
// produces, it guarantees that archiving the same file tree twice - on
// any machine, as any user, and even if the source files were
// regenerated with identical content at a different time - yields a
// byte-identical .tar, analogous to the torrentzip convention for ZIP.
func applyDeterministicHeader(hdr *Header, fixedTime time.Time) {
	if fixedTime.IsZero() {
		fixedTime = time.Unix(0, 0).UTC()
	}

	// The real mtime of the source file (and, on non-USTAR-safe values,
	// the atime/ctime archive/tar would otherwise fold into a PAX record)
	// is the main source of run-to-run non-determinism for reproducible
	// builds: identical file *content* regenerated at a different time
	// still gets a different mtime.
	hdr.ModTime = fixedTime
	hdr.AccessTime = time.Time{}
	hdr.ChangeTime = time.Time{}

	// The uid/gid and resolved owner/group names depend on whoever is
	// running the archiver and on that machine's /etc/passwd, not on the
	// archived content.
	hdr.Uid = 0
	hdr.Gid = 0
	hdr.Uname = ""
	hdr.Gname = ""

	// Strip platform-specific extended metadata that encodes the host's
	// or filesystem's security context (POSIX ACLs, SELinux labels, other
	// xattrs, Windows security descriptors) rather than the file's actual
	// content.
	if hdr.PAXRecords != nil {
		for k := range hdr.PAXRecords {
			if k == "MSWINDOWS.raw_sd" ||
				strings.HasPrefix(k, "SCHILY.xattr.") ||
				strings.HasPrefix(k, "LIBARCHIVE.xattr.") {
				delete(hdr.PAXRecords, k)
			}
		}
		if len(hdr.PAXRecords) == 0 {
			hdr.PAXRecords = nil
		}
	}
}
