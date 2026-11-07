package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
	"github.com/chadmayfield/agent-kit/ask/internal/testenv"
	"github.com/spf13/cobra"
)

// stubAdapter is a do-nothing adapter for tests that only need a known tool.
type stubAdapter struct{ name string }

func (s stubAdapter) Name() string                         { return s.name }
func (s stubAdapter) Efforts() []string                    { return nil }
func (s stubAdapter) ReadOnly() adapter.Verification       { return adapter.Verification{} }
func (s stubAdapter) Prompt() adapter.Verification         { return adapter.Verification{} }
func (s stubAdapter) Parse([]byte) (adapter.Result, error) { return adapter.Result{}, nil }
func (s stubAdapter) Build(adapter.Request, config.ToolConfig) (runner.Invocation, error) {
	return runner.Invocation{}, nil
}

func testDeps(t *testing.T) (Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	testenv.Isolate(t)
	var out, errb bytes.Buffer
	return Deps{
		Stdin:           strings.NewReader(""),
		StdinIsTerminal: true,
		Stdout:          &out,
		Stderr:          &errb,
		Env:             os.Getenv,
		Now:             func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		Pid:             4242,
		Adapters:        map[string]adapter.Adapter{"stub": stubAdapter{name: "stub"}},
		Signals:         make(chan os.Signal),
		Version:         func(string) (string, error) { return "0.0.0", nil },
	}, &out, &errb
}

func run(t *testing.T, d Deps, args ...string) int {
	t.Helper()
	cmd := NewRootCmd(d)
	cmd.SetArgs(args)
	return Execute(cmd)
}

func TestRootHelpListsUsage(t *testing.T) {
	d, out, _ := testDeps(t)
	if code := run(t, d, "--help"); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	help := out.String()
	for _, want := range []string{"ask <tool> <model> [prompt...]", "stub", "Exit codes"} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not mention %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "\u2014") {
		t.Errorf("help contains an em dash")
	}
}

func TestUnknownToolExits2(t *testing.T) {
	d, _, errb := testDeps(t)
	if code := run(t, d, "nosuchtool", "some-model", "hello"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown tool") {
		t.Errorf("stderr does not say unknown tool: %q", errb.String())
	}
}

func TestFlagsDeclared(t *testing.T) {
	d, _, _ := testDeps(t)
	cmd := NewRootCmd(d)
	for _, name := range []string{"write", "timeout", "effort", "final", "dry-run", "no-report", "verbose"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is not declared", name)
		}
	}
	for _, short := range []string{"C", "f"} {
		if cmd.Flags().ShorthandLookup(short) == nil {
			t.Errorf("flag -%s is not declared", short)
		}
	}
}

func TestMissingArgsExits2(t *testing.T) {
	d, _, _ := testDeps(t)
	if code := run(t, d, "stub"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestBadFlagExits2(t *testing.T) {
	d, _, _ := testDeps(t)
	if code := run(t, d, "--no-such-flag", "stub", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	d, _, _ = testDeps(t)
	if code := run(t, d, "--timeout", "soon", "stub", "m", "hi"); code != 2 {
		t.Fatalf("bad --timeout: exit %d, want 2", code)
	}
}

func TestRefusedErrorExits2(t *testing.T) {
	d, _, errb := testDeps(t)
	cmd := NewRootCmd(d)
	cmd.RunE = func(*cobra.Command, []string) error {
		return adapter.Refuse("read-only is not tested for %s", "stub")
	}
	cmd.SetArgs([]string{"stub", "m", "hi"})
	if code := Execute(cmd); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "read-only is not tested for stub") {
		t.Errorf("stderr does not carry the refusal: %q", errb.String())
	}

	// A wrapped refusal still maps to exit 2.
	cmd = NewRootCmd(d)
	cmd.RunE = func(*cobra.Command, []string) error {
		return errors.Join(errors.New("context"), adapter.Refuse("no"))
	}
	cmd.SetArgs([]string{"stub", "m", "hi"})
	if code := Execute(cmd); code != 2 {
		t.Fatalf("wrapped refusal: exit %d, want 2", code)
	}
}

func TestOtherErrorExits1(t *testing.T) {
	d, _, _ := testDeps(t)
	cmd := NewRootCmd(d)
	cmd.RunE = func(*cobra.Command, []string) error { return errors.New("tool failed") }
	cmd.SetArgs([]string{"stub", "m", "hi"})
	if code := Execute(cmd); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestIsolateKeepsPathsUnderTempHome(t *testing.T) {
	fake := t.TempDir() + "/fakecli"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, fake)
	if os.Getenv("HOME") != home {
		t.Fatalf("HOME is not the temp home")
	}
	path := os.Getenv("PATH")
	if !testenv.Under(home, path) || strings.Contains(path, string(os.PathListSeparator)) {
		t.Fatalf("PATH %q is not one folder under the temp home", path)
	}
	info, err := os.Stat(path + "/fakecli")
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("fake CLI not installed as 0755: %v %v", info, err)
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if !testenv.Under(home, os.Getenv(k)) {
			t.Errorf("%s is not under the temp home", k)
		}
	}
}

func TestDoubleDashEndsFlags(t *testing.T) {
	d, _, _ := testDeps(t)
	cmd := NewRootCmd(d)
	var got []string
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		got = args
		return nil
	}
	cmd.SetArgs([]string{"codex", "m", "--", "--verbose"})
	if code := Execute(cmd); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if v, _ := cmd.Flags().GetBool("verbose"); v {
		t.Errorf("--verbose after -- was read as a flag")
	}
	if strings.Join(got, " ") != "codex m --verbose" {
		t.Errorf("args are %q, want codex m --verbose", got)
	}
}

func TestIsolateClearsKeys(t *testing.T) {
	t.Setenv("EXAMPLE_API_KEY", "placeholder")
	t.Setenv("EXAMPLE_KEY", "placeholder")
	t.Setenv("CODEX_HOME", "/Users/example/.codex")
	t.Setenv("EXAMPLE_TOKEN", "placeholder")
	t.Setenv("EXAMPLE_SECRET_FILE", "placeholder")
	t.Setenv("EXAMPLE_PASSWORD", "placeholder")
	testenv.Isolate(t)
	for _, k := range []string{"EXAMPLE_API_KEY", "EXAMPLE_KEY", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_CONFIG", "EXAMPLE_TOKEN", "EXAMPLE_SECRET_FILE", "EXAMPLE_PASSWORD"} {
		if _, set := os.LookupEnv(k); set {
			t.Errorf("%s is still set after Isolate", k)
		}
	}
}

// fakeAdapter drives a fake CLI script named "fake" on the isolated PATH.
// Its final message is the CLI's trimmed stdout.
type fakeAdapter struct {
	ro      adapter.Verification
	efforts []string
	// onParse, when set, runs at each Parse, after the CLI has exited.
	onParse func()
}

