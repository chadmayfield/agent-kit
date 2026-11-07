// Package prompt assembles the prompt text that ask sends to a CLI.
//
// Build joins its parts in this order, with a blank line between parts:
//
//  1. the argument text, joined with spaces
//  2. stdin, only when the arguments hold a lone "-"
//  3. each -f file, in flag order, as "--- <basename> ---\n<content>"
package prompt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Build returns the assembled prompt. A lone "-" in args is removed and
// means "read stdin"; it is an error when stdin is a terminal or when "-"
// appears more than once. An empty prompt is an error.
func Build(args []string, files []string, stdin io.Reader, stdinIsTerminal bool) (string, error) {
	var words []string
	dashes := 0
	for _, a := range args {
		if a == "-" {
			dashes++
			continue
		}
		words = append(words, a)
	}
	if dashes > 1 {
		return "", errors.New(`"-" can appear only once`)
	}

	var parts []string
	if text := strings.Join(words, " "); strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	}

	if dashes == 1 {
		if stdinIsTerminal {
			return "", errors.New(`"-" reads the prompt from stdin, but stdin is a terminal; pipe the text in`)
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		if s := string(data); strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read prompt file: %w", err)
		}
		parts = append(parts, "--- "+filepath.Base(f)+" ---\n"+string(data))
	}

	if len(parts) == 0 {
		return "", errors.New("empty prompt: give prompt text, a -f file, or \"-\" with piped stdin")
	}
	return strings.Join(parts, "\n\n"), nil
}
