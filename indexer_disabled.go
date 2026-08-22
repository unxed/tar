//go:build freebsd || openbsd || netbsd || dragonfly || solaris || illumos

package tar

func IndexArchive(archivePath, indexPath string) error {
	return errNoSqlite
}
