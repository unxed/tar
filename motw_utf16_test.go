package tar

import (
	"bytes"
	"testing"
)

func TestSanitizeMOTW_UTF16LE(t *testing.T) {
	// Содержимое Zone.Identifier в UTF-16LE с BOM
	rawInput := "[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=http://evil.com\r\n"

	var buf bytes.Buffer
	buf.WriteByte(0xFF)
	buf.WriteByte(0xFE)
	for _, r := range rawInput {
		buf.WriteByte(byte(r))
		buf.WriteByte(byte(r >> 8))
	}

	sanitized := sanitizeZoneIdentifier(buf.Bytes())

	if !isUTF16LE(sanitized) {
		t.Fatal("expected output to be UTF-16LE")
	}

	decoded := decodeUTF16LE(sanitized)
	expected := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if decoded != expected {
		t.Errorf("Sanitization failed:\nGot: %q\nWant: %q", decoded, expected)
	}
}
