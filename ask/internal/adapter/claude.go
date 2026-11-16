package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

type claude struct{}

// NewClaude returns the adapter for the claude CLI.
func NewClaude() Adapter { return claude{} }

func (claude) Name() string { return "claude" }

func (claude) Efforts() []string { return slices.Clone(claudeEfforts) }

// claudeLastTested is the trimmed `claude --version` output of the install
// where the read-only setup last passed its full test. Every version with
// the same major number runs read-only, and claudeGuard checks each run.
const claudeLastTested = "2.1.325 (Claude Code)"

// ReadOnly is verified for the major version of claudeLastTested. The
// read-only argv needs both --safe-mode and --restricted. See
// docs/read-only-tests.md.
func (claude) ReadOnly() Verification {
	return Verification{OK: true, LastTested: claudeLastTested}
}

// claudeReadOnlyTools are the only tools a read-only session may load.
var claudeReadOnlyTools = []string{"Read", "Grep", "Glob"}

// claudeGuard checks a read-only claude run as it streams. It stops the
// run at once on a hook event, on an init event that lists a tool other
// than claudeReadOnlyTools or any MCP server, and on any event before init
// other than a result. A non-blank line that is not a JSON object cannot
// be checked: it fails a run that exits 0 (in End), and leaves a failing
// run as an ordinary failure. Errors are fixed text and never quote the
// stream.
type claudeGuard struct {
	seenInit   bool
	unreadable bool
}

type claudeGuardEvent struct {
	Type       string             `json:"type"`
	Subtype    string             `json:"subtype"`
	Tools      *[]string          `json:"tools"`
	MCPServers *[]json.RawMessage `json:"mcp_servers"`
}

func (g *claudeGuard) Line(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	if line[0] != '{' || !json.Valid(line) {
		g.unreadable = true
		return nil
	}
	// A JSON event in a shape the guard does not expect, such as a tools
	// value that is not a list, stops the run at once.
	var ev claudeGuardEvent
	if json.Unmarshal(line, &ev) != nil {
		return errors.New("claude sent an event in an unexpected shape")
	}
	if isHookEvent(ev.Type) || isHookEvent(ev.Subtype) {
		return errors.New("a hook ran during a read-only claude run")
	}
	isInit := ev.Type == "system" && ev.Subtype == "init"
	if !g.seenInit && !isInit {
		// A result before init is claude reporting that it failed to
		// start; it ends the run and acts on nothing.
		if ev.Type == "result" {
			return nil
		}
		return errors.New("claude did not start with an init event")
	}
	if !isInit {
		return nil
	}
	g.seenInit = true
	if ev.Tools == nil || ev.MCPServers == nil {
		return errors.New("claude's init event has no tools or mcp_servers list")
	}
	for _, t := range *ev.Tools {
		if !slices.Contains(claudeReadOnlyTools, t) {
			return errors.New("claude loaded a tool other than Read, Grep and Glob")
		}
	}
	if len(*ev.MCPServers) != 0 {
		return errors.New("claude started an MCP server")
	}
	return nil
}

// End runs only after claude exited 0.
func (g *claudeGuard) End() error {
	if !g.seenInit {
		return errors.New("claude sent no init event")
	}
	if g.unreadable {
		return errors.New("claude sent an event the read-only guard cannot read")
	}
	return nil
}

// isHookEvent matches any event type or subtype that names a hook, such as
// hook_started or hook_response.
func isHookEvent(s string) bool {
	return strings.Contains(strings.ToLower(s), "hook")
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
			"--tools", "Read,Grep,Glob", "--include-hook-events",
		)
	}

	inv := runner.Invocation{
		Binary: bin,
		Args:   args,
		Stdin:  req.Prompt,
		Dir:    req.Dir,
	}
	if !req.Write {
		inv.Guard = &claudeGuard{}
	}
	return inv, nil
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
