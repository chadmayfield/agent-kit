package adapter

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/runner"
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
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lines++
		var ev struct {
			Type string `json:"type"`
			Part *struct {
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
	return res, nil
}
