package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

type claude struct{}

// NewClaude returns the adapter for the claude CLI.
func NewClaude() Adapter { return claude{} }

func (claude) Name() string { return "claude" }

func (claude) Efforts() []string { return slices.Clone(claudeEfforts) }

// claudeTestedVersion is the trimmed `claude --version` output of the
// install where the read-only setup passed its test. Any other version is
// refused until it is tested again.
const claudeTestedVersion = "2.1.319 (Claude Code)"

// ReadOnly is verified for claudeTestedVersion only. The read-only argv
// needs both --safe-mode and --restricted. See docs/read-only-tests.md.
func (claude) ReadOnly() Verification {
	return Verification{OK: true, TestedVersion: claudeTestedVersion}
}

// Prompt is verified: with -p, claude reads the prompt from stdin.
func (claude) Prompt() Verification { return Verification{OK: true} }

// Build returns the command line for one run. The prompt goes on stdin.
// Build never adds --bare; the cli layer puts any API key in the environment.
func (c claude) Build(req Request, tc config.ToolConfig) (runner.Invocation, error) {
	if err := CheckEffort(c, req.Effort); err != nil {
		return runner.Invocation{}, err
	}
	bin, err := config.BinaryFor("claude", tc)
	if err != nil {
		return runner.Invocation{}, &SetupError{Err: err}
	}

	model := ResolveModel(req.Model, tc)

	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.Write {
		args = append(args, "--permission-mode", "acceptEdits")
	} else {
		args = append(args,
			"--safe-mode", "--restricted", "--strict-mcp-config", "--no-chrome",
			"--tools", "Read,Grep,Glob", "--no-session-persistence",
		)
	}

	return runner.Invocation{
		Binary: bin,
		Args:   args,
		Stdin:  req.Prompt,
		Dir:    req.Dir,
	}, nil
}

type claudeEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Model   string `json:"model"`
	Usage   struct {
		InputTokens              int `json:"input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

// claudeSubtypeErrors maps the error subtypes claude is known to send to
// fixed messages.
var claudeSubtypeErrors = map[string]string{
	"error_max_turns":        "claude stopped at its turn limit",
	"error_during_execution": "claude failed during execution",
}

// claudeSubtypeMessage returns a fixed message for subtype. An unknown
// subtype gives a generic message, never the subtype text.
func claudeSubtypeMessage(subtype string) string {
	if m, ok := claudeSubtypeErrors[subtype]; ok {
		return m
	}
	return "claude reported an error"
}

// Parse reads stream-json output. Final and token counts come from the last
// result event, and Model from the system init event. No result event gives
// an empty Final. Lines that do not start with "{" are skipped. An error
// result gives an empty Final and an error without the result text.
func (claude) Parse(stdout []byte) (Result, error) {
	var (
		res      Result
		model    string
		found    bool
		runErr   error
		firstBad error
	)
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if firstBad == nil {
				firstBad = fmt.Errorf("claude output line %d is not valid JSON: %w", n, err)
			}
			continue
		}
		if ev.Type == "system" && ev.Subtype == "init" {
			model = ev.Model
			continue
		}
		if ev.Type != "result" {
			continue
		}
		found = true
		res = Result{
			InputTokens:  ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens + ev.Usage.CacheCreationInputTokens,
			OutputTokens: ev.Usage.OutputTokens,
		}
		runErr = nil
		// An empty subtype counts as success.
		okSubtype := ev.Subtype == "success" || ev.Subtype == ""
		switch {
		case !okSubtype:
			// Keep the result text and subtype out of the error; they may
			// hold prompt content or a secret.
			runErr = errors.New(claudeSubtypeMessage(ev.Subtype))
		case ev.IsError:
			runErr = errors.New("claude reported an error")
		default:
			res.Final = ev.Result
		}
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("read claude output: %w", err)
	}
	if !found && firstBad != nil {
		return Result{}, firstBad
	}
	res.Model = model
	return res, runErr
}
