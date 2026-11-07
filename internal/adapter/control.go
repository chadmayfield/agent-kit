package adapter

import (
	"strings"
	"unicode/utf8"
)

// StripControl removes control characters from s so that tool output
// printed to a terminal cannot act on it. It keeps \n and \t. It drops an
// escape sequence whole: CSI, OSC, DCS and the other string sequences in
// their ESC or C1 form, and the short ESC forms. It drops every other C0 or
// C1 control character, DEL, U+2028, U+2029 and each invalid UTF-8 byte.
// A string sequence with no terminator loses only its introducer.
func StripControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n', r == '\t':
			b.WriteByte(byte(r))
			i += size
		case r == 0x1b:
			i += escapeLen(s[i:])
		case r == 0x9b:
			i += size + csiLen(s[i+size:])
		case r == 0x90, r == 0x98, r == 0x9d, r == 0x9e, r == 0x9f:
			i += size + stringLen(s[i+size:])
		case isControl(r, size):
			i += size
		default:
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

// escapeLen returns the length of the escape sequence that starts s. s
// starts with ESC.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch c := s[1]; {
	case c == '[':
		return 2 + csiLen(s[2:])
	case c == ']', c == 'P', c == 'X', c == '^', c == '_':
		return 2 + stringLen(s[2:])
	case c >= 0x20 && c <= 0x2f:
		// Intermediate bytes, then one final byte.
		n := 1
		for n < len(s) && s[n] >= 0x20 && s[n] <= 0x2f {
			n++
		}
		if n < len(s) && s[n] >= 0x30 && s[n] <= 0x7e {
			n++
		}
		return n
	case c >= 0x30 && c <= 0x7e:
		return 2
	}
	return 1
}

// csiLen returns the length of a CSI body: parameter and intermediate
// bytes, then one final byte. It stops early at any other byte.
func csiLen(s string) int {
	for n := 0; n < len(s); n++ {
		c := s[n]
		switch {
		case c >= 0x20 && c <= 0x3f:
		case c >= 0x40 && c <= 0x7e:
			return n + 1
		default:
			return n
		}
	}
	return len(s)
}

// stringLen returns the length of a string sequence body through its
// terminator: BEL, ESC \ or U+009C. It returns 0 when there is none.
func stringLen(s string) int {
	for n := 0; n < len(s); n++ {
		switch {
		case s[n] == 0x07:
			return n + 1
		case s[n] == 0x1b && n+1 < len(s) && s[n+1] == '\\':
			return n + 2
		case s[n] == 0xc2 && n+1 < len(s) && s[n+1] == 0x9c:
			return n + 2
		}
	}
	return 0
}
