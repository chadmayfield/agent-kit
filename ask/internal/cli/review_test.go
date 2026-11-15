package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
)

// newReview sets up claude (verified read-only), codex and agy (both
// unverified) as fake tools. Each runs the shell body given for it; the
// body sees the prompt on stdin and runs in the review's working folder.
func newReview(t *testing.T, bodies map[string]string) (*fakeRun, string) {
	t.Helper()
	f := newFake(t, `echo unused`, "", verified)
	// PATH holds only the test's bin folder, so tool scripts call
	// /bin/cat and /bin/sleep by full path.
	f.d.Adapters = map[string]adapter.Adapter{
		"claude": fakeAdapter{ro: verified},
		"codex":  fakeAdapter{},
		"agy":    fakeAdapter{},
	}
	tools := map[string]string{}
	for tool, body := range bodies {
		p := filepath.Join(f.home, "bin", "fake-"+tool)
		script := "#!/bin/sh\necho " + tool + " >> \"$HOME/ran\"\n" + body + "\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		tools[tool] = fmt.Sprintf(`%q: {"binary": %q}`, tool, p)
	}
	var parts []string
	for _, tool := range []string{"claude", "codex", "agy"} {
		if s, ok := tools[tool]; ok {
			parts = append(parts, s)
		}
	}
	writeConfig(t, `{"tools": {`+strings.Join(parts, ", ")+`}}`)
	project := filepath.Join(f.home, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f, project
}

func reviewArgs(project string, extra ...string) []string {
	return append([]string{"review", "-C", project, "--claude", "opus", "--codex", "sol", "--agy", "flash"}, extra...)
}

func TestReviewRunsAllInParallel(t *testing.T) {
	body := "/bin/cat >/dev/null; /bin/sleep 1; echo answer-%s"
	f, project := newReview(t, map[string]string{
		"claude": fmt.Sprintf(body, "claude"),
		"codex":  fmt.Sprintf(body, "codex"),
		"agy":    fmt.Sprintf(body, "agy"),
	})
	start := time.Now()
	if code := run(t, f.d, reviewArgs(project, "check this")...); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("took %v; the three reviews did not run at once", took)
	}
	out := f.out.String()
	ic, ix, ia := strings.Index(out, "== claude · opus =="), strings.Index(out, "== codex · sol =="), strings.Index(out, "== agy · flash ==")
	if ic < 0 || ix < ic || ia < ix {
		t.Errorf("blocks missing or out of order:\n%s", out)
	}
	for _, want := range []string{"answer-claude", "answer-codex", "answer-agy", "read-only", "copy", "report:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestReviewReadOnlyVsCopy(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"claude": "/bin/cat >/dev/null; pwd",
		"codex":  "/bin/cat >/dev/null; pwd",
	})
	if code := run(t, f.d, "review", "--json", "-C", project, "--claude", "opus", "--codex", "sol", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	var res struct {
		Reviews []reviewResult `json:"reviews"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, f.out.String())
	}
	if len(res.Reviews) != 2 {
		t.Fatalf("reviews = %+v", res.Reviews)
	}
	cl, cx := res.Reviews[0], res.Reviews[1]
	real, _ := filepath.EvalSymlinks(project)
	if cl.Mode != "read-only" || strings.TrimSpace(cl.Final) != real && strings.TrimSpace(cl.Final) != project {
		t.Errorf("claude = %+v; want read-only in %s", cl, project)
	}
	if cx.Mode != "copy" || strings.TrimSpace(cx.Final) == project || !strings.HasSuffix(strings.TrimSpace(cx.Final), "/project") {
		t.Errorf("codex = %+v; want a copy named project, not the real folder", cx)
	}
}

func TestReviewDetectsEdits(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": "/bin/cat >/dev/null; echo hacked > main.go; echo done",
	})
	code := run(t, f.d, "review", "-C", project, "--codex", "sol", "hi")
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, f.err.String())
	}
	out := f.out.String() + f.err.String()
	if !strings.Contains(out, "changed files") || !strings.Contains(out, "main.go") {
		t.Errorf("no changed-files report:\n%s", out)
	}
	data, _ := os.ReadFile(filepath.Join(project, "main.go"))
	if string(data) != "package main\n" {
		t.Errorf("the real folder changed: main.go = %q", data)
	}
}

func TestReviewPartialFailure(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"claude": "/bin/cat >/dev/null; echo answer-claude",
		"agy":    "/bin/cat >/dev/null; echo answer-agy",
	})
	// codex has no config entry, so its binary is looked up as "fake"
	// on PATH, which is the newFake binary; point it at nothing instead.
	writeConfig(t, fmt.Sprintf(`{"tools": {"claude": {"binary": %q}, "codex": {"binary": "/nonexistent/codex"}, "agy": {"binary": %q}}}`,
		filepath.Join(f.home, "bin", "fake-claude"), filepath.Join(f.home, "bin", "fake-agy")))
	code := run(t, f.d, reviewArgs(project, "hi")...)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, f.err.String())
	}
	out := f.out.String()
	for _, want := range []string{"answer-claude", "answer-agy", "== codex · sol =="} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestReviewSignalStopsAll(t *testing.T) {
	body := "/bin/cat >/dev/null; /bin/sleep 30; echo late"
	f, project := newReview(t, map[string]string{"claude": body, "codex": body, "agy": body})
	sig := make(chan os.Signal, 1)
	f.d.Signals = sig
	go func() {
		time.Sleep(500 * time.Millisecond)
		sig <- syscall.SIGINT
	}()
	start := time.Now()
	code := run(t, f.d, reviewArgs(project, "hi")...)
	if code != exitSignalBase+sigintNumber {
		t.Fatalf("exit %d, want %d", code, exitSignalBase+sigintNumber)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("took %v to stop", took)
	}
	if strings.Contains(f.out.String(), "late") {
		t.Error("a review finished after the signal")
	}
	tmp := os.TempDir()
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "ask-review-") {
			t.Errorf("copy folder left behind: %s", filepath.Join(tmp, e.Name()))
		}
	}
}

func TestReviewJSON(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"claude": "/bin/cat >/dev/null; echo answer-claude",
		"agy":    "/bin/cat >/dev/null; echo answer-agy",
	})
	if code := run(t, f.d, "review", "--json", "-C", project, "--claude", "opus", "--agy", "flash", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	var res struct {
		Reviews []reviewResult `json:"reviews"`
		Report  string         `json:"report"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, f.out.String())
	}
	if len(res.Reviews) != 2 || res.Reviews[0].Tool != "claude" || res.Reviews[1].Tool != "agy" {
		t.Fatalf("reviews = %+v", res.Reviews)
	}
	r := res.Reviews[0]
	if r.Model != "opus" || r.Final != "answer-claude" || r.Exit != 0 || r.Report == "" || r.InputTokens != 11 {
		t.Errorf("claude result = %+v", r)
	}
	if res.Report == "" {
		t.Error("no combined report")
	} else if data, err := os.ReadFile(res.Report); err != nil || !strings.Contains(string(data), "answer-agy") {
		t.Errorf("combined report: %v, %q", err, data)
	}
}

