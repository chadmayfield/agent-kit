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

const distinctPrompt = "summarize the build at placeholder.invalid zq7x"

// fakeCodex writes a fake codex script and isolates the test environment
// so BinaryFor finds only that fake on PATH.
func fakeCodex(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, src)
	return filepath.Join(home, "bin", "codex")
}

func TestCodexInvocationHasNoPromptInArgs(t *testing.T) {
	want := fakeCodex(t)
	for _, write := range []bool{false, true} {
		req := Request{Model: "gpt-x", Prompt: distinctPrompt, Dir: "/tmp/work", Write: write}
		inv, err := Codex{}.Build(req, config.ToolConfig{})
		if err != nil {
			t.Fatalf("Build(write=%v): %v", write, err)
		}
		if inv.Binary != want {
			t.Errorf("Binary = %q, want %q", inv.Binary, want)
		}
		for _, a := range append([]string{inv.Binary}, inv.Args...) {
			if strings.Contains(a, "zq7x") || strings.Contains(a, "placeholder.invalid") {
				t.Errorf("argv element %q holds prompt text", a)
			}
		}
		if inv.Stdin != distinctPrompt {
			t.Errorf("Stdin = %q, want the prompt", inv.Stdin)
		}
		if inv.Args[len(inv.Args)-1] != "-" {
			t.Errorf("last arg = %q, want -", inv.Args[len(inv.Args)-1])
		}
		if len(inv.Env) != 0 {
			t.Errorf("Env = %v, want empty", inv.Env)
		}
	}
	if !(Codex{}).Prompt().OK {
		t.Error("Prompt().OK = false, want true")
	}
	if (Codex{}).ReadOnly().OK {
		t.Error("ReadOnly().OK = true, want false until tested")
	}
}

func TestCodexReadOnlyFlags(t *testing.T) {
	fakeCodex(t)
	tc := config.ToolConfig{Binary: "/opt/example/codex", Models: map[string]string{"m": "gpt-long-name"}}
	inv, err := Codex{}.Build(Request{Model: "m", Prompt: "hi", Dir: "/tmp/work"}, tc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check",
		"--ignore-user-config", "--ignore-rules",
		"-m", "gpt-long-name", "-C", "/tmp/work", "-s", "read-only", "-"}
	if !slices.Equal(inv.Args, want) {
		t.Errorf("Args = %q\nwant   %q", inv.Args, want)
	}
	if inv.Binary != "/opt/example/codex" {
		t.Errorf("Binary = %q, want the configured binary", inv.Binary)
	}
	if inv.Dir != "/tmp/work" {
		t.Errorf("Dir = %q, want /tmp/work", inv.Dir)
	}
}

func TestCodexWriteFlags(t *testing.T) {
	fakeCodex(t)
	inv, err := Codex{}.Build(Request{Model: "gpt-x", Prompt: "hi", Dir: "/tmp/work", Write: true}, config.ToolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check",
		"-m", "gpt-x", "-C", "/tmp/work", "-s", "workspace-write", "-"}
	if !slices.Equal(inv.Args, want) {
		t.Errorf("Args = %q\nwant   %q", inv.Args, want)
	}
}

func TestCodexParseFinal(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "codex-exec.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Codex{}.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Final != "The answer is 42." {
		t.Errorf("Final = %q", res.Final)
	}
	if res.InputTokens != 1500 || res.OutputTokens != 57 {
		t.Errorf("tokens = %d in, %d out; want 1500, 57", res.InputTokens, res.OutputTokens)
	}

	empty, err := Codex{}.Parse([]byte(`{"type":"thread.started"}` + "\n"))
	if err != nil || empty.Final != "" {
		t.Errorf("no agent_message: got %+v, %v; want empty final, nil", empty, err)
	}

	if _, err := (Codex{}).Parse([]byte("{\"type\":\"turn.started\"}\n{not json\n")); err == nil {
		t.Error("invalid JSON line: want an error")
	}
	truncated := append(append([]byte{}, data...), []byte(`{"type":"item.completed","item":{"type":"agent_mes`)...)
	if _, err := (Codex{}).Parse(truncated); err == nil {
		t.Error("truncated last event after an agent_message: want an error")
	}
	for _, in := range []string{"", "WARNING: something\n"} {
		got, err := Codex{}.Parse([]byte(in))
		if err != nil || got != (Result{}) {
			t.Errorf("no events in %q: got %+v, %v; want empty Result, nil", in, got, err)
		}
	}

	withWarning := append([]byte("WARNING: proceeding without a sandbox config\n"), data...)
	res, err = Codex{}.Parse(withWarning)
	if err != nil {
		t.Fatalf("stray lines before events: %v", err)
	}
	if res.Final != "The answer is 42." {
		t.Errorf("stray lines: Final = %q", res.Final)
	}
}

func TestCodexBuildRefusals(t *testing.T) {
	fakeCodex(t)
	var re *RefusedError
	_, err := Codex{}.Build(Request{Prompt: "hi", Dir: "/tmp/work"}, config.ToolConfig{})
	if !errors.As(err, &re) || re.Msg != "codex: no model given" {
		t.Errorf("no model: %v, want refusal", err)
	}
	_, err = Codex{}.Build(Request{Model: "gpt-x", Prompt: "hi"}, config.ToolConfig{})
	if !errors.As(err, &re) || re.Msg != "codex: no working folder given" {
		t.Errorf("no dir: %v, want refusal", err)
	}
}