func (f fakeAdapter) Name() string                   { return "fake" }
func (f fakeAdapter) Efforts() []string              { return f.efforts }
func (f fakeAdapter) ReadOnly() adapter.Verification { return f.ro }
func (f fakeAdapter) Prompt() adapter.Verification   { return adapter.Verification{OK: true} }
func (f fakeAdapter) Build(req adapter.Request, tc config.ToolConfig) (runner.Invocation, error) {
	bin, err := config.BinaryFor("fake", tc)
	if err != nil {
		return runner.Invocation{}, &adapter.SetupError{Err: err}
	}
	args := []string{"--model", adapter.ResolveModel(req.Model, tc)}
	if req.Write {
		args = append(args, "--write")
	}
	return runner.Invocation{Binary: bin, Args: args, Stdin: req.Prompt, Dir: req.Dir}, nil
}
func (f fakeAdapter) Parse(stdout []byte) (adapter.Result, error) {
	if f.onParse != nil {
		f.onParse()
	}
	s := strings.TrimSpace(string(stdout))
	if s == "BADPARSE" {
		return adapter.Result{}, errors.New("fake: bad output")
	}
	if s == "" {
		return adapter.Result{}, nil
	}
	return adapter.Result{Final: s, InputTokens: 11, OutputTokens: 22}, nil
}

const tested = "fake 1.0.0"

var verified = adapter.Verification{OK: true, TestedVersion: tested}

// promptMarker is prompt text that must never reach a log or report.
const promptMarker = "PROMPT-MARKER-7f3a"

// fakeRun holds one test's isolated setup.
type fakeRun struct {
	d        Deps
	out, err *bytes.Buffer
	home     string
}

// newFake isolates the test, installs a fake CLI whose body is script
// (after a line that appends to $HOME/ran), and writes cfg as the config
// file when it is not empty.
func newFake(t *testing.T, script, cfg string, ro adapter.Verification) *fakeRun {
	t.Helper()
	src := filepath.Join(t.TempDir(), "fake")
	body := "#!/bin/sh\necho run >> \"$HOME/ran\"\n" + script + "\n"
	if err := os.WriteFile(src, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, src)
	if cfg != "" {
		cfg = strings.ReplaceAll(cfg, "$HOME", home)
		p := config.DefaultPath(os.Getenv)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	d := Deps{
		Stdin:           strings.NewReader(""),
		StdinIsTerminal: true,
		Stdout:          &out,
		Stderr:          &errb,
		Env:             os.Getenv,
		Now:             func() time.Time { return time.Date(2026, 11, 7, 15, 30, 0, 0, time.UTC) },
		Pid:             4242,
		Adapters:        map[string]adapter.Adapter{"fake": fakeAdapter{ro: ro}},
		Signals:         make(chan os.Signal),
		Version:         func(string) (string, error) { return tested, nil },
		Argv0:           "ask",
	}
	return &fakeRun{d: d, out: &out, err: &errb, home: home}
}

// runs returns how many times the fake CLI started.
func (f *fakeRun) runs(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.home, "ran"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "run\n")
}

func (f *fakeRun) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.home, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func (f *fakeRun) reports(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "reports")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

func (f *fakeRun) logText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "log.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(data)
}

const secretsCfg = `{"tools": {"fake": {
  "pass_env": ["EXAMPLE_PASS"],
  "secrets": [
    {"name": "ro_key", "env_var": "EXAMPLE_RO_TOKEN", "ref": "file:$HOME/ro-secret"},
    {"name": "api_key", "env_var": "EXAMPLE_API_KEY", "ref": "file:$HOME/api-secret"}
  ],
  "read_only_secrets": ["ro_key"]}}}`

func TestReadOnlyRefusedWhenUnverified(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, f.d, "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 0 {
		t.Fatal("the CLI ran")
	}
	if f.out.Len() != 0 {
		t.Errorf("stdout not empty: %q", f.out.String())
	}
}

func TestReadOnlyRefusedOnVersionChange(t *testing.T) {
	f := newFake(t, `echo answer`, "", verified)
	var asked string
	f.d.Version = func(bin string) (string, error) { asked = bin; return "fake 1.1.0", nil }
	if code := run(t, f.d, "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if f.runs(t) != 0 {
		t.Fatal("the CLI ran after a version change")
	}
	if filepath.Base(asked) != "fake" {
		t.Errorf("version asked of %q, want the fake binary", asked)
	}

	// A failing version check refuses too.
	f.d.Version = func(string) (string, error) { return "", errors.New("no version") }
	if code := run(t, f.d, "fake", "m", "hi"); code != 2 {
		t.Fatalf("version error: exit %d, want 2", code)
	}

	// The tested version runs.
	f.d.Version = func(string) (string, error) { return tested, nil }
	if code := run(t, f.d, "fake", "m", "hi"); code != 0 {
		t.Fatalf("tested version: exit %d, want 0; stderr %q", code, f.err.String())
	}
}

