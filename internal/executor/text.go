package executor

import (
	"strings"
	"unicode/utf8"
)

// toValidUTF8 makes output storable in a PostgreSQL text column: invalid
// bytes, a rune cut at the start by truncation and NUL are replaced.
func toValidUTF8(b []byte) string {
	// Skip continuation bytes left at the start of a cut buffer.
	for i := 0; len(b) > 0 && i < utf8.UTFMax && !utf8.RuneStart(b[0]); i++ {
		b = b[1:]
	}

	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}

	return strings.ReplaceAll(s, "\x00", "\uFFFD")
}
