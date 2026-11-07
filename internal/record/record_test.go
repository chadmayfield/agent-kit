package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 7, 15, 30, 0, 0, time.UTC)

func TestReportSameSecondSamePidUnique(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	seen := map[string]bool{}
	for i, final := range []string{"first", "second", "third"} {
		p, err := SaveReport(dir, testNow, "codex", 4242, final)
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		if seen[p] {
			t.Fatalf("save %d reused path %s", i, p)
		}
		seen[p] = true
		data, err := os.ReadFile(p)
		if err != nil || strings.TrimSpace(string(data)) != final {
			t.Fatalf("report %s holds %q (err %v), want %q", p, data, err, final)
		}
	}
	want := []string{
		"20261107T153000Z-codex-4242-1.md",
		"20261107T153000Z-codex-4242-2.md",
		"20261107T153000Z-codex-4242-3.md",
	}
	var got []string
	for p := range seen {
		got = append(got, filepath.Base(p))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("names %q, want %q", got, want)
	}
	// The first report still holds its own text.
	data, _ := os.ReadFile(filepath.Join(dir, want[0]))
	if strings.TrimSpace(string(data)) != "first" {
		t.Fatalf("first report was overwritten: %q", data)
	}
}

func TestReportPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "reports")
	p, err := SaveReport(dir, testNow, "codex", 1, "hello")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("report mode %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
	di, err := os.Stat(dir)
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v (err %v), want 0700", di.Mode().Perm(), err)
	}

	// An existing folder with wider permissions is tightened.
	wide := filepath.Join(t.TempDir(), "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveReport(wide, testNow, "codex", 1, "hello"); err != nil {
		t.Fatal(err)
	}
	if di, _ := os.Stat(wide); di.Mode().Perm() != 0o700 {
		t.Fatalf("existing dir mode %v, want 0700", di.Mode().Perm())
	}
}

func TestReportRefusesSymlinkFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "20261107T153000Z-codex-7-1.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveReport(dir, testNow, "codex", 7, "new text"); err == nil {
		t.Fatal("SaveReport wrote past a symlinked report path")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("symlink target changed: %q", data)
	}
}

func TestReportRefusesSymlinkDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "reports")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveReport(link, testNow, "codex", 7, "text"); err == nil {
		t.Fatal("SaveReport wrote into a symlinked folder")
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("symlink target holds files: %v", entries)
	}
	if err := Prune(link, testNow, 30); err == nil {
		t.Fatal("Prune followed a symlinked folder")
	}
}

func TestPruneDeletesOld(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.md")
	recent := filepath.Join(dir, "recent.md")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, testNow.AddDate(0, 0, -31), testNow.AddDate(0, 0, -31)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recent, testNow.AddDate(0, 0, -29), testNow.AddDate(0, 0, -29)); err != nil {
		t.Fatal(err)
	}
	if err := Prune(dir, testNow, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old report still there (err %v)", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent report removed: %v", err)
	}
	// A missing folder is not an error.
	if err := Prune(filepath.Join(dir, "none"), testNow, 30); err != nil {
		t.Errorf("missing folder: %v", err)
	}
}

func TestLogHasNoPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ask", "log.jsonl")
	e := LogEntry{Time: testNow, Tool: "codex", Model: "m", DurationMS: 1500, Exit: 0, InputTokens: 10, OutputTokens: 20}
	for i := 0; i < 2; i++ {
		if err := AppendLog(path, e); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("log has %d lines, want 2:\n%s", len(lines), data)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"duration_ms", "exit", "input_tokens", "model", "model_id", "output_tokens", "reported_model", "time", "tool"}
	if !slices.Equal(keys, want) {
		t.Fatalf("log keys %q, want %q", keys, want)
	}
}

func TestStateDirSymlinkRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "ask")
	if err := os.Symlink(real, state); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveReport(filepath.Join(state, "reports"), testNow, "codex", 1, "x"); err == nil {
		t.Error("SaveReport used a symlinked state folder")
	}
	if err := AppendLog(filepath.Join(state, "log.jsonl"), LogEntry{}); err == nil {
		t.Error("AppendLog used a symlinked state folder")
	}
	if err := Prune(filepath.Join(state, "reports"), testNow, 30); err == nil {
		t.Error("Prune used a symlinked state folder")
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("symlink target holds files: %v", entries)
	}

	// A state folder ask creates is 0700.
	for i, save := range []func(string) error{
		func(s string) error {
			_, err := SaveReport(filepath.Join(s, "reports"), testNow, "codex", 1, "x")
			return err
		},
		func(s string) error { return AppendLog(filepath.Join(s, "log.jsonl"), LogEntry{}) },
	} {
		s := filepath.Join(t.TempDir(), "ask")
		if err := save(s); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(s)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("case %d: state folder mode %v, want 0700", i, fi.Mode().Perm())
		}
	}
}
