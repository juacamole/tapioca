// Package textenc decodes file bytes into clean UTF-8 for prompts and
// transcripts: UTF-16 (BOM or NUL-pattern), Latin-1 fallback, and binary
// detection so object files never get inlined as glyph soup.
package textenc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Decode returns the text content of data and whether it looks like text at
// all; ok=false means binary.
func Decode(data []byte) (string, bool) {
	if len(data) == 0 {
		return "", true
	}
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		return utf16String(data[2:], binary.LittleEndian), true
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		return utf16String(data[2:], binary.BigEndian), true
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}

	if bytes.IndexByte(data, 0) >= 0 {
		odd, even := 0, 0
		for i, b := range data {
			if b == 0 {
				if i%2 == 1 {
					odd++
				} else {
					even++
				}
			}
		}
		half := len(data) / 2
		if half > 0 && odd > half*6/10 {
			return utf16String(data, binary.LittleEndian), true
		}
		if half > 0 && even > half*6/10 {
			return utf16String(data, binary.BigEndian), true
		}
		return "", false
	}

	ctrl := 0
	for _, b := range data {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			ctrl++
		}
	}
	if ctrl*20 > len(data) {
		return "", false
	}

	if utf8.Valid(data) {
		return string(data), true
	}
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		b.WriteRune(rune(c)) // Latin-1: byte value == code point
	}
	return b.String(), true
}

func utf16String(data []byte, ord binary.ByteOrder) string {
	u := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		u = append(u, ord.Uint16(data[i:]))
	}
	return string(utf16.Decode(u))
}
