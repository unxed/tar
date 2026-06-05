package tar

import (
	"bytes"
	"strings"
	"testing"
)

func TestPBKDF2(t *testing.T) {
	// Simple sanity check for local PBKDF2 implementation
	pass := []byte("password")
	salt := []byte("salt")
	key := pbkdf2(pass, salt, 4096, 32)
	if len(key) != 32 {
		t.Fatalf("Expected 32 byte key, got %d", len(key))
	}
}

func TestF4CryptHeader_Roundtrip(t *testing.T) {
	pass := "my_secure_password"
	hdr1, key1, err := generateF4CryptHeader(pass, 1000) // Small iterations for fast testing
	if err != nil {
		t.Fatal(err)
	}

	encoded := hdr1.Encode()
	if len(encoded) != 93 {
		t.Fatalf("Expected header to be exactly 93 bytes, got %d", len(encoded))
	}

	hdr2, err := parseF4CryptHeader(encoded)
	if err != nil {
		t.Fatal(err)
	}

	key2 := hdr2.DeriveKey(pass)

	if !bytes.Equal(key1, key2) {
		t.Fatal("Derived keys do not match after roundtrip")
	}
	if hdr1.Iterations != hdr2.Iterations {
		t.Errorf("Iterations mismatch: %d vs %d", hdr1.Iterations, hdr2.Iterations)
	}
}

func TestF4Crypt_StreamEncryptionAndRandomAccess(t *testing.T) {
	pass := "test_password"
	hdr, key, _ := generateF4CryptHeader(pass, 1000)

	plaintext := []byte(strings.Repeat("0123456789ABCDEF", 1000)) // 16000 bytes

	var cipherBuf bytes.Buffer
	writer, err := newF4CryptWriter(&cipherBuf, key, hdr.IV)
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt sequentially
	writer.Write(plaintext[:8000])
	writer.Write(plaintext[8000:])
	mac := writer.MAC()

	if len(mac) != 32 {
		t.Fatalf("Invalid MAC length: %d", len(mac))
	}

	ciphertext := cipherBuf.Bytes()

	// Decrypt using Random Access Reader
	readerAt := bytes.NewReader(ciphertext)
	decReader := newF4CryptReaderAt(readerAt, key, hdr.IV)

	// Test 1: Read a chunk perfectly aligned with blocks
	buf1 := make([]byte, 32)
	n, err := decReader.ReadAt(buf1, 32)
	if err != nil || n != 32 {
		t.Fatalf("ReadAt aligned failed: %v", err)
	}
	if !bytes.Equal(buf1, plaintext[32:64]) {
		t.Errorf("Aligned read mismatch")
	}

	// Test 2: Read unaligned chunk crossing block boundaries
	buf2 := make([]byte, 20)
	n, err = decReader.ReadAt(buf2, 17) // starts at byte 1 of block 1
	if err != nil || n != 20 {
		t.Fatalf("ReadAt unaligned failed: %v", err)
	}
	if !bytes.Equal(buf2, plaintext[17:37]) {
		t.Errorf("Unaligned read mismatch\nExpected: %s\nGot:      %s", plaintext[17:37], buf2)
	}

	// Test 3: Read near EOF
	buf3 := make([]byte, 16)
	n, err = decReader.ReadAt(buf3, int64(len(plaintext)-10))
	if n != 10 {
		t.Errorf("Expected 10 bytes near EOF, got %d", n)
	}
	if !bytes.Equal(buf3[:10], plaintext[len(plaintext)-10:]) {
		t.Errorf("EOF read mismatch")
	}
}