package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/testenv"
)

const claudeTestBinary = "/opt/example/bin/claude"

func buildClaude(t *testing.T, req Request, tc config.ToolConfig) []string {
	t.Helper()
	if tc.Binary == "" {
		tc.Binary = claudeTestBinary
	}
	inv, err := NewClaude().Build(req, tc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return inv.Args
}

func TestClaudeInvocationHasNoPromptInArgs(t *testing.T) {
	const prompt = "ZEBRA-PROMPT-7f3a review the parser"
	for _, write := range []bool{false, true} {
		req := Request{Model: "example-model", Prompt: prompt, Effort: "high", Write: write}
		inv, err := NewClaude().Build(req, config.ToolConfig{Binary: claudeTestBinary})
		if err != nil {
			t.Fatalf("Build (write=%v): %v", write, err)
		}
		for i, a := range inv.Args {
			if strings.Contains(a, "ZEBRA-PROMPT-7f3a") {
				t.Errorf("write=%v: argv[%d] = %q holds the prompt", write, i, a)
			}
		}
		if inv.Stdin != prompt {
			t.Errorf("write=%v: Stdin = %q, want the prompt", write, inv.Stdin)
		}
		if !slices.Contains(inv.Args, "-p") {
			t.Errorf("write=%v: argv %q lacks -p", write, inv.Args)
		}
	}
	if v := NewClaude().Prompt(); !v.OK {
		t.Error("Prompt() is not OK; stdin with -p is the tested path")
	}
}

func TestClaudeReadOnlyFlags(t *testing.T) {
	got := buildClaude(t, Request{Model: "example-model", Prompt: "x"}, config.ToolConfig{})
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "example-model",
		"--safe-mode", "--restricted", "--strict-mcp-config", "--no-chrome",
		"--tools", "Read,Grep,Glob", "--no-session-persistence",
	}
	if !slices.Equal(got, want) {
		t.Errorf("read-only argv =\n %q\nwant\n %q", got, want)
	}
	if slices.Contains(got, "--permission-mode") {
		t.Error("read-only argv sets a permission mode")
	}

	write := buildClaude(t, Request{Model: "example-model", Prompt: "x", Write: true}, config.ToolConfig{})
	wantWrite := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "example-model",
		"--permission-mode", "acceptEdits",
	}
	if !slices.Equal(write, wantWrite) {
		t.Errorf("write argv =\n %q\nwant\n %q", write, wantWrite)
	}

	eff := buildClaude(t, Request{Model: "example-model", Prompt: "x", Effort: "xhigh"}, config.ToolConfig{})
	if i := slices.Index(eff, "--effort"); i < 0 || i+1 >= len(eff) || eff[i+1] != "xhigh" {
		t.Errorf("argv %q lacks --effort xhigh", eff)
	}
	if noEff := buildClaude(t, Request{Prompt: "x"}, config.ToolConfig{}); slices.Contains(noEff, "--effort") {
		t.Errorf("argv %q has --effort with no effort set", noEff)
	}

	_, err := NewClaude().Build(Request{Prompt: "x", Effort: "extreme"}, config.ToolConfig{Binary: claudeTestBinary})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Errorf("unsupported effort: err = %v, want *RefusedError", err)
	}

	if want := []string{"low", "medium", "high", "xhigh", "max"}; !slices.Equal(NewClaude().Efforts(), want) {
		t.Errorf("Efforts() = %q, want %q", NewClaude().Efforts(), want)
	}
	if NewClaude().Name() != "claude" {
		t.Errorf("Name() = %q", NewClaude().Name())
	}
}