func TestReviewPresetPrefix(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"claude": `/bin/cat > "$HOME/prompt-claude"; echo ok`,
		"codex":  `/bin/cat > "$HOME/prompt-codex"; echo ok`,
	})
	if code := run(t, f.d, "review", "-C", project, "--claude", "opus", "--codex", "sol", "--as", "adversarial", promptMarker); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	for _, tool := range []string{"claude", "codex"} {
		got, err := os.ReadFile(filepath.Join(f.home, "prompt-"+tool))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(got), reviewPresets["adversarial"]) || !strings.Contains(string(got), promptMarker) {
			t.Errorf("%s got prompt %q", tool, got)
		}
	}
	if strings.Contains(f.logText(t), promptMarker) {
		t.Error("the prompt reached the log")
	}
}

func TestReviewNeedsAPick(t *testing.T) {
	f, project := newReview(t, map[string]string{"claude": "echo ok"})
	if code := run(t, f.d, "review", "-C", project, "hi"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestReviewCopyUsesReadOnlySetup(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; echo "$@" > "$HOME/args-codex"; echo "ro=$EXAMPLE_RO_TOKEN api=$EXAMPLE_API_KEY" > "$HOME/env-codex"; echo ok`,
	})
	// Only the read-only secret has a file. A run that asked for the
	// write secret too would fail its lookup with exit 3.
	writeConfig(t, fmt.Sprintf(`{"tools": {"codex": {"binary": %q,
	  "secrets": [
	    {"name": "ro_key", "env_var": "EXAMPLE_RO_TOKEN", "ref": "file:%s/ro-secret"},
	    {"name": "api_key", "env_var": "EXAMPLE_API_KEY", "ref": "file:%s/api-secret"}
	  ],
	  "read_only_secrets": ["ro_key"]}}}`, filepath.Join(f.home, "bin", "fake-codex"), f.home, f.home))
	if err := os.WriteFile(filepath.Join(f.home, "ro-secret"), []byte("ro-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "review", "-C", project, "--codex", "sol", "hi"); code != 0 {
		t.Fatalf("exit %d; stdout %q stderr %q", code, f.out.String(), f.err.String())
	}
	if args, _ := os.ReadFile(filepath.Join(f.home, "args-codex")); strings.Contains(string(args), "--write") {
		t.Errorf("codex ran with --write in the copy: %q", args)
	}
	if env, _ := os.ReadFile(filepath.Join(f.home, "env-codex")); strings.TrimSpace(string(env)) != "ro=ro-value api=" {
		t.Errorf("codex environment = %q; want only the read-only secret", env)
	}
	if !strings.Contains(f.out.String(), "copy") {
		t.Errorf("summary does not say it ran in a copy:\n%s", f.out.String())
	}
}

func TestReviewRedactsSplitSecret(t *testing.T) {
	const value = "example-secret-value"
	f, project := newReview(t, map[string]string{
		"claude": `/bin/cat >/dev/null; printf 'RESULT: key example-sec\033[0mret-value\n'`,
	})
	writeConfig(t, fmt.Sprintf(`{"tools": {"claude": {"binary": %q,
	  "secrets": [{"name": "ro_key", "env_var": "EXAMPLE_RO_TOKEN", "ref": "file:%s/ro-secret"}],
	  "read_only_secrets": ["ro_key"]}}}`, filepath.Join(f.home, "bin", "fake-claude"), f.home))
	writeSecrets(t, f.home, value)
	f.d.StdoutIsTerminal = true
	if code := run(t, f.d, "review", "-C", project, "--claude", "opus", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if got := f.out.String(); strings.Contains(got, value) || !strings.Contains(got, "RESULT: key <redacted:ro_key>") {
		t.Errorf("stdout %q, want the secret redacted", got)
	}
}

func TestReviewSymlinkedFolder(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": "/bin/cat >/dev/null; /bin/ls",
	})
	link := filepath.Join(f.home, "link-to-project")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "review", "-C", link, "--codex", "sol", "hi"); code != 0 {
		t.Fatalf("exit %d; stdout %q stderr %q", code, f.out.String(), f.err.String())
	}
	if !strings.Contains(f.out.String(), "main.go") {
		t.Errorf("the copy of a symlinked folder lacks its files:\n%s", f.out.String())
	}
}

func TestReviewSignalKeepsFinished(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"claude": "/bin/cat >/dev/null; echo answer-claude",
		"codex":  "/bin/cat >/dev/null; /bin/sleep 30; echo late",
	})
	sig := make(chan os.Signal, 1)
	f.d.Signals = sig
	go func() {
		time.Sleep(700 * time.Millisecond)
		sig <- syscall.SIGINT
	}()
	code := run(t, f.d, "review", "-C", project, "--claude", "opus", "--codex", "sol", "hi")
	if code != exitSignalBase+sigintNumber {
		t.Fatalf("exit %d, want %d", code, exitSignalBase+sigintNumber)
	}
	out := f.out.String()
	if !strings.Contains(out, "answer-claude") || strings.Contains(out, "late") {
		t.Errorf("want claude's finished answer and no late codex answer:\n%s", out)
	}
	if !strings.Contains(out, "report:") {
		t.Errorf("no combined report after the signal:\n%s", out)
	}
}

func TestReviewCopyErrorExitCodes(t *testing.T) {
	f, project := newReview(t, map[string]string{"codex": "/bin/cat >/dev/null; echo ok"})
	old := reviewCopyLimits
	t.Cleanup(func() { reviewCopyLimits = old })

	reviewCopyLimits.MaxFiles = 0
	if code := run(t, f.d, "review", "--json", "-C", project, "--codex", "sol", "hi"); code != 1 {
		t.Fatalf("too large: exit %d", code)
	}
	var res struct {
		Reviews []reviewResult `json:"reviews"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if r := res.Reviews[0]; r.Exit != ExitUsage || strings.Contains(r.Error, project) {
		t.Errorf("too large: %+v; want exit 2 without the absolute path", r)
	}

	reviewCopyLimits = old
	if os.Geteuid() != 0 {
		locked := filepath.Join(project, "private.txt")
		if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		f.out.Reset()
		if code := run(t, f.d, "review", "--json", "-C", project, "--codex", "sol", "hi"); code != 1 {
			t.Fatalf("unreadable file: exit %d", code)
		}
		if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if r := res.Reviews[0]; r.Exit != ExitSetup || strings.Contains(r.Error, project) || !strings.Contains(r.Error, "private.txt") {
			t.Errorf("unreadable file: %+v; want exit 3 naming private.txt without the absolute path", r)
		}
	}
}

func TestReviewShowsToolWarnings(t *testing.T) {
	f, project := newReview(t, map[string]string{"claude": "/bin/cat >/dev/null; echo ok"})
	// A folder where the log file should be makes the log append fail,
	// which the run reports as a warning on its stderr.
	logPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "log.jsonl")
	if err := os.MkdirAll(logPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "review", "-C", project, "--claude", "opus", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if !strings.Contains(f.err.String(), "claude: ask: warning: log") {
		t.Errorf("the tool's warning was not shown: stderr %q", f.err.String())
	}
}

func TestReviewCopySetsGitCeiling(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; echo "ceiling=$GIT_CEILING_DIRECTORIES pwd=$(pwd)"`,
	})
	if code := run(t, f.d, "review", "-C", project, "--codex", "sol", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	out := f.out.String()
	i := strings.Index(out, "ceiling=")
	if i < 0 {
		t.Fatalf("no output:\n%s", out)
	}
	fields := strings.Fields(out[i:])
	ceiling := strings.TrimPrefix(fields[0], "ceiling=")
	pwd := strings.TrimPrefix(fields[1], "pwd=")
	if ceiling == "" || filepath.Dir(pwd) != ceiling {
		t.Errorf("GIT_CEILING_DIRECTORIES = %q, want the copy's parent of %q", ceiling, pwd)
	}
}

// roSecretConfig gives the named tool one read-only secret, ro_key.
func roSecretConfig(t *testing.T, f *fakeRun, tool string) {
	t.Helper()
	writeConfig(t, fmt.Sprintf(`{"tools": {%q: {"binary": %q,
	  "secrets": [{"name": "ro_key", "env_var": "EXAMPLE_RO_TOKEN", "ref": "file:%s/ro-secret"}],
	  "read_only_secrets": ["ro_key"]}}}`, tool, filepath.Join(f.home, "bin", "fake-"+tool), f.home))
}

func TestReviewRedactsChangedFileNames(t *testing.T) {
	const value = "example-secret-value"
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; : > "$EXAMPLE_RO_TOKEN"; echo ok`,
	})
	roSecretConfig(t, f, "codex")
	writeSecrets(t, f.home, value)
	for _, args := range [][]string{{"review", "-C", project, "--codex", "sol", "hi"}, {"review", "--json", "-C", project, "--codex", "sol", "hi"}} {
		f.out.Reset()
		if code := run(t, f.d, args...); code != 1 {
			t.Fatalf("%v: exit %d, want 1 (changed files)", args, code)
		}
		if strings.Contains(f.out.String(), value) {
			t.Errorf("%v: the secret leaked as a file name:\n%s", args, f.out.String())
		}
		if !strings.Contains(f.out.String(), "redacted:ro_key") {
			t.Errorf("%v: no redaction marker:\n%s", args, f.out.String())
		}
	}
	reports, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "reports", "*review*"))
	for _, r := range reports {
		if data, _ := os.ReadFile(r); strings.Contains(string(data), value) {
			t.Errorf("the combined report %s holds the secret", filepath.Base(r))
		}
	}
}

