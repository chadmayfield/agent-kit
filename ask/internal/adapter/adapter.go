// Package adapter turns an ask request into one CLI's command line and
// reads that CLI's output.
package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

// Request is what the user asked for, after prompt assembly.
type Request struct {
	Model  string
	Prompt string
	Dir    string
	Effort string
	Write  bool
}

// Result is what ask reads back from a CLI's output.
type Result struct {
	Final        string
	InputTokens  int
	OutputTokens int
	// Model is the model the CLI names in its own output, or empty.
	Model string
}

// ResolveModel returns the model string passed to the CLI: the tc.Models
// entry for model when it is set and not empty, else model.
func ResolveModel(model string, tc config.ToolConfig) string {
	if m, ok := tc.Models[model]; ok && m != "" {
		return m
	}
	return model
}

// Verification records whether a setup passed its test, and on which CLI
// version. The test counts for every version with the same major number as
// LastTested. A different major version means the test no longer counts.
type Verification struct {
	OK bool
	// LastTested is the trimmed `<binary> --version` output of the install
	// where the test last passed.
	LastTested string
}

// Covers reports whether the test also counts for version: both must name
// the same major version. Below 1.0, where a minor release can change
// behavior, they must also name the same minor version.
func (v Verification) Covers(version string) bool {
	want, ok := MajorVersion(v.LastTested)
	if !ok {
		return false
	}
	got, ok := MajorVersion(version)
	if !ok || got != want {
		return false
	}
	if want != 0 {
		return true
	}
	wantMinor, ok := minorVersion(v.LastTested)
	if !ok {
		return false
	}
	gotMinor, ok := minorVersion(version)
	return ok && gotMinor == wantMinor
}

// Range names the versions the test counts for, such as "2.x" for
// "2.1.319 (Claude Code)" or "0.163.x" for "codex-cli 0.163.0".
func (v Verification) Range() string {
	major, ok := MajorVersion(v.LastTested)
	if !ok {
		return "the tested version"
	}
	if major != 0 {
		return fmt.Sprintf("%d.x", major)
	}
	minor, ok := minorVersion(v.LastTested)
	if !ok {
		return "the tested version"
	}
	return fmt.Sprintf("0.%d.x", minor)
}

// ToolError is an error the tool reported in its own output, such as a
// failed login. Msg is built by ask from fixed words and checked fields,
// never copied from the tool's free text, so it is safe to print.
type ToolError struct {
	Msg string
}

func (e *ToolError) Error() string { return e.Msg }

// minorVersion returns the minor number of the versions in s, failing
// when they disagree.
func minorVersion(s string) (int, bool) {
	vs := FindVersions(s)
	if len(vs) == 0 {
		return 0, false
	}
	for _, v := range vs[1:] {
		if v.Nums[1] != vs[0].Nums[1] {
			return 0, false
		}
	}
	return vs[0].Nums[1], true
}

// Version is one version number found in a CLI's --version output.
type Version struct {
	// Nums holds major, minor and patch. A two-part version has patch 0.
	Nums [3]int
	// Pre is true for a pre-release, such as 3.0.0-beta.
	Pre bool
}

// FindVersions returns every version-like word in s, in order. A word may
// be wrapped in brackets or end in punctuation, may start with "v", and may
// carry a "-..." pre-release or "+..." build suffix: "2.1.325",
// "(3.0.0)", "v1.1.10-beta," all count.
func FindVersions(s string) []Version {
	var out []Version
	for _, word := range strings.Fields(s) {
		word = strings.Trim(word, "()[]{}<>,;:'\"")
		word = strings.TrimPrefix(word, "v")
		pre := false
		if i := strings.IndexAny(word, "-+"); i > 0 {
			pre = word[i] == '-'
			word = word[:i]
		}
		parts := strings.Split(word, ".")
		if len(parts) < 2 || len(parts) > 3 {
			continue
		}
		var v Version
		ok := true
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
				ok = false
				break
			}
			v.Nums[i] = n
		}
		if ok {
			v.Pre = pre
			out = append(out, v)
		}
	}
	return out
}

// MajorVersion returns the major number of the version in s, such as 2 in
// "2.1.325 (Claude Code)" or 0 in "codex-cli 0.163.0". It fails when s has
// no version, or when its versions name different major numbers, so a
// stray number in a notice can never pick the major for the real version.
func MajorVersion(s string) (int, bool) {
	vs := FindVersions(s)
	if len(vs) == 0 {
		return 0, false
	}
	for _, v := range vs[1:] {
		if v.Nums[0] != vs[0].Nums[0] {
			return 0, false
		}
	}
	return vs[0].Nums[0], true
}

// Adapter knows one CLI. Adapters never see secrets: the cli layer adds the
// environment to the Invocation that Build returns.
type Adapter interface {
	Name() string
	Efforts() []string
	// ReadOnly reports whether the read-only setup passed its test.
	ReadOnly() Verification
	// Prompt reports whether passing the prompt off the command line passed
	// its test.
	Prompt() Verification
	Build(req Request, tc config.ToolConfig) (runner.Invocation, error)
	Parse(stdout []byte) (Result, error)
}

// writePromptFile writes prompt to prompt.md, mode 0600, in a new temp
// folder with mode 0700. It returns the file path and a Cleanup that removes
// the folder.
func writePromptFile(tool, prompt string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ask-"+tool+"-")
	if err != nil {
		return "", nil, fmt.Errorf("%s: make temp folder: %w", tool, err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	file := filepath.Join(dir, "prompt.md")
	if err := writeNewFile(dir, file, prompt); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s: write prompt file: %w", tool, err)
	}
	return file, cleanup, nil
}

// writeNewFile sets dir to 0700 and creates file, which must not exist,
// with mode 0600.
func writeNewFile(dir, file, data string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RefusedError means ask refused the run on purpose, for example an
// untested read-only setup or an unsupported effort. ask exits 2 on it.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// Refuse returns a *RefusedError with a formatted message.
func Refuse(format string, args ...any) error {
	return &RefusedError{Msg: fmt.Sprintf(format, args...)}
}