func TestClaudeReadOnlyVerifiedForTestedVersion(t *testing.T) {
	ro := NewClaude().ReadOnly()
	if !ro.OK {
		t.Fatal("ReadOnly().OK is false; the read-only setup passed its test")
	}
	if want := "2.1.319 (Claude Code)"; ro.TestedVersion != want {
		t.Errorf("TestedVersion = %q, want %q", ro.TestedVersion, want)
	}
	// The read-only argv needs both flags.
	args := buildClaude(t, Request{Prompt: "x"}, config.ToolConfig{})
	for _, flag := range []string{"--safe-mode", "--restricted"} {
		if !slices.Contains(args, flag) {
			t.Errorf("read-only argv %q lacks %s", args, flag)
		}
	}
}

func TestClaudeNeverBare(t *testing.T) {
	modes := map[string]config.ToolConfig{
		"login":   {},
		"api_key": {Auth: "api_key", Secrets: []config.SecretRef{{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "env:EXAMPLE_API_KEY"}}},
	}
	for name, tc := range modes {
		for _, write := range []bool{false, true} {
			args := buildClaude(t, Request{Model: "example-model", Prompt: "x", Write: write}, tc)
			for _, a := range args {
				if a == "--bare" || strings.HasPrefix(a, "--bare=") {
					t.Errorf("%s write=%v: argv %q has --bare", name, write, args)
				}
				if strings.Contains(a, "EXAMPLE_API_KEY") {
					t.Errorf("%s write=%v: argv %q names the key variable", name, write, args)
				}
			}
		}
	}
	login := buildClaude(t, Request{Model: "m", Prompt: "x"}, modes["login"])
	key := buildClaude(t, Request{Model: "m", Prompt: "x"}, modes["api_key"])
	if !slices.Equal(login, key) {
		t.Errorf("api_key argv %q differs from login argv %q", key, login)
	}
}

func TestClaudeUsesConfiguredBinary(t *testing.T) {
	src := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, src)

	set := config.ToolConfig{Binary: "/opt/example/bin/claude-custom"}
	inv, err := NewClaude().Build(Request{Prompt: "x", Dir: "/tmp/example"}, set)
	if err != nil {
		t.Fatalf("Build with binary set: %v", err)
	}
	if inv.Binary != set.Binary {
		t.Errorf("Binary = %q, want %q", inv.Binary, set.Binary)
	}
	if inv.Dir != "/tmp/example" {
		t.Errorf("Dir = %q, want /tmp/example", inv.Dir)
	}

	inv, err = NewClaude().Build(Request{Prompt: "x"}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build with no binary set: %v", err)
	}
	if want := filepath.Join(home, "bin", "claude"); inv.Binary != want {
		t.Errorf("Binary = %q, want %q", inv.Binary, want)
	}
}

func TestClaudeParseFinal(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "claude-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewClaude().Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Final != "The file declares package main." {
		t.Errorf("Final = %q", res.Final)
	}
	// Input tokens count fresh, cache-read and cache-write tokens: 42+100+5.
	if res.InputTokens != 147 || res.OutputTokens != 17 {
		t.Errorf("tokens = %d in, %d out, want 147 in, 17 out", res.InputTokens, res.OutputTokens)
	}

	// Stray non-JSON lines, such as hook or warning output, are skipped.
	noisy := "warning: example hook output\n\n" + string(data) + "trailing note\n"
	res, err = NewClaude().Parse([]byte(noisy))
	if err != nil || res.Final != "The file declares package main." {
		t.Errorf("Parse with stray lines = %+v, %v", res, err)
	}

	// A broken JSON line does not matter once a result event is found.
	broken := "{\"type\":\"assistant\",\"message\":\n" + string(data)
	res, err = NewClaude().Parse([]byte(broken))
	if err != nil || res.Final != "The file declares package main." {
		t.Errorf("Parse with a broken line and a result = %+v, %v", res, err)
	}

	// A run that ends before the result event has no final text, and that
	// is not an error.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	cut := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	res, err = NewClaude().Parse([]byte(cut))
	if err != nil {
		t.Fatalf("Parse without result event: %v", err)
	}
	if res.Final != "" {
		t.Errorf("Final without result event = %q, want empty", res.Final)
	}

	if _, err := NewClaude().Parse([]byte("{\"type\":\"system\"}\n{not json\n")); err == nil {
		t.Error("Parse accepted invalid JSON with no result event")
	}
}

