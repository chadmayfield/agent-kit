// Package testenv keeps tests away from the real home folder and real CLIs.
package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Isolate points HOME and the XDG folders at a fresh temp home, and PATH at
// a temp bin folder holding copies of the given fake CLI scripts. It fails
// the test if os.UserHomeDir or a path built from the XDG folders is outside
// the temp home.
// It uses t.Setenv, so tests that call it cannot run in parallel.
func Isolate(t *testing.T, fakes ...string) (home string) {
	t.Helper()

	home = t.TempDir()
	// Resolve symlinks, since macOS temp folders live under /private/var.
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}

	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatalf("testenv: make bin folder: %v", err)
	}
	for _, src := range fakes {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("testenv: read fake %s: %v", filepath.Base(src), err)
		}
		dst := filepath.Join(bin, filepath.Base(src))
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatalf("testenv: write fake %s: %v", filepath.Base(src), err)
		}
		if err := os.Chmod(dst, 0o755); err != nil {
			t.Fatalf("testenv: chmod fake %s: %v", filepath.Base(src), err)
		}
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("PATH", bin)

	// Unset CLI config folders and credential-like variables. t.Setenv
	// restores them when the test ends.
	unset := func(k string) {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("testenv: unset %s: %v", k, err)
		}
	}
	for _, k := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_CONFIG"} {
		unset(k)
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		if strings.HasSuffix(up, "_KEY") || strings.HasSuffix(up, "_TOKEN") ||
			strings.Contains(up, "SECRET") || strings.Contains(up, "PASSWORD") {
			unset(k)
		}
	}

	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Fatalf("testenv: os.UserHomeDir is %q (err %v), want the temp home", got, err)
	}
	for name, p := range map[string]string{
		"config path":   filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agent-kit", "config.json"),
		"report folder": filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "reports"),
		"log path":      filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "log.jsonl"),
	} {
		if !Under(home, p) {
			t.Fatalf("testenv: %s %s is not under the temp home", name, p)
		}
	}
	return home
}

// Under reports whether path p is inside folder root.
func Under(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
