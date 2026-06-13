package tar

import (
	"encoding/binary"
	"io"
	"strings"
)

// F4 Magic Constants
var magicF4IDX = []byte("F4IDX\x00\x00\x00") // 8 bytes

// WriteMagicFooter appends a physical footer at the end of the archive so TarFS can locate
// the shadow stream in O(1) time without scanning backwards.
func WriteMagicFooter(w io.Writer, method uint16, shadowStart, shadowSize int64) error {
	var footer []byte

	switch method {
	case Store:
		// Simply append 24 bytes at EOF
		// [8 bytes Shadow Offset] [8 bytes Shadow Size] [8 bytes Magic]
		buf := make([]byte, 24)
		binary.LittleEndian.PutUint64(buf[0:8], uint64(shadowStart))
		binary.LittleEndian.PutUint64(buf[8:16], uint64(shadowSize))
		copy(buf[16:24], magicF4IDX)
		footer = buf

	case ZSTD:
		// ZSTD Skippable Frame
		// Frame Magic (4) + Frame Size (4) + [Offset (8) + Size (8) + F4IDX (8)] = 32 bytes total.
		// Standard zstd decoders safely ignore this frame.
		buf := make([]byte, 32)
		binary.LittleEndian.PutUint32(buf[0:4], 0x184D2A50) // Skippable Magic
		binary.LittleEndian.PutUint32(buf[4:8], 24)         // Frame Payload Size
		binary.LittleEndian.PutUint64(buf[8:16], uint64(shadowStart))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(shadowSize))
		copy(buf[24:32], magicF4IDX)
		footer = buf

	case GZIP:
		// GZIP Stream with empty payload but containing an FEXTRA field.
		// GZIP RFC 1952 permits extra fields.
		// Header (10) + XLEN (2) + Extra Payload (28) + Empty Deflate (5) + Footer (8) = 53 bytes
		buf := make([]byte, 53)

		// 1. GZIP Header
		buf[0], buf[1] = 0x1f, 0x8b // Magic
		buf[2] = 0x08               // Deflate
		buf[3] = 0x04               // FEXTRA flag set
		// buf[4:10] are MTIME, XFL, OS (leave 0)

		// 2. XLEN
		binary.LittleEndian.PutUint16(buf[10:12], 28)

		// 3. FEXTRA Payload
		buf[12], buf[13] = 'F', '4' // Subfield ID
		binary.LittleEndian.PutUint16(buf[14:16], 24) // Subfield length
		binary.LittleEndian.PutUint64(buf[16:24], uint64(shadowStart))
		binary.LittleEndian.PutUint64(buf[24:32], uint64(shadowSize))
		copy(buf[32:40], magicF4IDX)

		// 4. Empty Deflate Block (BFINAL=1, BTYPE=00)
		buf[40], buf[41], buf[42], buf[43], buf[44] = 0x01, 0x00, 0x00, 0xff, 0xff

		// 5. GZIP Footer (CRC32=0, ISIZE=0)
		// buf[45:53] are automatically 0
		footer = buf

	case XZ:
		// For XZ we could use Stream Padding, but it's simpler to append a valid empty XZ stream
		// with a custom block. However, XZ is less used for random access since we rely on its native index.
		// For now, if someone uses XZ with embedded index, we'll append a dummy uncompressed stream.
		// Skipping XZ implementation for this iteration, returning nil.
		return nil

	default:
		return nil
	}

	_, err := w.Write(footer)
	return err
}

// LocateShadowStream finds the F4SS Magic Footer and returns the physical offset and size of Stream 2.
func LocateShadowStream(ra io.ReaderAt, fileSize int64, method uint16) (int64, int64, error) {
	if fileSize < 53 {
		return 0, 0, nil // Too small
	}

	var footerSize int64
	switch method {
	case Store:
		footerSize = 24
	case ZSTD:
		footerSize = 32
	case GZIP:
		footerSize = 53
	default:
		return 0, 0, nil
	}

	buf := make([]byte, footerSize)
	if _, err := ra.ReadAt(buf, fileSize-footerSize); err != nil {
		return 0, 0, nil
	}

	var shadowStart, shadowSize uint64

	switch method {
	case Store:
		if string(buf[16:24]) != string(magicF4IDX) {
			return 0, 0, nil
		}
		shadowStart = binary.LittleEndian.Uint64(buf[0:8])
		shadowSize = binary.LittleEndian.Uint64(buf[8:16])
	case ZSTD:
		if string(buf[24:32]) != string(magicF4IDX) {
			return 0, 0, nil
		}
		shadowStart = binary.LittleEndian.Uint64(buf[8:16])
		shadowSize = binary.LittleEndian.Uint64(buf[16:24])
	case GZIP:
		if string(buf[32:40]) != string(magicF4IDX) {
			return 0, 0, nil
		}
		shadowStart = binary.LittleEndian.Uint64(buf[16:24])
		shadowSize = binary.LittleEndian.Uint64(buf[24:32])
	default:
		return 0, 0, nil
	}

	if shadowStart == 0 || shadowSize == 0 || int64(shadowStart+shadowSize) > fileSize-footerSize {
		return 0, 0, io.ErrUnexpectedEOF // Corrupted footer
	}

	return int64(shadowStart), int64(shadowSize), nil
}

// extractShadowIndex looks for the F4IDX magic footer and, if found, extracts and returns the SQLite database payload.
func extractShadowIndex(ra io.ReaderAt, fileSize int64, method uint16) ([]byte, error) {
	return extractShadowFile(ra, fileSize, method, ".tarext/ratarmount/index.sqlite")
}

// extractShadowFile extracts a specific file payload from the Stream 2 metadata shadow stream.
func extractShadowFile(ra io.ReaderAt, fileSize int64, method uint16, targetName string) ([]byte, error) {
	shadowStart, shadowSize, err := LocateShadowStream(ra, fileSize, method)
	if err != nil || shadowSize == 0 {
		return nil, err
	}

	sr := io.NewSectionReader(ra, shadowStart, shadowSize)
	var rd io.Reader = sr

	if method != Store {
		di, ok := decompressors.Load(method)
		if !ok {
			return nil, ErrAlgorithm
		}
		dcomp, err := di.(Decompressor).Decompress(rd)
		if err != nil {
			return nil, err
		}
		defer dcomp.Close()
		rd = dcomp
	}

	tr := NewReader(rd)
	var payload []byte

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if hdr.Name == targetName {
			payload, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			break
		}
	}

	if payload == nil {
		return nil, io.ErrUnexpectedEOF
	}

	return payload, nil
}

func serializeProperties(props map[string]string) []byte {
	var sb strings.Builder
	for k, v := range props {
		sb.WriteString(k + "=" + v + "\n")
	}
	return []byte(sb.String())
}

func parseProperties(data []byte) map[string]string {
	props := make(map[string]string)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			props[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return props
}