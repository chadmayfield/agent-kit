// Package adapter turns an ask request into one CLI's command line and
// reads that CLI's output.
package adapter

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/runner"
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
// version. A different version at run time means the test no longer counts.
type Verification struct {
	OK            bool
	TestedVersion string
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
