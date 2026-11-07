package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/testenv"
)

const aoMarker = "build-host.placeholder.invalid"

// aoIsolate runs testenv.Isolate with a fake CLI named name on PATH and
// TMPDIR pointed inside the temp home.
func aoIsolate(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, src)
	tmp := filepath.Join(home, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	return home
}

func aoArgsContain(args []string, s string) bool {
	for _, a := range args {
		if strings.Contains(a, s) {
			return true
		}
	}
	return false
}

// agyPromptFile returns the prompt file path named in agy's --print message.
func agyPromptFile(t *testing.T, args []string) string {
	t.Helper()
	if len(args) < 2 || args[0] != "--print" {
		t.Fatalf("Args = %q, want --print <message> first", args)
	}
	file, ok := strings.CutPrefix(args[1], "Read the file ")
	if ok {
		file, ok = strings.CutSuffix(file, " and follow the instructions in it.")
	}
	if !ok {
		t.Fatalf("message = %q, want the fixed message around a path", args[1])
	}
	return file
}

func TestAgyNoPromptInArgs(t *testing.T) {
	home := aoIsolate(t, "agy")
	a := NewAgy()
	req := Request{Model: "example-model", Prompt: "check " + aoMarker, Effort: "high", Write: true}
	inv, err := a.Build(req, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	if aoArgsContain(inv.Args, aoMarker) {
		t.Fatalf("prompt found in args: %q", inv.Args)
	}
	if inv.Stdin != "" {
		t.Fatalf("Stdin = %q, want empty", inv.Stdin)
	}
	file := agyPromptFile(t, inv.Args)
	if !testenv.Under(filepath.Join(home, "tmp"), file) || filepath.Base(file) != "prompt.md" {
		t.Fatalf("prompt file %s is not prompt.md under TMPDIR", file)
	}
	want := []string{"--print", "Read the file " + file + " and follow the instructions in it.",
		"--output-format", "json", "--model", "example-model", "--mode", "accept-edits", "--effort", "high"}
	if !slices.Equal(inv.Args, want) {
		t.Fatalf("Args = %q, want %q", inv.Args, want)
	}
	for i, arg := range inv.Args {
		if i != 1 && strings.Contains(arg, string(filepath.Separator)) {
			t.Fatalf("arg %d %q names a path; only the message may", i, arg)
		}
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != req.Prompt {
		t.Fatalf("prompt file = %q, %v, want the prompt", data, err)
	}
	if filepath.Base(inv.Binary) != "agy" {
		t.Fatalf("Binary = %q", inv.Binary)
	}
}

// TestAgyEnabledForWrite checks that the shipped agy adapter builds with
// --write, keeps its prompt file private, and removes it on Cleanup.
func TestAgyEnabledForWrite(t *testing.T) {
	aoIsolate(t, "agy")
	a := NewAgy()
	if !a.Prompt().OK {
		t.Fatal("Prompt().OK is false")
	}
	if a.ReadOnly().OK {
		t.Fatal("ReadOnly().OK is true")
	}
	inv, err := a.Build(Request{Model: "m", Prompt: "p", Write: true}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	file := agyPromptFile(t, inv.Args)
	dir := filepath.Dir(file)
	di, err := os.Stat(dir)
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v, want 700", di.Mode().Perm(), err)
	}
	fi, err := os.Lstat(file)
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("file mode = %v, %v, want regular 0600", fi.Mode(), err)
	}
	if inv.Cleanup == nil {
		t.Fatal("no Cleanup")
	}
	inv.Cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Cleanup left %s: %v", dir, err)
	}
}

