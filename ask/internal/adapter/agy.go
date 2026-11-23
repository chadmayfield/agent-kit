package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

var agyEfforts = []string{"low", "medium", "high", "xhigh", "max"}

type agy struct {
	promptOK bool
}

// agyMessage is the --print value. %s is the prompt file path.
const agyMessage = "Read the file %s and follow the instructions in it."

// agyPlanNote goes in front of the prompt of a read-only run. Read-only
// runs use --mode plan, which adds a /plan instruction to write a plan and
// wait for approval. A --print run cannot approve anything, and on
// 2026-11-22 agy 1.5.14 often ended with a plan instead of an answer, even
// when the prompt said not to use tools. Naming the /plan instruction and
// saying the answer is the plan fixed it in a live run. Shell commands
// outside agy's allow-list are soft-denied, which ends a --print run with no
// answer, so the note also rules them out.
const agyPlanNote = `IMPORTANT FOR THIS RUN, read before anything else:
1. This is a single, non-interactive run. No one can read a plan, answer a question or approve anything, so a run that ends with a plan or a request for approval has failed.
2. This session starts with a /plan instruction. For this task, your answer itself is the plan. Do not create implementation_plan.md, a walkthrough or any other artifact, and do not ask for approval. Write your complete answer as your final response text.
3. Apart from this file, do not run any shell command or use any other tool. Such calls are refused in this session and end the run with no answer. Work only from the text in this file, which includes any files attached to the request.`

// agyConversationID matches the conversation_id agy prints, a UUID. Only
// a match is used in a path.
var agyConversationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// agyPlanPath is where agy keeps the plan a conversation wrote. agy 1.5.9
// and 1.5.14 both wrote plans there.
func agyPlanPath(home, id string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain", id, "implementation_plan.md")
}

// maxAgyPlanGuardOutput is the most stdout the plan guard keeps. agy prints
// one JSON object, far smaller than this. Tests lower it.
var maxAgyPlanGuardOutput = 16 << 20

// agyPlanGuard checks a read-only agy run after it exits 0. When agy wrote
// a plan in that conversation, End returns a *NoAnswerError: the run
// stopped to wait for approval. Output it cannot read, or a run with no
// conversation_id, passes, and Parse reports any other problem.
type agyPlanGuard struct {
	home    string
	out     []byte
	tooLong bool
}

func (g *agyPlanGuard) Line(line []byte) error {
	if g.tooLong {
		return nil
	}
	if len(g.out)+len(line)+1 > maxAgyPlanGuardOutput {
		g.tooLong, g.out = true, nil
		return nil
	}
	g.out = append(g.out, line...)
	g.out = append(g.out, '\n')
	return nil
}

func (g *agyPlanGuard) End() error {
	if g.tooLong {
		return nil
	}
	var out struct {
		ConversationID string `json:"conversation_id"`
	}
	if json.Unmarshal(bytes.TrimSpace(g.out), &out) != nil || !agyConversationID.MatchString(out.ConversationID) {
		return nil
	}
	plan := agyPlanPath(g.home, out.ConversationID)
	if _, err := os.Lstat(plan); err != nil {
		return nil
	}
	return &NoAnswerError{Msg: "agy wrote a plan instead of an answer: " + plan}
}

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
	text := req.Prompt
	if !req.Write {
		text = agyPlanNote + "\n\n" + req.Prompt
	}
	file, cleanup, err := writePromptFile(a.Name(), text)
	if err != nil {
		return runner.Invocation{}, err
	}
	model := ResolveModel(req.Model, tc)
	args := []string{"--print", fmt.Sprintf(agyMessage, file), "--output-format", "json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	var guard runner.Guard
	if req.Write {
		args = append(args, "--mode", "accept-edits")
	} else {
		// Read-only candidate, untested. Plan mode can end with a plan
		// instead of an answer; the note and the guard handle that.
		args = append(args, "--sandbox", "--mode", "plan")
		if home, err := os.UserHomeDir(); err == nil {
			guard = &agyPlanGuard{home: home}
		}
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	return runner.Invocation{Binary: bin, Args: args, Dir: req.Dir, Cleanup: cleanup, Guard: guard}, nil
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
