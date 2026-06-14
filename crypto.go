package tar

import (
    "os"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
)

// XCryptHeader represents the 93-byte binary header for encrypted streams
type XCryptHeader struct {
	Version    uint8
	KdfAlgo    uint8
	Cipher     uint8
	Iterations uint32
	Salt       []byte
	IV         []byte
	MAC        []byte
}

// pbkdf2 implements PBKDF2-HMAC-SHA256 to avoid dependency on golang.org/x/crypto.
func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)

	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		dkBlock := prf.Sum(U[:0])

		T := make([]byte, hashLen)
		copy(T, dkBlock)

		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(dkBlock)
			dkBlock = prf.Sum(dkBlock[:0])
			for j := range dkBlock {
				T[j] ^= dkBlock[j]
			}
		}
		dk = append(dk, T...)
	}
	return dk[:keyLen]
}

// generateF4CryptHeader creates a new secure header and derives the AES-256 key.
func generateXCryptHeader(password string, iterations int) (*XCryptHeader, []byte, error) {
	if iterations == 0 {
		iterations = 600000 // Recommended default
	}

	salt := make([]byte, 32)
	iv := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, nil, err
	}
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, nil, err
	}

	key := pbkdf2([]byte(password), salt, iterations, 32)

	hdr := &XCryptHeader{
		Version:    1,
		KdfAlgo:    1,
		Cipher:     1,
		Iterations: uint32(iterations),
		Salt:       salt,
		IV:         iv,
		MAC:        make([]byte, 32),
	}
	return hdr, key, nil
}

func parseXCryptHeader(data []byte) (*XCryptHeader, error) {
	if len(data) != 93 {
		return nil, errors.New("tar: invalid XCrypt header size")
	}
	if string(data[0:6]) != "XCRYPT" {
		return nil, errors.New("tar: invalid XCrypt magic signature")
	}
	if data[6] != 1 {
		return nil, errors.New("tar: unsupported XCrypt version")
	}
	if data[7] != 1 {
		return nil, errors.New("tar: unsupported XCrypt KDF algorithm")
	}
	if data[8] != 1 {
		return nil, errors.New("tar: unsupported XCrypt cipher")
	}

	hdr := &XCryptHeader{
		Version:    data[6],
		KdfAlgo:    data[7],
		Cipher:     data[8],
		Iterations: binary.LittleEndian.Uint32(data[9:13]),
		Salt:       make([]byte, 32),
		IV:         make([]byte, 16),
		MAC:        make([]byte, 32),
	}
	copy(hdr.Salt, data[13:45])
	copy(hdr.IV, data[45:61])
	copy(hdr.MAC, data[61:93])

	return hdr, nil
}

func (h *XCryptHeader) Encode() []byte {
	b := make([]byte, 93)
	copy(b[0:6], "XCRYPT")
	b[6] = h.Version
	b[7] = h.KdfAlgo
	b[8] = h.Cipher
	binary.LittleEndian.PutUint32(b[9:13], h.Iterations)
	copy(b[13:45], h.Salt)
	copy(b[45:61], h.IV)
	copy(b[61:93], h.MAC)
	return b
}

func (h *XCryptHeader) DeriveKey(password string) []byte {
	return pbkdf2([]byte(password), h.Salt, int(h.Iterations), 32)
}

// addIV adds a block offset to the 128-bit big-endian CTR initialization vector
func addIV(baseIV []byte, offset uint64) []byte {
	iv := make([]byte, 16)
	copy(iv, baseIV)
	var carry uint64 = offset
	for i := 15; i >= 0 && carry > 0; i-- {
		sum := uint64(iv[i]) + (carry & 0xFF)
		iv[i] = byte(sum)
		carry = (carry >> 8) + (sum >> 8)
	}
	return iv
}

// f4CryptWriter encrypts data on the fly and calculates the MAC of the ciphertext
type xCryptWriter struct {
	w      io.Writer
	stream cipher.Stream
	mac    hash.Hash
}

func newXCryptWriter(w io.Writer, key, iv []byte) (*xCryptWriter, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, iv)
	mac := hmac.New(sha256.New, key)

	return &xCryptWriter{
		w:      w,
		stream: stream,
		mac:    mac,
	}, nil
}

func (cw *xCryptWriter) Write(p []byte) (int, error) {
	enc := make([]byte, len(p))
	cw.stream.XORKeyStream(enc, p)
	cw.mac.Write(enc)
	return cw.w.Write(enc)
}

func (cw *xCryptWriter) MAC() []byte {
	return cw.mac.Sum(nil)
}

// xCryptReaderAt provides O(1) random access decryption using AES-CTR properties
type xCryptReaderAt struct {
	r   io.ReaderAt
	key []byte
	iv  []byte
}

