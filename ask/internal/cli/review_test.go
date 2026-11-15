package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
