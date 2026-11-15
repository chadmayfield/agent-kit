package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
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
	a, err := Fingerprint(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(context.Background(), dir, noLimits)
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
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if left := leftovers(t, tmp); len(left) != 0 {
		t.Errorf("cleanup left %v", left)
	}
}

func TestCopyKeepsOnlyInsideLinks(t *testing.T) {
	src, _ := tree(t)
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Kept: relative links that resolve inside the folder.
	must(os.Symlink("a.txt", filepath.Join(src, "to-a")))
	must(os.Symlink("../a.txt", filepath.Join(src, "sub", "up-a")))
	must(os.Symlink("sub", filepath.Join(src, "to-sub")))
	// Left out: absolute links, links that climb out, links into the
	// source by absolute path, dangling links, and a link whose text
	// stays inside but whose real path leaves through a linked folder.
	must(os.Symlink("/etc", filepath.Join(src, "abs")))
	must(os.Symlink("../"+filepath.Base(outside), filepath.Join(src, "climb")))
	must(os.Symlink(filepath.Join(src, "a.txt"), filepath.Join(src, "abs-inside")))
	must(os.Symlink("missing", filepath.Join(src, "dangling")))
	must(os.MkdirAll(filepath.Join(src, "x", "y"), 0o755))
	must(os.Symlink("../..", filepath.Join(src, "x", "y", "d")))
	must(os.Symlink("d/../../..", filepath.Join(src, "x", "y", "tricky")))

	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	for rel, want := range map[string]string{"to-a": "a.txt", "sub/up-a": "../a.txt", "to-sub": "sub", "x/y/d": "../.."} {
		if got, err := os.Readlink(filepath.Join(dir, rel)); err != nil || got != want {
			t.Errorf("%s = %q, %v; want a link to %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{"abs", "climb", "abs-inside", "dangling", "x/y/tricky"} {
		if _, err := os.Lstat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was copied; it can reach outside the copy", rel)
		}
	}
	fp, err := Fingerprint(context.Background(), dir, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if fp["to-a"] != "link:a.txt" {
		t.Errorf("fingerprint[to-a] = %q", fp["to-a"])
	}
	for rel := range fp {
		if strings.HasPrefix(rel, "to-sub/") {
			t.Errorf("fingerprint followed a link: %s", rel)
		}
	}
}

func TestCopyLimits(t *testing.T) {
	src, tmp := tree(t)
	if err := os.WriteFile(filepath.Join(src, "big"), make([]byte, 11), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Copy(context.Background(), src, Limits{MaxBytes: 10, MaxFiles: 100})
	if err == nil || !strings.Contains(err.Error(), "10") {
		t.Errorf("byte limit: err = %v, want one naming 10", err)
	}
	_, _, err = Copy(context.Background(), src, Limits{MaxBytes: 1 << 20, MaxFiles: 2})
	if err == nil || !strings.Contains(err.Error(), "2") {
		t.Errorf("file limit: err = %v, want one naming 2", err)
	}
	if left := leftovers(t, tmp); len(left) != 0 {
		t.Errorf("a failed copy left %v", left)
	}
}

func TestChangedFindsEdits(t *testing.T) {
	src, _ := tree(t)
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	before, err := Fingerprint(context.Background(), dir, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := Fingerprint(context.Background(), dir, noLimits)
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
	after, err := Fingerprint(context.Background(), dir, noLimits)
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
	var cleanup func() error
	go func() {
		var err error
		dir, cleanup, err = Copy(context.Background(), src, noLimits)
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
	defer func() { _ = cleanup() }()
	if _, err := os.Lstat(filepath.Join(dir, "pipe")); !os.IsNotExist(err) {
		t.Errorf("the FIFO was copied: %v", err)
	}
	// A FIFO a reviewer makes in the copy shows in the fingerprint
	// without blocking it.
	if err := syscall.Mkfifo(filepath.Join(dir, "made"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(context.Background(), dir, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if fp["made"] != "other" {
		t.Errorf("fingerprint[made] = %q, want other", fp["made"])
	}
}

func TestCopyStripsGitRemotes(t *testing.T) {
	src, _ := tree(t)
	cfg := `[core]
	repositoryformatversion = 0
	bare = false
[remote "origin"]
	url = git@github.com:example/project.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[branch "main"]
	remote = origin
	merge = refs/heads/main
[url "git@github.com:"]
	pushInsteadOf = https://github.com/
[credential]
	helper = store
[include]
	path = ../../shared.gitconfig
[includeIf "gitdir:~/work/"]
	path = work.gitconfig
[Remote "upper"]
	url = x
`
	if err := os.WriteFile(filepath.Join(src, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	got, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, gone := range []string{"remote \"origin\"", "github.com", "pushInsteadOf", "credential", "helper", "include", "shared.gitconfig", "Remote \"upper\"", "branch", "merge"} {
		if strings.Contains(text, gone) {
			t.Errorf("copy's git config still has %q:\n%s", gone, text)
		}
	}
	for _, kept := range []string{"[core]", "repositoryformatversion = 0", "bare = false"} {
		if !strings.Contains(text, kept) {
			t.Errorf("copy's git config lost %q:\n%s", kept, text)
		}
	}
	// The source is untouched.
	if orig, _ := os.ReadFile(filepath.Join(src, ".git", "config")); string(orig) != cfg {
		t.Error("the source git config changed")
	}
}

func TestCopyDropsGitFile(t *testing.T) {
	src, _ := tree(t)
	if err := os.RemoveAll(filepath.Join(src, ".git")); err != nil {
		t.Fatal(err)
	}
	// A worktree or submodule: .git is a file that points at the real
	// repository's git folder.
	if err := os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: /real/repo/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Errorf("the .git pointer file was copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("other files were not copied: %v", err)
	}
}

func TestCopiedRepoCannotPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", "https://example.invalid/repo.git"}} {
		if out, err := git(src, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := git(src, "add", "f.txt"); err != nil {
		t.Fatal(out)
	}
	if out, err := git(src, "commit", "-q", "-m", "first"); err != nil {
		t.Fatal(out)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if out, err := git(dir, "log", "--oneline"); err != nil || !strings.Contains(out, "first") {
		t.Errorf("git log in the copy: %v %s", err, out)
	}
	if out, _ := git(dir, "remote"); strings.TrimSpace(out) != "" {
		t.Errorf("the copy still has remotes: %q", out)
	}
	if out, err := git(dir, "push"); err == nil {
		t.Errorf("git push in the copy succeeded: %s", out)
	}
	if out, _ := git(src, "remote"); strings.TrimSpace(out) != "origin" {
		t.Errorf("the source lost its remote: %q", out)
	}
}

func TestCopyRejectsLinksThatClimbOutAndBack(t *testing.T) {
	src, _ := tree(t)
	// Resolves inside the source by climbing to its parent and coming
	// back in by name; in the copy the same text would land outside it.
	back := filepath.Join("..", "..", filepath.Base(src), "a.txt")
	if err := os.Symlink(back, filepath.Join(src, "sub", "back")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "sub", "back")); err != nil {
		t.Fatalf("test setup: the link should resolve in the source: %v", err)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	// The copy holds the canonical path to the resolved target, never
	// the climbing text.
	if got, err := os.Readlink(filepath.Join(dir, "sub", "back")); err != nil || got != "../a.txt" {
		t.Errorf("sub/back = %q, %v; want the canonical ../a.txt", got, err)
	}
}

func TestCopyRewritesKeptLinksCanonically(t *testing.T) {
	src, _ := tree(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "p", "q"), 0o755))
	// d points two levels up; "d/../a.txt" means a.txt in the folder the
	// kernel reaches through d, not the text's parent.
	must(os.Symlink("../..", filepath.Join(src, "p", "q", "d")))
	must(os.Symlink("d/sub/../a.txt", filepath.Join(src, "p", "q", "via")))
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	got, err := os.Readlink(filepath.Join(dir, "p", "q", "via"))
	if err != nil || got != "../../a.txt" {
		t.Errorf("p/q/via = %q, %v; want ../../a.txt", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "p", "q", "via")); err != nil || string(data) != "alpha\n" {
		t.Errorf("reading p/q/via in the copy: %q, %v", data, err)
	}
}

func TestCopySanitizesEveryGitFolder(t *testing.T) {
	src, _ := tree(t)
	remote := "[core]\n\tworktree = /real/checkout\n\tbare = false\n[remote \"origin\"]\n\turl = git@example.invalid:x.git\n"
	write := func(rel, data string) {
		t.Helper()
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".git/config", remote)
	write(".git/config.worktree", remote)
	write(".git/modules/lib/config", remote)
	write("vendor/dep/.git/config", remote)
	write(".git/commondir", "/real/repo/.git\n")
	write(".git/worktrees/w/gitdir", "/real/checkout/.git\n")
	write(".git/objects/info/alternates", "/real/repo/.git/objects\n")
	// A config that is a symlink, even to a file inside the folder, is
	// left out like every symlink inside a git folder.
	write("shared.gitconfig", remote)
	if err := os.Symlink("../../shared.gitconfig", filepath.Join(src, ".git", "modules", "config")); err != nil {
		t.Fatal(err)
	}

	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	for _, rel := range []string{".git/config", ".git/config.worktree", ".git/modules/lib/config", "vendor/dep/.git/config"} {
		p := filepath.Join(dir, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Errorf("%s missing: %v", rel, err)
			continue
		}
		if !fi.Mode().IsRegular() {
			t.Errorf("%s is not a regular file in the copy", rel)
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "remote") || strings.Contains(string(data), "worktree") {
			t.Errorf("%s still points outside:\n%s", rel, data)
		}
		if !strings.Contains(string(data), "bare = false") {
			t.Errorf("%s lost its other settings:\n%s", rel, data)
		}
	}
	for _, rel := range []string{".git/commondir", ".git/worktrees/w/gitdir", ".git/objects/info/alternates", ".git/modules/config"} {
		if _, err := os.Lstat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was copied; it points git outside the copy", rel)
		}
	}
	// Files outside .git folders are copied as they are.
	if data, _ := os.ReadFile(filepath.Join(dir, "shared.gitconfig")); string(data) != remote {
		t.Errorf("shared.gitconfig outside .git changed: %q", data)
	}
}

// gitDirAt makes a minimal git folder at rel: HEAD, objects/, refs/ and a
// config with a remote.
func gitDirAt(t *testing.T, root, rel string) {
	t.Helper()
	base := filepath.Join(root, rel)
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@example.invalid:x.git\n"
	if err := os.WriteFile(filepath.Join(base, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyHandlesGitFoldersByContent(t *testing.T) {
	src, _ := tree(t)
	if err := os.RemoveAll(filepath.Join(src, ".git")); err != nil {
		t.Fatal(err)
	}
	// .git is a symlink to a git folder with another name.
	gitDirAt(t, src, "repo-data")
	if err := os.Symlink("repo-data", filepath.Join(src, ".git")); err != nil {
		t.Fatal(err)
	}
	// A bare repository fixture, and a symlinked folder inside it.
	gitDirAt(t, src, "fixtures/server.git")
	if err := os.MkdirAll(filepath.Join(src, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../mods", filepath.Join(src, "fixtures", "server.git", "modules")); err != nil {
		t.Fatal(err)
	}

	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Errorf("a .git symlink was copied: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "fixtures", "server.git", "modules")); !os.IsNotExist(err) {
		t.Errorf("a symlink inside a git folder was copied: %v", err)
	}
	for _, rel := range []string{"repo-data/config", "fixtures/server.git/config"} {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if strings.Contains(string(data), "remote") {
			t.Errorf("%s still has a remote:\n%s", rel, data)
		}
	}
	// A plain folder that only has a file named config is not a git
	// folder and is copied as it is.
	if err := os.WriteFile(filepath.Join(src, "sub", "config"), []byte("[remote \"x\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir2, cleanup2, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup2() }()
	if data, _ := os.ReadFile(filepath.Join(dir2, "sub", "config")); string(data) != "[remote \"x\"]\n" {
		t.Errorf("a non-git config was changed: %q", data)
	}
}

func TestGitConfigAllowlist(t *testing.T) {
	src, _ := tree(t)
	cfg := "[core] worktree = /real/checkout\n" +
		"[core]\n\trepositoryformatversion = 1\n\tbare = false\n\tfsmonitor = touch /tmp/x\n\thooksPath = /real/hooks\n\tsshCommand = ssh -i key\n" +
		"[branch \"main\"] remote = /real/bare.git\n" +
		"[diff \"x\"]\n\ttextconv = cat\n" +
		"[filter \"lfs\"]\n\tclean = git-lfs clean\n" +
		"[alias]\n\tx = !echo \\\n\tmore\n" +
		"[extensions]\n\tobjectformat = sha256\n\tpartialclone = origin\n"
	if err := os.WriteFile(filepath.Join(src, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	got, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"repositoryformatversion = 1", "bare = false", "[extensions]", "objectformat = sha256"} {
		if !strings.Contains(text, want) {
			t.Errorf("config lost %q:\n%s", want, text)
		}
	}
	for _, gone := range []string{"worktree", "fsmonitor", "hooksPath", "sshCommand", "branch", "/real", "textconv", "filter", "lfs", "alias", "echo", "more", "partialclone"} {
		if strings.Contains(text, gone) {
			t.Errorf("config kept %q:\n%s", gone, text)
		}
	}
}

func TestCopyLeavesOutGitHooks(t *testing.T) {
	src, _ := tree(t)
	hook := filepath.Join(src, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitDirAt(t, src, "fixtures/server.git")
	if err := os.MkdirAll(filepath.Join(src, "fixtures", "server.git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	for _, rel := range []string{".git/hooks", "fixtures/server.git/hooks"} {
		if _, err := os.Lstat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was copied", rel)
		}
	}
}

func TestCopiedRepoIgnoresCommandSettings(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	marker := filepath.Join(t.TempDir(), "ran")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := git(filepath.Dir(bare), "init", "-q", "--bare", bare); err != nil {
		t.Fatal(out)
	}
	if out, err := git(src, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(out)
	}
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "f.txt"}, {"commit", "-q", "-m", "one"}} {
		if out, err := git(src, args...); err != nil {
			t.Fatal(out)
		}
	}
	f, err := os.OpenFile(filepath.Join(src, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "[core] worktree = %s\n[core]\n\tfsmonitor = touch %s; false\n[branch \"main\"]\n\tremote = %s\n", t.TempDir(), marker, bare)
	f.Close()

	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if out, err := git(dir, "status", "--short"); err != nil {
		t.Errorf("git status in the copy: %v %s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("core.fsmonitor ran a command from the copy")
	}
	if out, _ := git(dir, "config", "core.worktree"); strings.TrimSpace(out) != "" {
		t.Errorf("core.worktree survived: %q", out)
	}
	_, _ = git(dir, "push")
	if out, _ := git(filepath.Dir(bare), "--git-dir", bare, "branch"); strings.TrimSpace(out) != "" {
		t.Errorf("git push from the copy reached the bare repo: %q", out)
	}
}

func TestCopyFollowsSymlinkedRoot(t *testing.T) {
	src, _ := tree(t)
	link := filepath.Join(t.TempDir(), "proj")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(context.Background(), link, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("copy root is not a folder: %v %v", fi, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "sub", "b.go")); err != nil || string(data) != "package b\n" {
		t.Errorf("sub/b.go in the copy: %q, %v", data, err)
	}
}

func TestCopyAndFingerprintStopOnCancel(t *testing.T) {
	src, tmp := tree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Copy(ctx, src, noLimits); !errors.Is(err, context.Canceled) {
		t.Errorf("Copy after cancel: %v, want context.Canceled", err)
	}
	if left := leftovers(t, tmp); len(left) != 0 {
		t.Errorf("a cancelled copy left %v", left)
	}
	if _, err := Fingerprint(ctx, src, noLimits); !errors.Is(err, context.Canceled) {
		t.Errorf("Fingerprint after cancel: %v, want context.Canceled", err)
	}
}

func TestFingerprintLimits(t *testing.T) {
	src, _ := tree(t)
	if err := os.WriteFile(filepath.Join(src, "big"), make([]byte, 11), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(context.Background(), src, Limits{MaxBytes: 10, MaxFiles: 100}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("byte limit: %v, want ErrTooLarge", err)
	}
	if _, err := Fingerprint(context.Background(), src, Limits{MaxBytes: 1 << 20, MaxFiles: 2}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("file limit: %v, want ErrTooLarge", err)
	}
	if _, _, err := Copy(context.Background(), src, Limits{MaxBytes: 10, MaxFiles: 100}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Copy over the limit: %v, want ErrTooLarge", err)
	}
}

func TestCleanupRemovesReadOnlyFolders(t *testing.T) {
	src, tmp := tree(t)
	locked := filepath.Join(src, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	_, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup: %v", err)
	}
	if left := leftovers(t, tmp); len(left) != 0 {
		t.Errorf("cleanup left %v", left)
	}
}

func TestGitNamesMatchInAnyCase(t *testing.T) {
	src, _ := tree(t)
	if err := os.RemoveAll(filepath.Join(src, ".git")); err != nil {
		t.Fatal(err)
	}
	// A folder named .GIT holding a config, and a pointer file .Git in a
	// subfolder.
	gitDirAt(t, src, ".GIT")
	if err := os.WriteFile(filepath.Join(src, "sub", ".Git"), []byte("gitdir: /real/repo/.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if data, _ := os.ReadFile(filepath.Join(dir, ".GIT", "config")); strings.Contains(string(data), "remote") {
		t.Errorf(".GIT/config kept its remote:\n%s", data)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub", ".Git")); !os.IsNotExist(err) {
		t.Errorf("the .Git pointer file was copied")
	}
}

func TestGitFolderWithCommondirIsCleaned(t *testing.T) {
	src, _ := tree(t)
	wt := filepath.Join(src, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"HEAD": "ref: refs/heads/x\n", "commondir": "/real/repo/.git\n", "config": "[remote \"o\"]\n\turl = x\n"} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir, cleanup, err := Copy(context.Background(), src, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if _, err := os.Lstat(filepath.Join(dir, "wt", "commondir")); !os.IsNotExist(err) {
		t.Error("commondir in a HEAD+commondir folder was copied")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "wt", "config")); strings.Contains(string(data), "remote") {
		t.Errorf("wt/config kept its remote:\n%s", data)
	}
}

func TestCopyErrorsUseRelativePaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	src, _ := tree(t)
	secret := filepath.Join(src, "sub", "private.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, _, err := Copy(context.Background(), src, noLimits)
	if err == nil {
		t.Fatal("copying an unreadable file worked")
	}
	if errors.Is(err, ErrTooLarge) {
		t.Errorf("an I/O error reads as ErrTooLarge: %v", err)
	}
	if strings.Contains(err.Error(), src) || !strings.Contains(err.Error(), filepath.Join("sub", "private.txt")) {
		t.Errorf("error %q should name sub/private.txt without the absolute path", err)
	}
}