func newXCryptReaderAt(r io.ReaderAt, key, iv []byte) *xCryptReaderAt {
	return &xCryptReaderAt{r: r, key: key, iv: iv}
}

func (cr *xCryptReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	blockOffset := uint64(off / 16)
	rem := int(off % 16)

	readSize := len(p) + rem
	encBuf := make([]byte, readSize)

	n, err := cr.r.ReadAt(encBuf, off-int64(rem))
	if n == 0 && err != nil {
		return 0, err
	}

	encBuf = encBuf[:n]

	c, errC := aes.NewCipher(cr.key)
	if errC != nil {
		return 0, errC
	}

	ctrIV := addIV(cr.iv, blockOffset)
	stream := cipher.NewCTR(c, ctrIV)

	decBuf := make([]byte, n)
	stream.XORKeyStream(decBuf, encBuf)

	copied := copy(p, decBuf[rem:])

	// If we successfully fulfilled the request but hit EOF, mask it to allow smooth reads
	if err == io.EOF && copied == len(p) {
		return copied, nil
	}

	return copied, err
}// encapsulateF4Crypt wraps the temp archive in a standard F4Crypt outer layer.
func encapsulateXCrypt(finalPath, tempPath, password string) error {
	out, err := os.Create(finalPath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Stream 1: Plaintext Stub
	stubTar := NewWriter(out)
	stubMsg := []byte("This is an encrypted archive. Please use f4 or an AXS-compatible tool to extract it.\n")
	stubTar.WriteHeader(&Header{Name: "README_ENCRYPTED.txt", Size: int64(len(stubMsg)), Mode: 0644})
	stubTar.Write(stubMsg)
	stubTar.Close()

	fi, _ := out.Stat()
	shadowStart := fi.Size()

	cHdr, key, err := generateXCryptHeader(password, 600000)
	if err != nil {
		return err
	}

	// Stream 2: Encrypted Payload
	shadowTar := NewWriter(out)
	shadowTar.WriteHeader(&Header{Name: ".tarext/", Mode: 0755, Typeflag: TypeDir})
	shadowTar.WriteHeader(&Header{Name: ".tarext/xcrypt/", Mode: 0755, Typeflag: TypeDir})

	tempFi, err := os.Stat(tempPath)
	if err != nil {
		return err
	}

	shadowTar.WriteHeader(&Header{Name: ".tarext/xcrypt/payload.enc", Size: tempFi.Size(), Mode: 0644})

	in, err := os.Open(tempPath)
	if err != nil {
		return err
	}
	cw, _ := newXCryptWriter(shadowTar, key, cHdr.IV)
	io.Copy(cw, in)
	in.Close()

	cHdr.MAC = cw.MAC()

	shadowTar.WriteHeader(&Header{Name: ".tarext/xcrypt/crypto.hdr", Size: 93, Mode: 0644})
	shadowTar.Write(cHdr.Encode())
	shadowTar.Close()

	fi, _ = out.Stat()
	shadowSize := fi.Size() - shadowStart

	return WriteMagicFooter(out, Store, shadowStart, shadowSize)
}

// checkXCrypt identifies XCrypt outer layers and returns a transparent decrypted ReaderAt.
func checkXCrypt(ra io.ReaderAt, size int64, password string) (io.ReaderAt, int64, error) {
	method, err := DetectFormat(ra)
	if err != nil || method != Store {
		return ra, size, nil
	}

	shadowStart, shadowSize, err := LocateShadowStream(ra, size, method)
	if err != nil || shadowSize == 0 {
		return ra, size, nil
	}

	sr := io.NewSectionReader(ra, shadowStart, shadowSize)
	tr := &trackingReader{r: sr}
	tarr := NewReader(tr)

	var cHdr *XCryptHeader
	var pOffset, pSize int64
	var foundCrypto, foundPayload bool

	for {
		hdr, err := tarr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ra, size, nil
		}

		if hdr.Name == ".tarext/xcrypt/crypto.hdr" {
			data, _ := io.ReadAll(tarr)
			cHdr, err = parseXCryptHeader(data)
			if err != nil {
				return nil, 0, err
			}
			foundCrypto = true
		} else if hdr.Name == ".tarext/xcrypt/payload.enc" {
			pOffset = shadowStart + tr.pos
			pSize = hdr.Size
			foundPayload = true
			io.Copy(io.Discard, tarr)
		}
	}

	if !foundCrypto || !foundPayload {
		return ra, size, nil
	}

	if password == "" {
		return ra, size, nil // Return legacy unencrypted view of the stub (README)
	}

	key := cHdr.DeriveKey(password)
	payloadSection := io.NewSectionReader(ra, pOffset, pSize)
	decReader := newXCryptReaderAt(payloadSection, key, cHdr.IV)

	return decReader, pSize, nil
}