func TestCodexMissingBinaryIsSetupError(t *testing.T) {
	testenv.Isolate(t)
	_, err := Codex{}.Build(Request{Model: "gpt-x", Prompt: "hi", Dir: "/tmp/work"}, config.ToolConfig{})
	var se *SetupError
	if !errors.As(err, &se) {
		t.Fatalf("missing binary: %v, want *SetupError", err)
	}
}

func TestDryRunEscapesControlChars(t *testing.T) {
	fakeCodex(t)
	tc := config.ToolConfig{
		Secrets: []config.SecretRef{{Name: "api\nkey", EnvVar: "OPENAI_API_KEY", Ref: "env:EXAMPLE_API_KEY"}},
	}
	env := []string{"OPENAI_API_KEY=api\nkey"}
	req := Request{Model: "gpt-x\nrm -rf x", Prompt: "hi", Dir: "/tmp/work"}
	out, err := DryRun(Codex{}, req, tc, env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\n\r") {
		t.Errorf("output splits lines: %q", out)
	}
	for _, want := range []string{`OPENAI_API_KEY=<'api\nkey'>`, `'gpt-x\nrm -rf x'`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	req.Model = "m\n$(touch x)"
	out, err = DryRun(Codex{}, req, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\n\r") {
		t.Errorf("output splits lines: %q", out)
	}
	if !strings.Contains(out, ` -m 'm\n$(touch x)' `) {
		t.Errorf("substitution not inside single quotes: %s", out)
	}
	if strings.Count(out, "$(") != 1 {
		t.Errorf("unexpected $( in output: %s", out)
	}

	for in, want := range map[string]string{
		"a\u0085b":      `'a\u0085b'`,
		"a\u2028b":      `'a\u2028b'`,
		"a\u2029b":      `'a\u2029b'`,
		"a\xffb":        `'a\xffb'`,
		"a\x1bb":        `'a\x1bb'`,
		"it's\n":        `'it'\''s\n'`,
		"back\\slash\n": `'back\\slash\n'`,
		"plain":         "plain",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestDryRunQuotesEnvAndKeyNames(t *testing.T) {
	fakeCodex(t)
	tc := config.ToolConfig{
		Secrets: []config.SecretRef{{Name: "api key$(x)", EnvVar: "EXAMPLE;KEY", Ref: "env:EXAMPLE_API_KEY"}},
		PassEnv: []string{"PASS`x`"},
	}
	env := []string{"EXAMPLE;KEY=api key$(x)", "PASS`x`=", "OTHER|NAME=v"}
	out, err := DryRun(Codex{}, Request{Model: "m", Prompt: "hi", Dir: "/tmp/work"}, tc, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`'EXAMPLE;KEY'=<'api key$(x)'>`, "'PASS`x`'=<pass_env>", `'OTHER|NAME'=<hidden>`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSetupErrorNilErr(t *testing.T) {
	if got := (&SetupError{}).Error(); got != "setup failed" {
		t.Errorf("Error() = %q", got)
	}
}

func TestDryRunShowsNamesNotValues(t *testing.T) {
	fakeCodex(t)
	tc := config.ToolConfig{
		Binary:  "/opt/example dir/codex",
		Secrets: []config.SecretRef{{Name: "api_key", EnvVar: "OPENAI_API_KEY", Ref: "env:EXAMPLE_API_KEY"}},
		PassEnv: []string{"LANG"},
	}
	env := []string{"OPENAI_API_KEY=api_key", "LANG=LANG", "EXTRA_TOKEN=sk-example-value-0001"}
	req := Request{Model: "gpt-x", Prompt: distinctPrompt, Dir: "/tmp/work"}
	out, err := DryRun(Codex{}, req, tc, env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("output is not one line: %q", out)
	}
	for _, want := range []string{
		"OPENAI_API_KEY=<api_key>",
		"LANG=<pass_env>",
		"EXTRA_TOKEN=<hidden>",
		"'/opt/example dir/codex' exec --json",
		"-s read-only -",
		"<prompt: 47 bytes>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"sk-example-value-0001", "zq7x", "EXAMPLE_API_KEY"} {
		if strings.Contains(out, bad) {
			t.Errorf("output holds %q:\n%s", bad, out)
		}
	}
}

func TestUnsupportedEffortExits2(t *testing.T) {
	fakeCodex(t)
	var re *RefusedError
	err := CheckEffort(Codex{}, "high")
	if !errors.As(err, &re) {
		t.Fatalf("CheckEffort = %v, want *RefusedError", err)
	}
	if re.Msg != "effort not supported by codex" {
		t.Errorf("Msg = %q", re.Msg)
	}
	if err := CheckEffort(Codex{}, ""); err != nil {
		t.Errorf("empty effort: %v", err)
	}
	_, err = Codex{}.Build(Request{Model: "gpt-x", Prompt: "hi", Dir: "/tmp/work", Effort: "high"}, config.ToolConfig{})
	if !errors.As(err, &re) {
		t.Errorf("Build with effort = %v, want *RefusedError", err)
	}
	if _, ok := Default()["codex"]; !ok {
		t.Error("Default() has no codex")
	}
}