func TestOpencodeNoPromptInArgs(t *testing.T) {
	aoIsolate(t, "opencode")
	a := NewOpencode()
	req := Request{Model: "example/model", Prompt: "check " + aoMarker, Write: true}
	inv, err := a.Build(req, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	if aoArgsContain(inv.Args, aoMarker) {
		t.Fatalf("prompt found in args: %q", inv.Args)
	}
	if got := inv.Args[len(inv.Args)-1]; got != "Follow the instructions in the attached file." {
		t.Fatalf("last arg = %q, want the fixed message", got)
	}
	if strings.Contains(inv.Stdin, aoMarker) {
		t.Fatalf("prompt found in stdin, want it in the file")
	}
	i := slices.Index(inv.Args, "-f")
	if i < 0 || i+1 >= len(inv.Args) {
		t.Fatalf("no -f <file> in args: %q", inv.Args)
	}
	data, err := os.ReadFile(inv.Args[i+1])
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	if string(data) != req.Prompt {
		t.Fatalf("prompt file = %q, want %q", data, req.Prompt)
	}
}

func TestRefusedUntilPromptVerified(t *testing.T) {
	home := aoIsolate(t, "agy")
	for _, a := range []Adapter{newAgy(false), newOpencode(false)} {
		if a.Prompt().OK {
			t.Fatalf("%s: Prompt().OK is true before a live check verifies it", a.Name())
		}
		if a.ReadOnly().OK {
			t.Fatalf("%s: ReadOnly().OK is true", a.Name())
		}
		for _, write := range []bool{false, true} {
			inv, err := a.Build(Request{Model: "m", Prompt: "p", Write: write}, config.ToolConfig{})
			var ref *RefusedError
			if !errors.As(err, &ref) {
				t.Fatalf("%s write=%v: err = %v, want *RefusedError", a.Name(), write, err)
			}
			want := a.Name() + " prompt input not verified"
			if ref.Msg != want {
				t.Fatalf("%s: msg = %q, want %q", a.Name(), ref.Msg, want)
			}
			if inv.Cleanup != nil {
				t.Fatalf("%s: refused Build returned a Cleanup", a.Name())
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("refused Build left temp files: %v %v", entries, err)
	}
}

// TestOpencodeEnabledForWrite checks that the shipped opencode adapter
// builds with --write and refuses without it.
func TestOpencodeEnabledForWrite(t *testing.T) {
	home := aoIsolate(t, "opencode")
	a := NewOpencode()
	if !a.Prompt().OK {
		t.Fatal("Prompt().OK is false")
	}
	if a.ReadOnly().OK {
		t.Fatal("ReadOnly().OK is true")
	}
	inv, err := a.Build(Request{Model: "m", Prompt: "p", Write: true}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("write Build: %v", err)
	}
	inv.Cleanup()
	inv, err = a.Build(Request{Model: "m", Prompt: "p"}, config.ToolConfig{})
	var ref *RefusedError
	if !errors.As(err, &ref) || ref.Msg != "opencode has no read-only setup" {
		t.Fatalf("read-only Build err = %v, want the read-only refusal", err)
	}
	if inv.Cleanup != nil {
		t.Fatal("refused Build returned a Cleanup")
	}
	entries, err := os.ReadDir(filepath.Join(home, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temp files left: %v %v", entries, err)
	}
}

func TestOpencodeUsesStandalone(t *testing.T) {
	aoIsolate(t, "opencode")
	inv, err := NewOpencode().Build(Request{Model: "example/model", Prompt: "p", Write: true}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	if len(inv.Args) < 2 || inv.Args[0] != "run" || inv.Args[1] != "--standalone" {
		t.Fatalf("Args = %q, want run --standalone first", inv.Args)
	}
	want := []string{"run", "--standalone", "--format", "json", "-m", "example/model", "--auto", "-f"}
	if len(inv.Args) != len(want)+2 || !slices.Equal(inv.Args[:len(want)], want) ||
		inv.Args[len(want)+1] != "Follow the instructions in the attached file." {
		t.Fatalf("Args = %q, want %q <file> <message>", inv.Args, want)
	}
}

func TestOpencodePromptFileIsPrivate(t *testing.T) {
	home := aoIsolate(t, "opencode")
	inv, err := newOpencode(true).Build(Request{Model: "example/model", Prompt: "p", Write: true}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	i := slices.Index(inv.Args, "-f")
	if i < 0 || i+1 >= len(inv.Args) {
		t.Fatalf("no -f <file> in args: %q", inv.Args)
	}
	file := inv.Args[i+1]
	dir := filepath.Dir(file)
	if !testenv.Under(filepath.Join(home, "tmp"), dir) {
		t.Fatalf("temp dir %s is not under TMPDIR", dir)
	}
	if filepath.Base(file) != "prompt.md" {
		t.Fatalf("file = %s, want prompt.md", file)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
	fi, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("file mode = %v, want regular 0600", fi.Mode())
	}
	inv.Cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Cleanup left %s: %v", dir, err)
	}
}

func TestOpencodeRefusesEffort(t *testing.T) {
	home := aoIsolate(t, "opencode")
	_, err := newOpencode(true).Build(Request{Model: "m", Prompt: "p", Effort: "high", Write: true}, config.ToolConfig{})
	var ref *RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("err = %v, want *RefusedError", err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, "tmp"))
	if len(entries) != 0 {
		t.Fatalf("refused Build left temp files: %v", entries)
	}
}

func TestAgyRefusesUnknownEffort(t *testing.T) {
	home := aoIsolate(t, "agy")
	_, err := NewAgy().Build(Request{Model: "m", Prompt: "p", Effort: "huge", Write: true}, config.ToolConfig{})
	var ref *RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("err = %v, want *RefusedError", err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, "tmp"))
	if len(entries) != 0 {
		t.Fatalf("refused Build left temp files: %v", entries)
	}
}

func TestAgyReadOnlyCandidate(t *testing.T) {
	aoIsolate(t, "agy")
	inv, err := NewAgy().Build(Request{Model: "m", Prompt: "p"}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	if !slices.Contains(inv.Args, "--sandbox") || slices.Contains(inv.Args, "accept-edits") {
		t.Fatalf("Args = %q, want --sandbox --mode plan", inv.Args)
	}
	i := slices.Index(inv.Args, "--mode")
	if i < 0 || inv.Args[i+1] != "plan" {
		t.Fatalf("Args = %q, want --mode plan", inv.Args)
	}
}

func TestAgyParse(t *testing.T) {
	a := NewAgy()
	data, err := os.ReadFile(filepath.Join("testdata", "agy-print.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Parse(data)
	if err != nil || r.Final != "Done." || r.InputTokens != 120 || r.OutputTokens != 8 {
		t.Fatalf("Parse(fixture) = %+v, %v", r, err)
	}
	for _, in := range []string{``, "  \n"} {
		r, err := a.Parse([]byte(in))
		if err != nil || r != (Result{}) {
			t.Fatalf("Parse(%q) = %+v, %v, want an empty Result and nil error", in, r, err)
		}
	}
	for _, in := range []string{`not json`, `[1,2]`} {
		_, err := a.Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), "agy") {
			t.Fatalf("Parse(%q) err = %v, want an error naming agy", in, err)
		}
	}
}

// TestAgyParseFailedStatus checks that a status other than SUCCESS gives an
// empty final and an error that never holds the response text.
func TestAgyParseFailedStatus(t *testing.T) {
	const secretText = "RESPONSE-MARKER-9c1e"
	for _, in := range []string{
		`{"status":"ERROR","response":"` + secretText + `","usage":{"input_tokens":5,"output_tokens":1}}`,
		`{"response":"` + secretText + `"}`,
		`{"status":"success","response":"` + secretText + `"}`,
	} {
		r, err := NewAgy().Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), "agy") {
			t.Fatalf("Parse(%q) err = %v, want an error naming agy", in, err)
		}
		if strings.Contains(err.Error(), secretText) || strings.Contains(err.Error(), "ERROR") {
			t.Fatalf("error %q holds CLI output", err)
		}
		if r.Final != "" {
			t.Fatalf("Final = %q, want empty", r.Final)
		}
	}
}

func TestOpencodeParse(t *testing.T) {
	a := NewOpencode()
	data, err := os.ReadFile(filepath.Join("testdata", "opencode-run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Parse(data)
	if err != nil || r != (Result{Final: "Done."}) {
		t.Fatalf("Parse(fixture) = %+v, %v", r, err)
	}
	in := strings.Join([]string{
		`{"type":"step_start","part":{"type":"step-start"}}`,
		`{"type":"text","part":{"type":"text","text":"first"}}`,
		`not json`,
		`{"type":"tool_use","part":{"type":"tool","text":"tool text"}}`,
		`{"type":"step_finish","part":{"type":"step-finish","tokens":{"input":10,"output":3}}}`,
		`{"type":"text","part":{"type":"text","text":"last"}}`,
		`{"type":"text","part":{"type":"text","text":""}}`,
		`{"type":"step_finish","part":{"type":"step-finish","tokens":{"input":20,"output":4}}}`,
	}, "\n")
	r, err = a.Parse([]byte(in))
	if err != nil || r.Final != "last" || r.InputTokens != 30 || r.OutputTokens != 7 {
		t.Fatalf("Parse = %+v, %v", r, err)
	}
	for _, in := range []string{``, `{"type":"step_start"}`, `{"type":"text","part":{"type":"text","text":""}}`, `{"type":"text","text":"top level"}`} {
		r, err := a.Parse([]byte(in))
		if err != nil || r != (Result{}) {
			t.Fatalf("Parse(%q) = %+v, %v, want an empty Result and nil error", in, r, err)
		}
	}
	_, err = a.Parse([]byte("not json\nalso not json"))
	if err == nil || !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("err = %v, want an error naming opencode", err)
	}
}

func TestAgyOpencodeEfforts(t *testing.T) {
	if got := NewAgy().Efforts(); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("agy efforts = %q", got)
	}
	if got := NewOpencode().Efforts(); len(got) != 0 {
		t.Fatalf("opencode efforts = %q", got)
	}
}

func TestAgyMapsModelAlias(t *testing.T) {
	aoIsolate(t, "agy")
	tc := config.ToolConfig{Models: map[string]string{"short": "example/model"}}
	inv, err := NewAgy().Build(Request{Model: "short", Prompt: "p", Write: true}, tc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	i := slices.Index(inv.Args, "--model")
	if i < 0 || inv.Args[i+1] != "example/model" {
		t.Fatalf("Args = %q, want --model example/model", inv.Args)
	}
}

func TestOpencodeMapsModelAlias(t *testing.T) {
	aoIsolate(t, "opencode")
	tc := config.ToolConfig{Models: map[string]string{"short": "example/model"}}
	inv, err := NewOpencode().Build(Request{Model: "short", Prompt: "p", Write: true}, tc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	i := slices.Index(inv.Args, "-m")
	if i < 0 || inv.Args[i+1] != "example/model" {
		t.Fatalf("Args = %q, want -m example/model", inv.Args)
	}
}

// TestNoBuildPlanNamesInSource checks that no Go file in this package
// contains "Task" followed by a number.
func TestNoBuildPlanNamesInSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`Task [0-9]+`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.Find(data); m != nil {
			t.Errorf("%s mentions %q", f, m)
		}
	}
}

func TestAgyParseCountsCacheReadInput(t *testing.T) {
	in := `{"status":"SUCCESS","response":"ok","usage":{"input_tokens":100,"output_tokens":8,"thinking_tokens":30,"cache_read_tokens":40}}`
	r, err := NewAgy().Parse([]byte(in))
	if err != nil || r.InputTokens != 140 || r.OutputTokens != 8 {
		t.Fatalf("Parse = %+v, %v, want 140 in, 8 out", r, err)
	}
}

func TestDryRunPromptInFile(t *testing.T) {
	for _, name := range []string{"agy", "opencode"} {
		aoIsolate(t, name)
		a := map[string]Adapter{"agy": NewAgy(), "opencode": NewOpencode()}[name]
		line, err := DryRun(a, Request{Model: "m", Prompt: "check " + aoMarker, Write: true}, config.ToolConfig{}, nil)
		if err != nil {
			t.Fatalf("%s: DryRun: %v", name, err)
		}
		want := fmt.Sprintf("<prompt: %d bytes, in a temp file>", len("check "+aoMarker))
		if !strings.HasSuffix(line, want) || strings.Contains(line, aoMarker) {
			t.Fatalf("%s: line = %q, want it to end with %q", name, line, want)
		}
	}
}
