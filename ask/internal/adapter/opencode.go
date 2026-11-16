package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

type opencode struct {
	promptOK bool
}

// opencodeMessage is the fixed message passed with the attached prompt file.
const opencodeMessage = "Follow the instructions in the attached file."

// NewOpencode returns the opencode adapter. Its prompt input is verified;
// its read-only setup is not, so read-only Builds are refused.
func NewOpencode() Adapter { return newOpencode(true) }

// newOpencode lets tests mark prompt input as verified.
func newOpencode(promptOK bool) *opencode { return &opencode{promptOK: promptOK} }

func (o *opencode) Name() string           { return "opencode" }
func (o *opencode) Efforts() []string      { return nil }
func (o *opencode) ReadOnly() Verification { return Verification{} }
func (o *opencode) Prompt() Verification   { return Verification{OK: o.promptOK} }
func (o *opencode) promptInFile() bool     { return true }

// Build writes the prompt to a private temp file and attaches it with -f,
// followed by a fixed message. The prompt never goes in the argv. Cleanup
// removes the temp folder.
func (o *opencode) Build(req Request, tc config.ToolConfig) (runner.Invocation, error) {
	if !o.Prompt().OK {
		return runner.Invocation{}, Refuse("%s prompt input not verified", o.Name())
	}
	if !req.Write {
		return runner.Invocation{}, Refuse("%s has no read-only setup", o.Name())
	}
	if err := CheckEffort(o, req.Effort); err != nil {
		return runner.Invocation{}, err
	}
	bin, err := config.BinaryFor(o.Name(), tc)
	if err != nil {
		return runner.Invocation{}, &SetupError{Err: err}
	}

	file, cleanup, err := writePromptFile(o.Name(), req.Prompt)
	if err != nil {
		return runner.Invocation{}, err
	}

	model := ResolveModel(req.Model, tc)
	args := []string{"run", "--standalone", "--format", "json"}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, "--auto", "-f", file, opencodeMessage)
	return runner.Invocation{Binary: bin, Args: args, Dir: req.Dir, Cleanup: cleanup}, nil
}

// Parse reads opencode run --format json output: one JSON event per line.
// The final message is part.text of the last "text" event that sets it.
// Tokens are summed from part.tokens on any event that carries them.
// Other events are ignored. Output with no text event gives an empty Final
// and no error. Non-empty output where no line is JSON is an error.
func (o *opencode) Parse(stdout []byte) (Result, error) {
	var res Result
	var lines, parsed int
	var toolErr *ToolError
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lines++
		var ev struct {
			Type  string              `json:"type"`
			Error *opencodeErrorEvent `json:"error"`
			Part  *struct {
				Text   string `json:"text"`
				Tokens *struct {
					Input  int `json:"input"`
					Output int `json:"output"`
				} `json:"tokens"`
			} `json:"part"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		parsed++
		if ev.Type == "error" && toolErr == nil {
			toolErr = opencodeError(ev.Error)
		}
		if ev.Part == nil {
			continue
		}
		if ev.Part.Tokens != nil {
			res.InputTokens += ev.Part.Tokens.Input
			res.OutputTokens += ev.Part.Tokens.Output
		}
		if ev.Type == "text" && ev.Part.Text != "" {
			res.Final = ev.Part.Text
		}
	}
	if lines > 0 && parsed == 0 {
		return Result{}, errors.New("opencode: output is not JSON lines")
	}
	// An error opencode recovered from, such as a retried rate limit, does
	// not fail a run that still answered.
	if toolErr != nil && res.Final == "" {
		return res, toolErr
	}
	return res, nil
}

// opencodeErrorEvent is the error field of an opencode "error" event.
type opencodeErrorEvent struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
}

// opencodeError describes an opencode error event from its type and status
// only. The message text may hold anything, so it is never shown.
func opencodeError(e *opencodeErrorEvent) *ToolError {
	if e == nil || !plainName(e.Type) {
		return &ToolError{Msg: "opencode reported an error"}
	}
	msg := "opencode reported a " + e.Type + " error"
	if e.Status >= 100 && e.Status <= 599 {
		msg += fmt.Sprintf(" (status %d)", e.Status)
	}
	return &ToolError{Msg: msg}
}

// plainName reports whether s is a short name made of letters, digits,
// dots, underscores and dashes, such as "provider.auth".
func plainName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
