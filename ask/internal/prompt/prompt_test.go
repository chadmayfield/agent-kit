package prompt

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestBuildJoinsArgs(t *testing.T) {
	got, err := Build([]string{"review", "this", "change"}, nil, strings.NewReader("unused"), false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "review this change"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildInlinesFiles(t *testing.T) {
	dir := t.TempDir()
	b := writeFile(t, dir, "b.go", "package b\n")
	a := writeFile(t, dir, "a.txt", "alpha")

	got, err := Build([]string{"check", "these"}, []string{b, a}, strings.NewReader(""), true)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "check these\n\n--- b.go ---\npackage b\n\n\n--- a.txt ---\nalpha"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Files alone, with no argument text, still make a prompt.
	got, err = Build(nil, []string{a}, strings.NewReader(""), true)
	if err != nil {
		t.Fatalf("Build files only: %v", err)
	}
	if want := "--- a.txt ---\nalpha"; got != want {
		t.Fatalf("files only: got %q, want %q", got, want)
	}
}

func TestBuildReadsStdinWithDash(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "notes.md", "note")

	got, err := Build([]string{"summarize", "-"}, []string{f}, strings.NewReader("piped text"), false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "summarize\n\npiped text\n\n--- notes.md ---\nnote"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A lone dash with only stdin.
	got, err = Build([]string{"-"}, nil, strings.NewReader("only stdin"), false)
	if err != nil {
		t.Fatalf("Build stdin only: %v", err)
	}
	if want := "only stdin"; got != want {
		t.Fatalf("stdin only: got %q, want %q", got, want)
	}

	// Two dashes is an error.
	if _, err := Build([]string{"-", "-"}, nil, strings.NewReader("x"), false); err == nil {
		t.Fatal("two dashes: want error, got nil")
	}
}

func TestBuildIgnoresStdinWithoutDash(t *testing.T) {
	// A pipe that is never written and never closed: any read blocks forever.
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })

	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := Build([]string{"hi"}, nil, r, false)
		done <- result{s, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Build: %v", res.err)
		}
		if res.s != "hi" {
			t.Fatalf("got %q, want %q", res.s, "hi")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Build read stdin without a dash argument")
	}
}

func TestBuildDashWithTerminalIsError(t *testing.T) {
	_, err := Build([]string{"-"}, nil, strings.NewReader("x"), true)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), "—") {
		t.Fatalf("error text has an em dash: %q", err)
	}
}

func TestBuildEmptyIsError(t *testing.T) {
	cases := map[string]struct {
		args  []string
		stdin string
	}{
		"no args":     {nil, ""},
		"blank args":  {[]string{"", "  "}, ""},
		"empty stdin": {[]string{"-"}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Build(c.args, nil, strings.NewReader(c.stdin), false)
			if err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestBuildMissingFileIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.txt")
	_, err := Build([]string{"hi"}, []string{missing}, strings.NewReader(""), true)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("error should name the file: %q", err)
	}
}