func TestClaudeParseErrorResult(t *testing.T) {
	const errText = "EXAMPLE-ERROR-TEXT"
	cases := map[string]string{
		"is_error":            `{"type":"result","subtype":"success","is_error":true,"result":"` + errText + `","usage":{"input_tokens":7,"output_tokens":1}}`,
		"is_error_no_subtype": `{"type":"result","is_error":true,"result":"` + errText + `","usage":{"input_tokens":7,"output_tokens":1}}`,
		"subtype":             `{"type":"result","subtype":"error_during_execution","is_error":false,"result":"` + errText + `","usage":{"input_tokens":7,"output_tokens":1}}`,
		"max_turns":           `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"` + errText + `","usage":{"input_tokens":7,"output_tokens":1}}`,
		"unknown_subtype":     `{"type":"result","subtype":"` + errText + `","is_error":true,"result":"x","usage":{"input_tokens":7,"output_tokens":1}}`,
	}
	wantErr := map[string]string{
		"is_error":            "claude reported an error",
		"is_error_no_subtype": "claude reported an error",
		"subtype":             "claude failed during execution",
		"max_turns":           "claude stopped at its turn limit",
		"unknown_subtype":     "claude reported an error",
	}
	for name, line := range cases {
		stream := `{"type":"system","subtype":"init","cwd":"/work/project","tools":[],"mcp_servers":[]}` + "\n" + line + "\n"
		res, err := NewClaude().Parse([]byte(stream))
		if res.Final != "" {
			t.Errorf("%s: Final = %q, want empty", name, res.Final)
		}
		if err == nil || err.Error() != wantErr[name] {
			t.Errorf("%s: err = %v, want %q", name, err, wantErr[name])
		} else if strings.Contains(err.Error(), errText) {
			t.Errorf("%s: err = %v holds the result text", name, err)
		}
		if res.InputTokens != 7 || res.OutputTokens != 1 {
			t.Errorf("%s: tokens = %d in, %d out, want 7 in, 1 out", name, res.InputTokens, res.OutputTokens)
		}
	}

	// A result event with no subtype and is_error false counts as success.
	ok := `{"type":"result","is_error":false,"result":"done","usage":{"input_tokens":3,"output_tokens":2}}` + "\n"
	res, err := NewClaude().Parse([]byte(ok))
	if err != nil || res.Final != "done" {
		t.Errorf("no subtype, is_error false: %+v, %v, want Final \"done\" and no error", res, err)
	}
}

func TestClaudeMapsModelNames(t *testing.T) {
	tc := config.ToolConfig{Models: map[string]string{"fast": "example-model-fast", "blank": ""}}
	cases := map[string]string{
		"fast":          "example-model-fast",
		"example-other": "example-other",
		"blank":         "blank",
	}
	for in, want := range cases {
		args := buildClaude(t, Request{Model: in, Prompt: "x"}, tc)
		i := slices.Index(args, "--model")
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Errorf("model %q: argv %q, want --model %q", in, args, want)
		}
	}
}

func TestClaudeParseReportsModel(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "claude-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewClaude().Parse(data)
	if err != nil || res.Model != "example-model" {
		t.Fatalf("Parse = %+v, %v, want Model example-model", res, err)
	}
}

func TestResolveModel(t *testing.T) {
	tc := config.ToolConfig{Models: map[string]string{"short": "example/model", "blank": ""}}
	for in, want := range map[string]string{"short": "example/model", "blank": "blank", "other": "other", "": ""} {
		if got := ResolveModel(in, tc); got != want {
			t.Errorf("ResolveModel(%q) = %q, want %q", in, got, want)
		}
	}
}
