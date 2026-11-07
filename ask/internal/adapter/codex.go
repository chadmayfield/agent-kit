package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

// Codex runs `codex exec`. The prompt goes to stdin through the `-`
// argument, so it never appears on the command line.
type Codex struct{}

// Name returns "codex".
func (Codex) Name() string { return "codex" }

// Efforts returns nil: ask passes no effort setting to codex.
func (Codex) Efforts() []string { return nil }

// ReadOnly reports the read-only setup as untested.
func (Codex) ReadOnly() Verification { return Verification{OK: false} }

// Prompt reports stdin input as tested: codex reads the prompt from stdin
// when the prompt argument is `-`.
func (Codex) Prompt() Verification { return Verification{OK: true} }

// Build returns the codex command for req. Read-only runs skip the user
// config and rules files and use the read-only sandbox; write runs use the
// workspace-write sandbox. Build never reads secrets and leaves Env empty.
func (c Codex) Build(req Request, tc config.ToolConfig) (runner.Invocation, error) {
	if err := CheckEffort(c, req.Effort); err != nil {
		return runner.Invocation{}, err
	}
	if req.Model == "" {
		return runner.Invocation{}, Refuse("codex: no model given")
	}
	if req.Dir == "" {
		return runner.Invocation{}, Refuse("codex: no working folder given")
	}
	bin, err := config.BinaryFor("codex", tc)
	if err != nil {
		return runner.Invocation{}, &SetupError{Err: err}
	}
	model := ResolveModel(req.Model, tc)

	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check"}
	sandbox := "workspace-write"
	if !req.Write {
		args = append(args, "--ignore-user-config", "--ignore-rules")
		sandbox = "read-only"
	}
	args = append(args, "-m", model, "-C", req.Dir, "-s", sandbox, "-")

	return runner.Invocation{
		Binary: bin,
		Args:   args,
		Stdin:  req.Prompt,
		Dir:    req.Dir,
	}, nil
}

// codexEvent is the part of a `codex exec --json` event that ask reads.
type codexEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Parse reads the JSONL event stream. Final is the text of the last
// agent_message item; tokens are summed over every turn.completed event.
// Unknown event types are skipped, and so are lines that do not start with
// `{`, such as warnings. A `{` line that is not valid JSON is an error.
// Output with no events gives an empty Result and no error.
func (Codex) Parse(stdout []byte) (Result, error) {
	var res Result
	for i, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return Result{}, fmt.Errorf("codex: output line %d is not valid JSON", i+1)
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				res.Final = ev.Item.Text
			}
		case "turn.completed":
			if ev.Usage != nil {
				res.InputTokens += ev.Usage.InputTokens
				res.OutputTokens += ev.Usage.OutputTokens
			}
		}
	}
	return res, nil
}