func TestReviewCopyIgnoresGlobalGitConfig(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; echo "global=$GIT_CONFIG_GLOBAL nosystem=$GIT_CONFIG_NOSYSTEM"`,
	})
	if code := run(t, f.d, "review", "-C", project, "--codex", "sol", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "global=/dev/null nosystem=1") {
		t.Errorf("git global and system config are not turned off:\n%s", f.out.String())
	}
}

func TestReviewShowsToolStderrOnFailure(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; echo "authentication expired" >&2; exit 1`,
	})
	if code := run(t, f.d, "review", "-C", project, "--codex", "sol", "hi"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "codex: ") || !strings.Contains(f.err.String(), "authentication expired") {
		t.Errorf("the tool's stderr was not shown: %q", f.err.String())
	}
}

func TestReviewCleanupWarningShown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses chflags uchg to make cleanup fail")
	}
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; /usr/bin/chflags uchg main.go; echo ok`,
	})
	t.Cleanup(func() {
		ents, _ := os.ReadDir(os.TempDir())
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "ask-review-") {
				p := filepath.Join(os.TempDir(), e.Name())
				_ = exec.Command("/usr/bin/chflags", "-R", "nouchg", p).Run()
				_ = os.RemoveAll(p)
			}
		}
	})
	_ = run(t, f.d, "review", "-C", project, "--codex", "sol", "hi")
	if !strings.Contains(f.err.String(), "codex: ask: warning: remove the copy folder") {
		t.Errorf("no cleanup warning: stderr %q", f.err.String())
	}
}

func TestReviewAfterSignalKeepsRunStatus(t *testing.T) {
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; trap 'echo answer; exit 0' TERM INT; /bin/sleep 30 & wait`,
	})
	sig := make(chan os.Signal, 1)
	f.d.Signals = sig
	go func() {
		time.Sleep(700 * time.Millisecond)
		sig <- syscall.SIGINT
	}()
	code := run(t, f.d, "review", "--json", "-C", project, "--codex", "sol", "hi")
	if code != exitSignalBase+sigintNumber {
		t.Fatalf("exit %d", code)
	}
	var res struct {
		Reviews []reviewResult `json:"reviews"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, f.out.String())
	}
	if r := res.Reviews[0]; strings.Contains(r.Error, "canceled") || r.Exit != exitSignalBase+sigintNumber {
		t.Errorf("codex result %+v; want the interrupt, not a cancelled copy check", r)
	}
}

func TestReviewRedactsCleanupWarning(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses chflags uchg to make cleanup fail")
	}
	const value = "example-secret-value"
	f, project := newReview(t, map[string]string{
		"codex": `/bin/cat >/dev/null; : > "$EXAMPLE_RO_TOKEN"; /usr/bin/chflags uchg "$EXAMPLE_RO_TOKEN"; echo ok`,
	})
	roSecretConfig(t, f, "codex")
	writeSecrets(t, f.home, value)
	t.Cleanup(func() {
		ents, _ := os.ReadDir(os.TempDir())
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "ask-review-") {
				p := filepath.Join(os.TempDir(), e.Name())
				_ = exec.Command("/usr/bin/chflags", "-R", "nouchg", p).Run()
				_ = os.RemoveAll(p)
			}
		}
	})
	_ = run(t, f.d, "review", "-C", project, "--codex", "sol", "hi")
	if !strings.Contains(f.err.String(), "remove the copy folder") {
		t.Fatalf("cleanup did not fail as set up: stderr %q", f.err.String())
	}
	if strings.Contains(f.err.String(), value) {
		t.Errorf("the cleanup warning leaked the secret: %q", f.err.String())
	}
}
