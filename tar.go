package tar

import (
	"archive/tar"
	"io"
	"os"
)

func NewWriter(w io.Writer) *tar.Writer {
	return tar.NewWriter(w)
}

func NewReader(r io.Reader) *tar.Reader {
	return tar.NewReader(r)
}

// Drop-in replacements for standard archive/tar types
type Header = tar.Header
type Reader = tar.Reader
type Writer = tar.Writer
type Format = tar.Format

const (
	TypeReg           = tar.TypeReg
	TypeRegA          = tar.TypeRegA
	TypeLink          = tar.TypeLink
	TypeSymlink       = tar.TypeSymlink
	TypeChar          = tar.TypeChar
	TypeBlock         = tar.TypeBlock
	TypeDir           = tar.TypeDir
	TypeFifo          = tar.TypeFifo
	TypeCont          = tar.TypeCont
	TypeXHeader       = tar.TypeXHeader
	TypeXGlobalHeader = tar.TypeXGlobalHeader
	TypeGNUSparse     = tar.TypeGNUSparse
	TypeGNULongName   = tar.TypeGNULongName
	TypeGNULongLink   = tar.TypeGNULongLink
	TypeVol           = 'V' // GNUTYPE_VOLHDR (Volume header)
	TypeGNUDumpDir    = 'D' // GNUTYPE_DUMPDIR
	TypeGNUMultiVol   = 'M' // GNUTYPE_MULTIVOL
)

const (
	FormatUnknown = tar.FormatUnknown
	FormatUSTAR   = tar.FormatUSTAR
	FormatPAX     = tar.FormatPAX
	FormatGNU     = tar.FormatGNU
)

var (
	ErrHeader          = tar.ErrHeader
	ErrWriteTooLong    = tar.ErrWriteTooLong
	ErrFieldTooLong    = tar.ErrFieldTooLong
	ErrWriteAfterClose = tar.ErrWriteAfterClose
)

// Abstraction hooks for NTFS security and stream operations to support unit testing on non-Windows platforms.
var (
	getFileSecurityFunc           = getFileSecurity
	applyNtfsAclFunc              = applyNtfsAcl
	getAlternativeDataStreamsFunc = getAlternativeDataStreams
)

func FileInfoHeader(fi os.FileInfo, link string) (*Header, error) {
	return tar.FileInfoHeader(fi, link)
}
