package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

var noLimits = Limits{MaxBytes: 1 << 30, MaxFiles: 100000}

// tree builds a small source folder and points TMPDIR at a fresh folder,
// so tests can see what Copy leaves behind.
func tree(t *testing.T) (src, tmp string) {
	t.Helper()
	src = t.TempDir()
	tmp = t.TempDir()
	t.Setenv("TMPDIR", tmp)
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "alpha\n", 0o644)
	write("sub/b.go", "package b\n", 0o644)
	write("run.sh", "#!/bin/sh\n", 0o755)
	write(".git/HEAD", "ref: refs/heads/main\n", 0o644)
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	return src, tmp
}

func leftovers(t *testing.T, tmp string) []string {
	t.Helper()
	ents, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestCopyMatchesSource(t *testing.T) {
	src, tmp := tree(t)
	dir, cleanup, err := Copy(src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, tmp) || filepath.Base(dir) != filepath.Base(src) {
		t.Errorf("copy at %q, want %s/.../%s", dir, tmp, filepath.Base(src))
	}
	root := filepath.Dir(dir)
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("copy root mode %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	a, err := Fingerprint(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ch := Changed(a, b); len(ch) != 0 {
		t.Errorf("copy differs from source: %v", ch)
	}
	for _, rel := range []string{"a.txt", "sub/b.go", "run.sh", ".git/HEAD", "empty"} {
		if _, ok := b[rel]; !ok {
			t.Errorf("fingerprint lacks %s", rel)
		}
	}
	if !strings.HasPrefix(b["run.sh"], "file:755:") || !strings.HasPrefix(b["empty"], "dir:750") {
		t.Errorf("modes not kept: run.sh=%q empty=%q", b["run.sh"], b["empty"])
	}
	cleanup()
	if left := leftovers(t, tmp); len(left) != 0 {
		t.Errorf("cleanup left %v", left)
	}
}

func TestCopyKeepsSymlinks(t *testing.T) {
	src, _ := tree(t)
	if err := os.Symlink("/etc", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	target, err := os.Readlink(filepath.Join(dir, "link"))
	if err != nil || target != "/etc" {
		t.Fatalf("copied link = %q, %v; want a link to /etc", target, err)
	}
	fp, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp["link"] != "link:/etc" {
		t.Errorf("fingerprint[link] = %q", fp["link"])
	}
	for rel := range fp {
		if strings.HasPrefix(rel, "link/") {
			t.Errorf("fingerprint followed the link: %s", rel)
		}
	}
}

func TestCopyLimits(t *testing.T) {
	src, tmp := tree(t)
	if err := os.WriteFile(filepath.Join(src, "big"), make([]byte, 11), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Copy(src, Limits{MaxBytes: 10, MaxFiles: 100})
	if err == nil || !strings.Contains(err.Error(), "10") {
		t.Errorf("byte limit: err = %v, want one naming 10", err)
	}
	_, _, err = Copy(src, Limits{MaxBytes: 1 << 20, MaxFiles: 2})
	if err == nil || !strings.Contains(err.Error(), "2") {
		t.Errorf("file limit: err = %v, want one naming 2", err)
	}
	if left := leftovers(t, tmp); len(left) != 0 {
		t.Errorf("a failed copy left %v", left)
	}
}

func TestChangedFindsEdits(t *testing.T) {
	src, _ := tree(t)
	dir, cleanup, err := Copy(src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	before, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := Fingerprint(dir)
	if ch := Changed(before, same); len(ch) != 0 {
		t.Fatalf("no edits, but Changed = %v", ch)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("ALPHA\n"), 0o644))
	must(os.Remove(filepath.Join(dir, "sub/b.go")))
	must(os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644))
	must(os.Chmod(filepath.Join(dir, "run.sh"), 0o644))
	after, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := Changed(before, after)
	want := []string{"a.txt", "new.txt", "run.sh", "sub/b.go"}
	if !slices.Equal(got, want) {
		t.Errorf("Changed = %v, want %v", got, want)
	}
}

func TestCopySkipsFifo(t *testing.T) {
	src, _ := tree(t)
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var dir string
	var cleanup func()
	go func() {
		var err error
		dir, cleanup, err = Copy(src, noLimits)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Copy hung on a FIFO")
	}
	defer cleanup()
	if _, err := os.Lstat(filepath.Join(dir, "pipe")); !os.IsNotExist(err) {
		t.Errorf("the FIFO was copied: %v", err)
	}
	// A FIFO a reviewer makes in the copy shows in the fingerprint
	// without blocking it.
	if err := syscall.Mkfifo(filepath.Join(dir, "made"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp["made"] != "other" {
		t.Errorf("fingerprint[made] = %q, want other", fp["made"])
	}
}