func TestClaudeReadOnlyRefusedOnOtherVersion(t *testing.T) {
	const testedClaude = "2.1.319 (Claude Code)"
	f := newFake(t, `echo answer`, "", verified)
	// A fake claude on the isolated PATH, driven by the real adapter.
	src := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\necho run >> \"$HOME/ran\"\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"answer"}'` + "\n"
	if err := os.WriteFile(src, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "bin", "claude"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	f.d.Adapters = map[string]adapter.Adapter{"claude": adapter.NewClaude()}

	for _, ver := range []string{"2.1.320 (Claude Code)", "2.1.319", "2.1.318 (Claude Code)", ""} {
		f.d.Version = func(string) (string, error) { return ver, nil }
		if code := run(t, f.d, "--no-report", "claude", "m", "hi"); code != 2 {
			t.Fatalf("version %q: exit %d, want 2", ver, code)
		}
		if f.runs(t) != 0 {
			t.Fatalf("version %q: claude ran", ver)
		}
	}

	f.d.Version = func(string) (string, error) { return testedClaude, nil }
	if code := run(t, f.d, "--no-report", "claude", "m", "hi"); code != 0 {
		t.Fatalf("tested version: exit %d, want 0; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 1 {
		t.Errorf("tested version: claude ran %d times, want 1", f.runs(t))
	}
}

func TestReadOnlyOmitsWriteOnlySecrets(t *testing.T) {
	f := newFake(t, `printf '%s' "$EXAMPLE_RO_TOKEN" > "$HOME/ro-seen"
/usr/bin/env | /usr/bin/cut -d= -f1 > "$HOME/env-names"
echo answer`, secretsCfg, verified)
	// api_key has no file: a lookup of it would fail with exit 3.
	if err := os.WriteFile(filepath.Join(f.home, "ro-secret"), []byte("ro-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	names := strings.Fields(f.read(t, "env-names"))
	if slices.Contains(names, "EXAMPLE_API_KEY") {
		t.Error("a write-only secret reached a read-only run")
	}
	if got := f.read(t, "ro-seen"); got != "ro-value" {
		t.Errorf("read-only secret is %q, want ro-value", got)
	}
}

func TestWriteRunsAndReports(t *testing.T) {
	f := newFake(t, `/bin/cat > "$HOME/stdin-seen"
echo "$@" > "$HOME/args"
echo "the answer"`, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "fake", "m", promptMarker); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	if f.out.String() != "the answer\n" {
		t.Errorf("stdout %q, want only the final message", f.out.String())
	}
	if got := f.read(t, "stdin-seen"); got != promptMarker {
		t.Errorf("prompt on stdin %q, want %q", got, promptMarker)
	}
	if args := f.read(t, "args"); !strings.Contains(args, "--write") || strings.Contains(args, promptMarker) {
		t.Errorf("args %q: want --write and no prompt", args)
	}
	reps := f.reports(t)
	if len(reps) != 1 || filepath.Base(reps[0]) != "20261107T153000Z-fake-4242-1.md" {
		t.Fatalf("reports %q, want one named for the run", reps)
	}
	data, _ := os.ReadFile(reps[0])
	if string(data) != "the answer\n" {
		t.Errorf("report holds %q", data)
	}
	log := f.logText(t)
	if strings.Count(log, "\n") != 1 || strings.Contains(log, promptMarker) || !strings.Contains(log, `"tool":"fake"`) {
		t.Errorf("log %q: want one line for fake, no prompt", log)
	}
}

const failFirst = `n=$(/usr/bin/wc -l < "$HOME/ran")
if [ "$n" -lt 2 ]; then exit 1; fi
echo "second try"`

func TestRetryOnceWhenEmptyReadOnly(t *testing.T) {
	f := newFake(t, failFirst, "", verified)
	if code := run(t, f.d, "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 2 {
		t.Fatalf("CLI ran %d times, want 2", f.runs(t))
	}
	if f.out.String() != "second try\n" {
		t.Errorf("stdout %q", f.out.String())
	}

	// A parse error counts as an empty final; still only one retry.
	g := newFake(t, `echo BADPARSE; exit 1`, "", verified)
	if code := run(t, g.d, "fake", "m", "hi"); code != 1 {
		t.Fatalf("always failing: exit %d, want 1", code)
	}
	if g.runs(t) != 2 {
		t.Fatalf("always failing: CLI ran %d times, want 2", g.runs(t))
	}

	// A non-zero exit with a final message is not retried.
	h := newFake(t, `echo partial; exit 1`, "", verified)
	if code := run(t, h.d, "fake", "m", "hi"); code != 1 {
		t.Fatalf("final with failure: exit %d, want 1", code)
	}
	if h.runs(t) != 1 {
		t.Fatalf("final with failure: CLI ran %d times, want 1", h.runs(t))
	}
}

func TestNoRetryWithWrite(t *testing.T) {
	f := newFake(t, failFirst, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if f.runs(t) != 1 {
		t.Fatalf("CLI ran %d times with --write, want 1", f.runs(t))
	}
}

const multiLine = `printf 'line one\nRESULT: a\nmiddle\nRESULT: b\nend\n'`

func TestFinalMatch(t *testing.T) {
	f := newFake(t, multiLine, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "--final", "^RESULT:", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	lines := strings.Split(strings.TrimSuffix(f.out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "RESULT: b" {
		t.Fatalf("stdout %q, want the last match and the report path", f.out.String())
	}
	if reps := f.reports(t); len(reps) != 1 || lines[1] != reps[0] {
		t.Errorf("report path %q, reports %q", lines[1], reps)
	}

	g := newFake(t, `echo x`, "", adapter.Verification{})
	if code := run(t, g.d, "--write", "--final", "(", "fake", "m", "hi"); code != 2 {
		t.Fatalf("bad regexp: exit %d, want 2", code)
	}
	if g.runs(t) != 0 {
		t.Error("CLI ran with a bad --final")
	}
}

func TestFinalNoMatchExits1(t *testing.T) {
	f := newFake(t, multiLine, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "--final", "^NOPE", "fake", "m", "hi"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if f.out.Len() != 0 {
		t.Errorf("stdout %q, want empty", f.out.String())
	}
}

func TestFinalWithNoReportExits2(t *testing.T) {
	f := newFake(t, `echo x`, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "--no-report", "--final", "x", "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if f.runs(t) != 0 {
		t.Error("CLI ran")
	}
}

func TestTimeoutExits124(t *testing.T) {
	f := newFake(t, `exec /bin/sleep 30`, "", verified)
	start := time.Now()
	if code := run(t, f.d, "--timeout", "300ms", "fake", "m", "hi"); code != 124 {
		t.Fatalf("exit %d, want 124", code)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
	// No retry after a timeout. Under load the deadline can fire before the
	// fake records its start, so zero starts is fine too.
	if n := f.runs(t); n > 1 {
		t.Errorf("CLI ran %d times after a timeout, want at most 1", n)
	}
	if !strings.Contains(f.logText(t), `"exit":124`) {
		t.Errorf("log does not record exit 124: %q", f.logText(t))
	}

	// The tool's config timeout applies when --timeout is not given.
	g := newFake(t, `exec /bin/sleep 30`, `{"tools": {"fake": {"timeout_seconds": 1}}}`, verified)
	if code := run(t, g.d, "fake", "m", "hi"); code != 124 {
		t.Fatalf("config timeout: exit %d, want 124", code)
	}
	if n := g.runs(t); n > 1 {
		t.Errorf("config timeout: CLI ran %d times, want at most 1", n)
	}
}

func TestSecretLookupFailureExits3(t *testing.T) {
	f := newFake(t, `echo answer`, secretsCfg, adapter.Verification{})
	if err := os.WriteFile(filepath.Join(f.home, "ro-secret"), []byte("ro-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if f.runs(t) != 0 {
		t.Error("CLI ran")
	}
	msg := f.err.String()
	if !strings.Contains(msg, "api_key") || strings.Contains(msg, "api-secret") || strings.Contains(msg, f.home) {
		t.Errorf("stderr %q: want the key name only", msg)
	}
}

func TestDryRunRunsNothing(t *testing.T) {
	f := newFake(t, `echo answer`, secretsCfg, verified)
	// Neither secret file exists: a dry run looks nothing up.
	if code := run(t, f.d, "--write", "--dry-run", "fake", "m", promptMarker); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	out := f.out.String()
	for _, want := range []string{"EXAMPLE_API_KEY=<api_key>", "EXAMPLE_RO_TOKEN=<ro_key>", "EXAMPLE_PASS=<pass_env>", "--write", fmt.Sprintf("<prompt: %d bytes>", len(promptMarker))} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, promptMarker) {
		t.Error("dry run shows the prompt")
	}
	if f.runs(t) != 0 || len(f.reports(t)) != 0 {
		t.Error("dry run ran the CLI or saved a report")
	}

	// Read-only shows only the read-only secrets, and never runs the
	// version check.
	f.out.Reset()
	f.d.Version = func(string) (string, error) {
		t.Error("dry run called Version")
		return tested, nil
	}
	if code := run(t, f.d, "--dry-run", "fake", "m", "hi"); code != 0 {
		t.Fatalf("read-only dry run: exit %d", code)
	}
	if out := f.out.String(); strings.Contains(out, "EXAMPLE_API_KEY") || !strings.Contains(out, "EXAMPLE_RO_TOKEN=<ro_key>") {
		t.Errorf("read-only dry run %q", out)
	}
	if !strings.Contains(f.err.String(), `read-only: verified for "fake 1.0.0"`) {
		t.Errorf("stderr %q lacks the gate status", f.err.String())
	}

	// An unverified read-only setup still dry-runs, and says so.
	f.d.Adapters = map[string]adapter.Adapter{"fake": fakeAdapter{}}
	f.err.Reset()
	if code := run(t, f.d, "--dry-run", "fake", "m", "hi"); code != 0 {
		t.Fatalf("unverified dry run: exit %d", code)
	}
	if !strings.Contains(f.err.String(), "read-only: not verified") {
		t.Errorf("stderr %q lacks the gate status", f.err.String())
	}
	if f.runs(t) != 0 {
		t.Error("dry run ran the CLI")
	}
}

func TestNoReportFlag(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "--no-report", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if f.out.String() != "answer\n" {
		t.Errorf("stdout %q", f.out.String())
	}
	if reps := f.reports(t); len(reps) != 0 {
		t.Errorf("reports saved: %q", reps)
	}
	if f.logText(t) == "" {
		t.Error("no log line")
	}
}

func TestVerboseGoesToStderr(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "--verbose", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if f.out.String() != "answer\n" {
		t.Errorf("stdout %q, want only the final message", f.out.String())
	}
	msg := f.err.String()
	for _, want := range []string{"tool=fake", "model=m", "duration=", "tokens=11/22", "exit=0", "report=" + f.reports(t)[0]} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q lacks %q", msg, want)
		}
	}
}

func TestAliasImpliesTool(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	f.d.Adapters = map[string]adapter.Adapter{"codex": fakeAdapter{}, "fake": fakeAdapter{}}
	writeConfig(t, `{"tools": {"codex": {"binary": "fake"}}}`)
	f.d.Argv0 = "/usr/local/bin/ask-codex"
	if code := run(t, f.d, "--write", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 1 || f.out.String() != "answer\n" {
		t.Fatalf("runs %d, stdout %q", f.runs(t), f.out.String())
	}

	f.d.Argv0 = "ask-nosuchtool"
	if code := run(t, f.d, "--write", "m", "hi"); code != 2 {
		t.Fatalf("unknown alias: exit %d, want 2", code)
	}
}

func TestAliasOnlyForKnownTools(t *testing.T) {
	for _, argv0 := range []string{"ask-check", "/usr/local/bin/ask-fake"} {
		f := newFake(t, `echo answer`, "", adapter.Verification{})
		f.d.Adapters = map[string]adapter.Adapter{"fake": fakeAdapter{}}
		f.d.Argv0 = argv0
		if got := aliasTool(argv0); got != "" {
			t.Errorf("aliasTool(%s) = %q, want none", argv0, got)
		}
		// Not an alias: the first word is still the tool.
		if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 0 {
			t.Fatalf("%s: exit %d, want 0; stderr %q", argv0, code, f.err.String())
		}
		if f.out.String() != "answer\n" {
			t.Errorf("%s: stdout %q", argv0, f.out.String())
		}
	}
	for _, tool := range []string{"codex", "agy", "opencode", "claude"} {
		if got := aliasTool("/usr/local/bin/ask-" + tool); got != tool {
			t.Errorf("aliasTool(ask-%s) = %q", tool, got)
		}
	}
}

func TestChildEnvIsAllowlist(t *testing.T) {
	f := newFake(t, `/usr/bin/env | /usr/bin/cut -d= -f1 > "$HOME/env-names"
echo answer`, secretsCfg, adapter.Verification{})
	for _, name := range []string{"ro-secret", "api-secret"} {
		if err := os.WriteFile(filepath.Join(f.home, name), []byte("value"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("EXAMPLE_PASS", "pass")
	t.Setenv("EXAMPLE_PARENT_ONLY", "parent")
	t.Setenv("LANG", "C")
	if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	names := strings.Fields(f.read(t, "env-names"))
	allowed := []string{"PATH", "HOME", "USER", "LANG", "TERM", "TMPDIR", "SHELL",
		"EXAMPLE_PASS", "EXAMPLE_API_KEY", "EXAMPLE_RO_TOKEN",
		// Set by /bin/sh itself, not passed by ask.
		"PWD", "OLDPWD", "SHLVL", "_"}
	for _, n := range names {
		if !slices.Contains(allowed, n) && !strings.HasPrefix(n, "XDG_") {
			t.Errorf("child env has %s", n)
		}
	}
	for _, want := range []string{"PATH", "HOME", "LANG", "XDG_STATE_HOME", "EXAMPLE_PASS", "EXAMPLE_API_KEY"} {
		if !slices.Contains(names, want) {
			t.Errorf("child env lacks %s", want)
		}
	}
}

func TestOutputRedactsSecrets(t *testing.T) {
	f := newFake(t, `echo "key is $EXAMPLE_API_KEY and $EXAMPLE_RO_TOKEN"`, secretsCfg, adapter.Verification{})
	const value = "example-secret-value"
	if err := os.WriteFile(filepath.Join(f.home, "api-secret"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	// A 3-byte value is redacted too.
	if err := os.WriteFile(filepath.Join(f.home, "ro-secret"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	want := "key is <redacted:api_key> and <redacted:ro_key>\n"
	if f.out.String() != want {
		t.Errorf("stdout %q, want %q", f.out.String(), want)
	}
	reps := f.reports(t)
	if len(reps) != 1 {
		t.Fatalf("reports %q", reps)
	}
	if data, _ := os.ReadFile(reps[0]); string(data) != want {
		t.Errorf("report %q, want %q", data, want)
	}
}

func TestMissingBinaryExits3(t *testing.T) {
	d, _, errb := testDeps(t)
	d.Adapters = map[string]adapter.Adapter{"fake": fakeAdapter{ro: verified}}
	d.Version = func(string) (string, error) {
		t.Error("Version called with no binary")
		return tested, nil
	}
	if code := run(t, d, "fake", "m", "hi"); code != 3 {
		t.Fatalf("exit %d, want 3; stderr %q", code, errb.String())
	}
	// An unverified tool with no binary is still a setup failure.
	d.Adapters = map[string]adapter.Adapter{"fake": fakeAdapter{}}
	if code := run(t, d, "fake", "m", "hi"); code != 3 {
		t.Fatalf("unverified: exit %d, want 3", code)
	}
}

func TestNilVersionRefuses(t *testing.T) {
	f := newFake(t, `echo answer`, "", verified)
	f.d.Version = nil
	if code := run(t, f.d, "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if f.runs(t) != 0 {
		t.Error("the CLI ran")
	}
}

func TestSignalBeforeRunExits(t *testing.T) {
	for _, mode := range [][]string{nil, {"--write"}} {
		f := newFake(t, `echo answer`, "", verified)
		sigs := make(chan os.Signal, 1)
		sigs <- syscall.SIGTERM
		f.d.Signals = sigs
		f.d.Version = func(string) (string, error) {
			t.Error("Version called after a signal")
			return tested, nil
		}
		args := append(append([]string{}, mode...), "fake", "m", "hi")
		if code := run(t, f.d, args...); code != 128+15 {
			t.Fatalf("%v: exit %d, want 143", mode, code)
		}
		if f.runs(t) != 0 || f.out.Len() != 0 {
			t.Errorf("%v: CLI ran %d times, stdout %q", mode, f.runs(t), f.out.String())
		}
	}

	// A signal between the first attempt and the retry stops the retry.
	f := newFake(t, failFirst, "", verified)
	sigs := make(chan os.Signal, 1)
	f.d.Signals = sigs
	f.d.Adapters = map[string]adapter.Adapter{"fake": fakeAdapter{ro: verified, onParse: func() {
		select {
		case sigs <- syscall.SIGINT:
		default:
		}
	}}}
	if code := run(t, f.d, "fake", "m", "hi"); code != 130 {
		t.Fatalf("before retry: exit %d, want 130", code)
	}
	if f.runs(t) != 1 {
		t.Errorf("before retry: CLI ran %d times, want 1", f.runs(t))
	}
}

func TestEmptyFinalExits1(t *testing.T) {
	for _, extra := range [][]string{nil, {"--final", "x"}} {
		f := newFake(t, `exit 0`, "", adapter.Verification{})
		args := append(append([]string{"--write"}, extra...), "fake", "m", "hi")
		if code := run(t, f.d, args...); code != 1 {
			t.Fatalf("%v: exit %d, want 1", extra, code)
		}
		if f.out.Len() != 0 {
			t.Errorf("%v: stdout %q", extra, f.out.String())
		}
		if !strings.Contains(f.err.String(), "no final message") {
			t.Errorf("%v: stderr %q", extra, f.err.String())
		}
	}
}

func TestFinalTrimsCarriageReturn(t *testing.T) {
	f := newFake(t, `printf 'RESULT: a\r\nend\r\n'`, "", adapter.Verification{})
	if code := run(t, f.d, "--write", "--final", "^RESULT: a$", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if lines := strings.Split(f.out.String(), "\n"); lines[0] != "RESULT: a" {
		t.Errorf("stdout %q", f.out.String())
	}
}

func TestHelpAliasAndExitTable(t *testing.T) {
	d, out, _ := testDeps(t)
	d.Argv0 = "ask-claude"
	d.Adapters = map[string]adapter.Adapter{"claude": stubAdapter{name: "claude"}}
	if code := run(t, d, "--help"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	help := out.String()
	if !strings.Contains(help, "ask-claude <model> [prompt...]") {
		t.Errorf("alias help lacks its usage:\n%s", help)
	}
	if !strings.Contains(help, "report save") || !strings.Contains(help, "no final message") {
		t.Errorf("exit table is not updated:\n%s", help)
	}
}

func TestSignalDuringFailedStepExits(t *testing.T) {
	// The version check gets a signal, then fails.
	f := newFake(t, `echo answer`, "", verified)
	sigs := make(chan os.Signal, 1)
	f.d.Signals = sigs
	f.d.Version = func(string) (string, error) {
		sigs <- syscall.SIGTERM
		return "", errors.New("no version")
	}
	if code := run(t, f.d, "fake", "m", "hi"); code != 128+15 {
		t.Fatalf("version: exit %d, want 143", code)
	}
	if f.runs(t) != 0 {
		t.Error("version: the CLI ran")
	}

	// The secret lookup gets a signal, then fails.
	g := newFake(t, `echo answer`, secretsCfg, adapter.Verification{})
	gsigs := make(chan os.Signal, 1)
	g.d.Signals = gsigs
	g.d.Resolve = func(config.SecretRef) (string, error) {
		gsigs <- syscall.SIGHUP
		return "", errors.New("lookup failed")
	}
	if code := run(t, g.d, "--write", "fake", "m", "hi"); code != 128+1 {
		t.Fatalf("secret: exit %d, want 129", code)
	}
	if g.runs(t) != 0 {
		t.Error("secret: the CLI ran")
	}

	// A signal during a lookup that succeeds also stops the run.
	h := newFake(t, `echo answer`, secretsCfg, adapter.Verification{})
	hsigs := make(chan os.Signal, 1)
	h.d.Signals = hsigs
	h.d.Resolve = func(config.SecretRef) (string, error) {
		select {
		case hsigs <- syscall.SIGINT:
		default:
		}
		return "value", nil
	}
	if code := run(t, h.d, "--write", "fake", "m", "hi"); code != 130 {
		t.Fatalf("secret ok: exit %d, want 130", code)
	}
	if h.runs(t) != 0 {
		t.Error("secret ok: the CLI ran")
	}
}

func TestConfiguredBinaryMissingExits3(t *testing.T) {
	for name, setup := range map[string]func(home string) string{
		"missing": func(home string) string { return filepath.Join(home, "no-such-cli") },
		"not executable": func(home string) string {
			p := filepath.Join(home, "plain-file")
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"folder": func(home string) string { return home },
	} {
		f := newFake(t, `echo answer`, "", verified)
		cfg := `{"tools": {"fake": {"binary": "` + setup(f.home) + `"}}}`
		p := config.DefaultPath(os.Getenv)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		f.d.Version = func(string) (string, error) {
			t.Errorf("%s: Version called", name)
			return tested, nil
		}
		for _, mode := range [][]string{nil, {"--write"}} {
			f.err.Reset()
			args := append(append([]string{}, mode...), "fake", "m", "hi")
			if code := run(t, f.d, args...); code != 3 {
				t.Fatalf("%s %v: exit %d, want 3; stderr %q", name, mode, code, f.err.String())
			}
			if strings.Contains(f.err.String(), f.home) {
				t.Errorf("%s %v: stderr names the path: %q", name, mode, f.err.String())
			}
		}
		if f.runs(t) != 0 {
			t.Errorf("%s: the CLI ran", name)
		}
	}
}

// writeConfig writes cfg as the isolated config file.
func writeConfig(t *testing.T, cfg string) {
	t.Helper()
	p := config.DefaultPath(os.Getenv)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredBinaryBareName(t *testing.T) {
	f := newFake(t, `echo "$0" > "$HOME/argv0"
echo answer`, "", verified)
	writeConfig(t, `{"tools": {"fake": {"binary": "fake"}}}`)
	var asked string
	f.d.Version = func(bin string) (string, error) { asked = bin; return tested, nil }
	if code := run(t, f.d, "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	want := filepath.Join(f.home, "bin", "fake")
	if asked != want {
		t.Errorf("version asked of %q, want %q", asked, want)
	}
	if got := strings.TrimSpace(f.read(t, "argv0")); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
}

func TestConfiguredBinaryRelativeIgnoresDir(t *testing.T) {
	f := newFake(t, `echo "$0" > "$HOME/argv0"
echo answer`, "", verified)
	writeConfig(t, `{"tools": {"fake": {"binary": "bin/fake"}}}`)
	t.Chdir(f.home)
	other := t.TempDir()
	for _, mode := range [][]string{nil, {"--write"}} {
		args := append(append([]string{}, mode...), "-C", other, "fake", "m", "hi")
		if code := run(t, f.d, args...); code != 0 {
			t.Fatalf("%v: exit %d, want 0; stderr %q", mode, code, f.err.String())
		}
		got := strings.TrimSpace(f.read(t, "argv0"))
		want, err := filepath.Abs(filepath.Join("bin", "fake"))
		if err != nil {
			t.Fatal(err)
		}
		if got != want || !filepath.IsAbs(got) {
			t.Errorf("%v: ran %q, want %q", mode, got, want)
		}
	}
}

func TestStdinWaitIsInterruptible(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	f.d.Stdin = pr
	f.d.StdinIsTerminal = false
	sigs := make(chan os.Signal, 1)
	f.d.Signals = sigs
	go func() {
		time.Sleep(100 * time.Millisecond)
		sigs <- syscall.SIGTERM
	}()
	done := make(chan int, 1)
	go func() { done <- run(t, f.d, "--write", "fake", "m", "-") }()
	select {
	case code := <-done:
		if code != 128+15 {
			t.Fatalf("exit %d, want 143", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ask kept waiting on stdin after SIGTERM")
	}
	if f.runs(t) != 0 {
		t.Error("the CLI ran")
	}
}

func TestStdinOver16MiBExits2(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	f.d.Stdin = strings.NewReader(strings.Repeat("a", 16<<20+1))
	f.d.StdinIsTerminal = false
	if code := run(t, f.d, "--write", "fake", "m", "-"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(f.err.String(), "stdin is larger than 16 MiB") {
		t.Errorf("stderr %q", f.err.String())
	}
	if f.runs(t) != 0 {
		t.Error("the CLI ran")
	}
}

func TestVerboseShowsChildStderrOnFailure(t *testing.T) {
	const value = "example-secret-value"
	script := `echo "noise before" >&2
i=0; while [ $i -lt 600 ]; do printf 'xxxxxxxxx\n' >&2; i=$((i+1)); done
echo "bad key $EXAMPLE_API_KEY" >&2
exit 1`
	f := newFake(t, script, secretsCfg, adapter.Verification{})
	for _, name := range []string{"api-secret", "ro-secret"} {
		if err := os.WriteFile(filepath.Join(f.home, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := run(t, f.d, "--write", "--verbose", "fake", "m", "hi"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	msg := f.err.String()
	if !strings.Contains(msg, "bad key <redacted:") || strings.Contains(msg, value) {
		t.Errorf("stderr lacks the redacted child stderr, or holds the value:\n%s", msg)
	}
	if strings.Contains(msg, "noise before") || strings.Count(msg, "x") > 4096 {
		t.Errorf("stderr shows more than the last 4 KiB of child stderr")
	}

	// Without --verbose, child stderr never shows.
	f.err.Reset()
	if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(f.err.String(), "bad key") {
		t.Errorf("child stderr shown without --verbose: %q", f.err.String())
	}

	// A run that succeeds shows no child stderr, even with --verbose.
	g := newFake(t, `echo noise >&2; echo answer`, "", adapter.Verification{})
	if code := run(t, g.d, "--write", "--verbose", "fake", "m", "hi"); code != 0 {
		t.Fatalf("ok run: exit %d", code)
	}
	if strings.Contains(g.err.String(), "noise") {
		t.Errorf("child stderr shown after success: %q", g.err.String())
	}
}

// logLines returns the run log's lines, or none when there is no log.
func logLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "log.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestLogOnEveryExit(t *testing.T) {
	check := func(name string, code, want int) {
		t.Helper()
		if code != want {
			t.Fatalf("%s: exit %d, want %d", name, code, want)
		}
		lines := logLines(t)
		if len(lines) != 1 {
			t.Fatalf("%s: log has %d lines, want 1", name, len(lines))
		}
		if !strings.Contains(lines[0], fmt.Sprintf(`"exit":%d`, want)) || !strings.Contains(lines[0], `"tool":"fake"`) || strings.Contains(lines[0], promptMarker) {
			t.Errorf("%s: log line %q", name, lines[0])
		}
	}

	ok := newFake(t, `echo answer`, "", adapter.Verification{})
	check("success", run(t, ok.d, "--write", "fake", "m", promptMarker), 0)

	f := newFake(t, `echo answer`, "", adapter.Verification{})
	check("refusal", run(t, f.d, "fake", "m", promptMarker), 2)

	g := newFake(t, `echo answer`, secretsCfg, adapter.Verification{})
	check("secret lookup", run(t, g.d, "--write", "fake", "m", promptMarker), 3)

	h := newFake(t, `echo answer`, "", verified)
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM
	h.d.Signals = sigs
	check("early signal", run(t, h.d, "fake", "m", promptMarker), 143)

	k := newFake(t, `echo answer`, "", adapter.Verification{})
	check("bad flags", run(t, k.d, "--write", "--no-report", "--final", "x", "fake", "m", promptMarker), 2)

	// A dry run runs nothing and logs nothing.
	m := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, m.d, "--write", "--dry-run", "fake", "m", promptMarker); code != 0 {
		t.Fatalf("dry run: exit %d", code)
	}
	if lines := logLines(t); len(lines) != 0 {
		t.Errorf("dry run logged %q", lines)
	}
}

func TestReadmeMatchesHelp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	for _, want := range []string{
		"gave no final message, or `--final` found no match",
		"config, secret lookup, missing binary or report save",
		"killed by signal n",
		"agent-kit/config.json", "keychain:", "env:", "file:", "cmd:",
		"read_only_secrets", "log.jsonl", "--no-report", "ask-codex", "ps",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	for _, path := range []string{
		filepath.Join("..", "..", "..", "README.md"),
		filepath.Join("..", "..", "..", "AGENTS.md"),
		filepath.Join("..", "..", "README.md"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"does not work yet", "—"} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s holds %q", path, bad)
			}
		}
	}
}

func TestTailTextKeepsRunesAndMarkers(t *testing.T) {
	// A cut inside a marker moves to just after it.
	text := "aaaa<redacted:api_key>bbbb"
	if got := tailText(text, 10); got != "bbbb" {
		t.Errorf("cut in marker: %q, want %q", got, "bbbb")
	}
	// A cut at the marker's start keeps the whole marker.
	if got := tailText(text, 22); got != "<redacted:api_key>bbbb" {
		t.Errorf("cut at marker: %q", got)
	}
	// A cut inside a multi-byte rune moves to the next rune.
	text = "x\u00e9\u00e9y"
	got := tailText(text, 4)
	if !utf8.ValidString(got) || got != "\u00e9y" {
		t.Errorf("cut in rune: %q, want %q", got, "\u00e9y")
	}
	// Short text is kept whole.
	if got := tailText("short", 4096); got != "short" {
		t.Errorf("short: %q", got)
	}
}

func TestVerboseHelpMentionsStderr(t *testing.T) {
	d, _, _ := testDeps(t)
	usage := NewRootCmd(d).Flags().Lookup("verbose").Usage
	if !strings.Contains(usage, "last 4 KiB") || !strings.Contains(usage, "redacted") {
		t.Errorf("--verbose help %q does not mention the stderr tail", usage)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	for _, want := range []string{"last 4 KiB of the tool's stderr", "A dry run looks up no secrets, writes no report and adds no log line."} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
}

func TestLogModelIDFromAlias(t *testing.T) {
	f := newFake(t, `echo answer`, `{"tools": {"fake": {"models": {"short": "example/model"}}}}`, adapter.Verification{})
	if code := run(t, f.d, "--write", "--verbose", "fake", "short", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	lines := logLines(t)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	for _, want := range []string{`"model":"short"`, `"model_id":"example/model"`, `"reported_model":""`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %s lacks %s", lines[0], want)
		}
	}
	for _, want := range []string{"model=short", "model_id=example/model", "reported_model=none"} {
		if !strings.Contains(f.err.String(), want) {
			t.Errorf("stderr %q lacks %q", f.err.String(), want)
		}
	}
}

func TestLogReportedModelFromClaude(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "adapter", "testdata", "claude-stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n/bin/cat '"+fixture+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	testenv.Isolate(t, src)
	var out, errb bytes.Buffer
	d := Deps{
		Stdin:           strings.NewReader(""),
		StdinIsTerminal: true,
		Stdout:          &out,
		Stderr:          &errb,
		Env:             os.Getenv,
		Now:             func() time.Time { return time.Date(2026, 11, 7, 15, 30, 0, 0, time.UTC) },
		Pid:             4242,
		Adapters:        map[string]adapter.Adapter{"claude": adapter.NewClaude()},
		Signals:         make(chan os.Signal),
		Argv0:           "ask",
	}
	if code := run(t, d, "--write", "--verbose", "claude", "alias-model", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errb.String())
	}
	lines := logLines(t)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	for _, want := range []string{`"model_id":"alias-model"`, `"reported_model":"example-model"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %s lacks %s", lines[0], want)
		}
	}
	if !strings.Contains(errb.String(), "reported_model=example-model") {
		t.Errorf("stderr %q lacks reported_model=example-model", errb.String())
	}
}

func TestShowStderrDropsPartialFirstLineAfterCut(t *testing.T) {
	const value = "example-secret-value"
	// The runner kept only the tail, and the cut fell inside the secret.
	out := runner.Outcome{
		Stderr:          []byte(value[7:] + " rest of line\nlater line\n"),
		StderrTruncated: true,
	}
	var w bytes.Buffer
	showStderr(&w, false, "fake", out, map[string]string{"api_key": value})
	msg := w.String()
	if strings.Contains(msg, value[7:]) || strings.Contains(msg, "rest of line") {
		t.Errorf("partial first line shown after a cut:\n%s", msg)
	}
	if !strings.Contains(msg, "later line") {
		t.Errorf("complete lines missing:\n%s", msg)
	}

	// A cut tail with no newline is one partial line and shows nothing.
	w.Reset()
	out.Stderr = []byte(value[7:] + " no newline")
	showStderr(&w, false, "fake", out, map[string]string{"api_key": value})
	if strings.Contains(w.String(), value[7:]) {
		t.Errorf("partial line shown: %q", w.String())
	}

	// Without a cut, the first line stays.
	w.Reset()
	showStderr(&w, false, "fake", runner.Outcome{Stderr: []byte("first\nsecond\n")}, nil)
	if !strings.Contains(w.String(), "first") {
		t.Errorf("first line dropped without a cut: %q", w.String())
	}
}

func TestShowStderrRedactsSecretWithControlByte(t *testing.T) {
	// The secret holds a control byte. Stripping it first would leave a
	// string that no longer matches the secret, so the rest would print.
	const value = "example\x07secret-value"
	out := runner.Outcome{Stderr: []byte("token " + value + " end\n")}
	var w bytes.Buffer
	showStderr(&w, true, "fake", out, map[string]string{"api_key": value})
	msg := w.String()
	if strings.Contains(msg, "secret-value") || strings.Contains(msg, "example") {
		t.Errorf("secret shown on a terminal:\n%q", msg)
	}
	if !strings.Contains(msg, "<redacted:api_key>") {
		t.Errorf("no redaction marker:\n%q", msg)
	}

	// A secret that only matches after stripping is redacted too.
	w.Reset()
	out.Stderr = []byte("token example\x1b[0msecret2 end\n")
	showStderr(&w, true, "fake", out, map[string]string{"api_key": "examplesecret2"})
	if strings.Contains(w.String(), "examplesecret2") {
		t.Errorf("secret shown after stripping:\n%q", w.String())
	}

	// The header counts the bytes printed after the final redaction.
	w.Reset()
	out = runner.Outcome{Stderr: []byte(strings.Repeat("x", stderrShown) + value + "\n")}
	showStderr(&w, true, "fake", out, map[string]string{"api_key": value})
	header, body, _ := strings.Cut(w.String(), "\n")
	want := fmt.Sprintf("ask: last %d bytes of fake stderr:", len(body))
	if header != want {
		t.Errorf("header %q, want %q", header, want)
	}
}

func TestModelMustBeOneWord(t *testing.T) {
	for _, model := range []string{"", "explain this code", "a\tb", "a\x01b", strings.Repeat("m", 201)} {
		f := newFake(t, `echo answer`, "", verified)
		if code := run(t, f.d, "fake", model, "hi"); code != 2 {
			t.Errorf("model %q: exit %d, want 2", model, code)
		}
		if !strings.Contains(f.err.String(), "the model must be one word, for example ask claude haiku ...") {
			t.Errorf("model %q: stderr %q", model, f.err.String())
		}
		if f.runs(t) != 0 {
			t.Errorf("model %q: the CLI ran", model)
		}
	}
	f := newFake(t, `echo answer`, "", verified)
	if code := run(t, f.d, "fake", strings.Repeat("m", 200), "hi"); code != 0 {
		t.Errorf("200-character model: exit %d, want 0; stderr %q", code, f.err.String())
	}
}

func TestParseErrorHasOneToolPrefix(t *testing.T) {
	f := newFake(t, `echo BADPARSE`, "", verified)
	if code := run(t, f.d, "fake", "m", "hi"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, f.err.String())
	}
	msg := f.err.String()
	if strings.Contains(msg, "fake: fake:") || !strings.Contains(msg, "fake: bad output") {
		t.Errorf("stderr %q: want one tool prefix", msg)
	}
}

// escapeOut prints a final message with a title write, a clipboard write
// and a screen clear.
const escapeOut = `printf 'RESULT: a\033]0;title\007b\033]52;c;ZXZpbA==\033\\c\033[2Jd\n'`

func TestTerminalStdoutStripsEscapes(t *testing.T) {
	for _, term := range []bool{true, false} {
		f := newFake(t, escapeOut, "", adapter.Verification{})
		f.d.StdoutIsTerminal = term
		if code := run(t, f.d, "--write", "fake", "m", "hi"); code != 0 {
			t.Fatalf("terminal=%v: exit %d; stderr %q", term, code, f.err.String())
		}
		raw := "RESULT: a\x1b]0;title\x07b\x1b]52;c;ZXZpbA==\x1b\\c\x1b[2Jd"
		want := raw + "\n"
		if term {
			want = "RESULT: abcd\n"
		}
		if f.out.String() != want {
			t.Errorf("terminal=%v: stdout %q, want %q", term, f.out.String(), want)
		}
		reps := f.reports(t)
		if len(reps) != 1 {
			t.Fatalf("terminal=%v: reports %q", term, reps)
		}
		if data, _ := os.ReadFile(reps[0]); string(data) != raw+"\n" {
			t.Errorf("terminal=%v: report %q, want the exact message", term, data)
		}
	}
}

func TestTerminalFinalLineStripsEscapes(t *testing.T) {
	for _, term := range []bool{true, false} {
		f := newFake(t, escapeOut, "", adapter.Verification{})
		f.d.StdoutIsTerminal = term
		if code := run(t, f.d, "--write", "--final", "^RESULT:", "fake", "m", "hi"); code != 0 {
			t.Fatalf("terminal=%v: exit %d; stderr %q", term, code, f.err.String())
		}
		want := "RESULT: a\x1b]0;title\x07b\x1b]52;c;ZXZpbA==\x1b\\c\x1b[2Jd"
		if term {
			want = "RESULT: abcd"
		}
		if lines := strings.Split(f.out.String(), "\n"); lines[0] != want {
			t.Errorf("terminal=%v: --final line %q, want %q", term, lines[0], want)
		}
	}
}

func TestTerminalStderrTailStripsEscapes(t *testing.T) {
	for _, term := range []bool{true, false} {
		f := newFake(t, `printf 'bad \033]0;title\007key\033[2J\n' >&2; exit 1`, "", adapter.Verification{})
		f.d.StderrIsTerminal = term
		if code := run(t, f.d, "--write", "--verbose", "fake", "m", "hi"); code != 1 {
			t.Fatalf("terminal=%v: exit %d", term, code)
		}
		want := "bad \x1b]0;title\x07key\x1b[2J\n"
		if term {
			want = "bad key\n"
		}
		if !strings.Contains(f.err.String(), "stderr:\n"+want) {
			t.Errorf("terminal=%v: stderr %q, want the tail %q", term, f.err.String(), want)
		}
	}
}

func TestTerminalReportedModelStripsEscapes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init","model":"example-\u001b]0;title\u0007model\u001b[2J"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"answer"}'` + "\n"
	if err := os.WriteFile(src, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	testenv.Isolate(t, src)
	for _, term := range []bool{true, false} {
		var out, errb bytes.Buffer
		d := Deps{
			Stdin:            strings.NewReader(""),
			StdinIsTerminal:  true,
			Stdout:           &out,
			Stderr:           &errb,
			StderrIsTerminal: term,
			Env:              os.Getenv,
			Pid:              4242,
			Adapters:         map[string]adapter.Adapter{"claude": adapter.NewClaude()},
			Signals:          make(chan os.Signal),
			Argv0:            "ask",
		}
		if code := run(t, d, "--write", "--verbose", "--no-report", "claude", "m", "hi"); code != 0 {
			t.Fatalf("terminal=%v: exit %d; stderr %q", term, code, errb.String())
		}
		want := "reported_model=example-\x1b]0;title\x07model\x1b[2J "
		if term {
			want = "reported_model=example-model "
		}
		if !strings.Contains(errb.String(), want) {
			t.Errorf("terminal=%v: stderr %q lacks %q", term, errb.String(), want)
		}
	}
}

// splitSecret prints the secret value from secretsCfg with an escape
// sequence inside it.
const splitSecret = `printf 'RESULT: key example-sec\033[0mret-value\n'`

func TestTerminalRedactsSecretSplitByEscape(t *testing.T) {
	const value = "example-secret-value"
	for _, extra := range [][]string{nil, {"--final", "^RESULT:"}} {
		f := newFake(t, splitSecret, secretsCfg, adapter.Verification{})
		writeSecrets(t, f.home, value)
		f.d.StdoutIsTerminal = true
		args := append(append([]string{"--write"}, extra...), "fake", "m", "hi")
		if code := run(t, f.d, args...); code != 0 {
			t.Fatalf("%v: exit %d; stderr %q", extra, code, f.err.String())
		}
		if got := f.out.String(); strings.Contains(got, value) || !strings.HasPrefix(got, "RESULT: key <redacted:api_key>\n") {
			t.Errorf("%v: stdout %q, want the secret redacted", extra, got)
		}
	}

	g := newFake(t, splitSecret+" >&2; exit 1", secretsCfg, adapter.Verification{})
	writeSecrets(t, g.home, value)
	g.d.StderrIsTerminal = true
	if code := run(t, g.d, "--write", "--verbose", "fake", "m", "hi"); code != 1 {
		t.Fatalf("stderr tail: exit %d", code)
	}
	if got := g.err.String(); strings.Contains(got, value) || !strings.Contains(got, "RESULT: key <redacted:api_key>\n") {
		t.Errorf("stderr tail %q, want the secret redacted", got)
	}
}

func TestTerminalStderrTailHeaderCountsShownBytes(t *testing.T) {
	script := `i=0; while [ $i -lt 600 ]; do printf '\033[2Jxxxxxxxx\n' >&2; i=$((i+1)); done
exit 1`
	f := newFake(t, script, "", adapter.Verification{})
	f.d.StderrIsTerminal = true
	if code := run(t, f.d, "--write", "--verbose", "fake", "m", "hi"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	msg := f.err.String()
	const head = "ask: last "
	start := strings.Index(msg, head)
	if start < 0 {
		t.Fatalf("no cut header in %q", msg)
	}
	var n int
	if _, err := fmt.Sscanf(msg[start+len(head):], "%d", &n); err != nil {
		t.Fatal(err)
	}
	body := msg[start:]
	body = body[strings.IndexByte(body, '\n')+1:]
	if end := strings.Index(body, "ask: tool="); end >= 0 {
		body = body[:end]
	}
	if len(body) != n || strings.Contains(body, "\x1b") {
		t.Errorf("header says %d bytes, printed %d: %q", n, len(body), body)
	}
}

// writeSecrets writes value as both secrets that secretsCfg reads.
func writeSecrets(t *testing.T, home, value string) {
	t.Helper()
	for _, name := range []string{"api-secret", "ro-secret"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
