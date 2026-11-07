package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/runner"
)

var agyEfforts = []string{"low", "medium", "high", "xhigh", "max"}

type agy struct {
	promptOK bool
}

// agyMessage is the --print value. %s is the prompt file path.
const agyMessage = "Read the file %s and follow the instructions in it."

// NewAgy returns the agy adapter. Its prompt input is verified; its
// read-only setup is not, so the cli refuses read-only runs.
func NewAgy() Adapter { return newAgy(true) }

// newAgy lets tests mark prompt input as verified.
func newAgy(promptOK bool) *agy { return &agy{promptOK: promptOK} }

func (a *agy) Name() string           { return "agy" }
func (a *agy) Efforts() []string      { return slices.Clone(agyEfforts) }
func (a *agy) ReadOnly() Verification { return Verification{} }
func (a *agy) Prompt() Verification   { return Verification{OK: a.promptOK} }
func (a *agy) promptInFile() bool     { return true }

// Build writes the prompt to a private temp file and passes --print a fixed
// message naming that file. The prompt never goes in the argv. Cleanup
// removes the temp folder.
func (a *agy) Build(req Request, tc config.ToolConfig) (runner.Invocation, error) {
	if !a.Prompt().OK {
		return runner.Invocation{}, Refuse("%s prompt input not verified", a.Name())
	}
	if err := CheckEffort(a, req.Effort); err != nil {
		return runner.Invocation{}, err
	}
	bin, err := config.BinaryFor(a.Name(), tc)
	if err != nil {
		return runner.Invocation{}, &SetupError{Err: err}
	}
	file, cleanup, err := writePromptFile(a.Name(), req.Prompt)
	if err != nil {
		return runner.Invocation{}, err
	}
	model := ResolveModel(req.Model, tc)
	args := []string{"--print", fmt.Sprintf(agyMessage, file), "--output-format", "json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.Write {
		args = append(args, "--mode", "accept-edits")
	} else {
		// Read-only candidate, untested.
		args = append(args, "--sandbox", "--mode", "plan")
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	return runner.Invocation{Binary: bin, Args: args, Dir: req.Dir, Cleanup: cleanup}, nil
}

// Parse reads agy --output-format json output: one JSON object. Final is
// "response". Input tokens are usage.input_tokens plus usage.cache_read_tokens;
// output tokens are usage.output_tokens. A "status" other than SUCCESS gives
// an empty Final and an error without the response text. Empty output gives
// an empty Result and no error.
func (a *agy) Parse(stdout []byte) (Result, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return Result{}, nil
	}
	var out struct {
		Status   string `json:"status"`
		Response string `json:"response"`
		Usage    struct {
			InputTokens     int `json:"input_tokens"`
			CacheReadTokens int `json:"cache_read_tokens"`
			OutputTokens    int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return Result{}, errors.New("agy: output is not a JSON object")
	}
	res := Result{
		InputTokens:  out.Usage.InputTokens + out.Usage.CacheReadTokens,
		OutputTokens: out.Usage.OutputTokens,
	}
	if out.Status != "SUCCESS" {
		return res, errors.New("agy did not report success")
	}
	res.Final = out.Response
	return res, nil
}
