package adapter

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
)

// DryRun returns the command a run would start, as one shell-quoted line,
// without starting it. It is called before any secret lookup.
//
// env holds NAME=<config key> pairs, never values. DryRun prints the key only
// when it names a secret in tc; a tc.PassEnv variable shows as
// NAME=<pass_env>, and anything else as NAME=<hidden>. The prompt shows as
// <prompt: N bytes>, or <prompt: N bytes, in a temp file> for an adapter that
// passes it in a file. A field with a control character is single-quoted with
// visible escapes.
//
// DryRun calls Build and then Cleanup, so a temp prompt file is created and
// removed again.
func DryRun(a Adapter, req Request, tc config.ToolConfig, env []string) (string, error) {
	inv, err := a.Build(req, tc)
	if err != nil {
		return "", err
	}
	if inv.Cleanup != nil {
		defer inv.Cleanup()
	}

	parts := make([]string, 0, len(env)+len(inv.Args)+2)
	for _, e := range env {
		parts = append(parts, envLabel(e, tc))
	}
	parts = append(parts, shellQuote(inv.Binary))
	for _, arg := range inv.Args {
		parts = append(parts, shellQuote(arg))
	}
	if pf, ok := a.(promptFiler); ok && pf.promptInFile() {
		parts = append(parts, fmt.Sprintf("<prompt: %d bytes, in a temp file>", len(req.Prompt)))
	} else {
		parts = append(parts, fmt.Sprintf("<prompt: %d bytes>", len(req.Prompt)))
	}
	return strings.Join(parts, " "), nil
}

// promptFiler is an adapter that passes the prompt in a temp file instead
// of on stdin.
type promptFiler interface{ promptInFile() bool }

// envLabel shows a NAME=key entry without ever echoing an unknown value.
// The name and the key are shell-quoted the same way as arguments.
func envLabel(entry string, tc config.ToolConfig) string {
	name, key, _ := strings.Cut(entry, "=")
	for _, s := range tc.Secrets {
		if s.Name == key && s.Name != "" {
			return shellQuote(name) + "=<" + shellQuote(key) + ">"
		}
	}
	if slices.Contains(tc.PassEnv, name) {
		return shellQuote(name) + "=<pass_env>"
	}
	return shellQuote(name) + "=<hidden>"
}

// hasControl reports whether s holds a C0 or C1 control character, DEL,
// U+2028, U+2029, a Unicode bidi control or invalid UTF-8.
func hasControl(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isControl(r, size) {
			return true
		}
		i += size
	}
	return false
}

func isControl(r rune, size int) bool {
	return r == utf8.RuneError && size == 1 ||
		r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f ||
		r == 0x2028 || r == 0x2029 ||
		r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

// quoteInert single-quotes s for a POSIX shell and replaces each control
// character with a visible escape such as \n or \x1b. Backslashes are
// doubled.
func quoteInert(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError && size == 1, r < 0x80 && isControl(r, size):
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case isControl(r, size):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return "'" + strings.ReplaceAll(b.String(), "'", `'\''`) + "'"
}

// shellQuote quotes s for a POSIX shell when it holds anything beyond a
// plain set of safe characters. Control characters become visible escapes.
func shellQuote(s string) string {
	if hasControl(s) {
		return quoteInert(s)
	}
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("@%+=:,./_-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
