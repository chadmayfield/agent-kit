//go:build asktest

package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/runner"
)

// The asktest build adds a fake tool named "fake" for the built-binary
// signal test. Release builds never include this file.
func init() { extra = append(extra, fakeSleeper{}) }

// fakeStateEnv names the folder where the fake records its temp folder and
// its child's pid, so the test can check both after the run.
const fakeStateEnv = "ASK_FAKE_STATE"

// fakeScript starts a sleeping child, records the child's pid, and waits.
const fakeScript = `#!/bin/sh
/bin/sleep 300 &
echo $! > "$1/child.tmp"
/bin/mv "$1/child.tmp" "$1/child"
wait
`

type fakeSleeper struct{}

func (fakeSleeper) Name() string           { return "fake" }
func (fakeSleeper) Efforts() []string      { return nil }
func (fakeSleeper) ReadOnly() Verification { return Verification{} }
func (fakeSleeper) Prompt() Verification   { return Verification{OK: true} }

// Build makes a 0700 temp folder holding the fake CLI script and returns a
// Cleanup that removes the folder.
func (fakeSleeper) Build(req Request, _ config.ToolConfig) (runner.Invocation, error) {
	state := os.Getenv(fakeStateEnv)
	if state == "" {
		return runner.Invocation{}, &SetupError{Err: errors.New("fake: " + fakeStateEnv + " is not set")}
	}
	dir, err := os.MkdirTemp("", "ask-fake-")
	if err != nil {
		return runner.Invocation{}, &SetupError{Err: fmt.Errorf("fake: make temp folder: %w", err)}
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	script := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(script, []byte(fakeScript), 0o700); err != nil {
		cleanup()
		return runner.Invocation{}, &SetupError{Err: fmt.Errorf("fake: write script: %w", err)}
	}
	if err := os.WriteFile(filepath.Join(state, "tmpdir"), []byte(dir), 0o600); err != nil {
		cleanup()
		return runner.Invocation{}, &SetupError{Err: fmt.Errorf("fake: record temp folder: %w", err)}
	}
	return runner.Invocation{
		Binary:  script,
		Args:    []string{state},
		Stdin:   req.Prompt,
		Dir:     req.Dir,
		Cleanup: cleanup,
	}, nil
}

// Parse returns the trimmed output as the final message.
func (fakeSleeper) Parse(stdout []byte) (Result, error) {
	return Result{Final: strings.TrimSpace(string(stdout))}, nil
}
