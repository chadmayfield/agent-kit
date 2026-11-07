package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/testenv"
)

func TestResolveEnv(t *testing.T) {
	testenv.Isolate(t)
	t.Setenv("EXAMPLE_SOURCE", "example-value\n")
	got, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "env:EXAMPLE_SOURCE"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "example-value" {
		t.Errorf("Resolve = %q, want %q", got, "example-value")
	}

	// One trailing CRLF is trimmed as a unit; only one line ending goes.
	for in, want := range map[string]string{
		"example-value\r\n":   "example-value",
		"example-value\r\n\n": "example-value\r\n",
		"example-value\n\r\n": "example-value\n",
		"example-value\r":     "example-value\r",
	} {
		t.Setenv("EXAMPLE_SOURCE", in)
		got, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "env:EXAMPLE_SOURCE"})
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

func TestResolveCmdKillsLeftoverChild(t *testing.T) {
	home := testenv.Isolate(t)

	cases := []struct {
		script  string
		want    string
		wantErr string
	}{
		// The shell exits 0 after printing; the background sleep still holds
		// stdout. The printed value is the result.
		{"/bin/sleep 30 & echo $! > %s; printf value", "value", ""},
		// Nothing was printed before the shell exited.
		{"/bin/sleep 30 & echo $! > %s", "", `secret "api_key": command left a process holding its output`},
	}
	for i, c := range cases {
		pidFile := filepath.Join(home, fmt.Sprintf("pid%d", i))
		ref := "cmd:" + fmt.Sprintf(c.script, pidFile)
		got, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: ref})
		if c.wantErr == "" {
			if err != nil || got != c.want {
				t.Errorf("case %d: Resolve = %q, %v, want %q", i, got, err, c.want)
			}
		} else if err == nil || err.Error() != c.wantErr {
			t.Errorf("case %d: Resolve error = %v, want %q", i, err, c.wantErr)
		}

		data, rerr := os.ReadFile(pidFile)
		if rerr != nil {
			t.Fatalf("case %d: read pid file: %v", i, rerr)
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr != nil {
			t.Fatalf("case %d: parse pid: %v", i, perr)
		}
		// The orphaned sleep is reaped by init once killed; allow a moment.
		gone := false
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if syscall.Kill(pid, 0) == syscall.ESRCH {
				gone = true
				break
			}
		}
		if !gone {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("case %d: background sleep %d outlived Resolve", i, pid)
		}
	}
}

func TestResolveCmdTimeout(t *testing.T) {
	testenv.Isolate(t)
	old := lookupTimeout
	lookupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = old })

	// The background sleep holds stdout open; only killing the whole
	// process group lets the lookup return promptly.
	start := time.Now()
	_, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "cmd:/bin/sleep 60 & /bin/sleep 60"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Resolve succeeded for a command that never finishes")
	}
	if want := `secret "api_key": lookup timed out`; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if elapsed > waitDelay {
		t.Errorf("Resolve took %v, want under %v", elapsed, waitDelay)
	}
}

func TestResolveFile(t *testing.T) {
	home := testenv.Isolate(t)
	p := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(p, []byte("example-value\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "file:" + p})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Only one trailing newline is trimmed.
	if got != "example-value\n" {
		t.Errorf("Resolve = %q, want %q", got, "example-value\n")
	}

	// A leading ~/ means the home folder.
	got, err = Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "file:~/secret.txt"})
	if err != nil {
		t.Fatalf("Resolve with ~/: %v", err)
	}
	if got != "example-value\n" {
		t.Errorf("Resolve with ~/ = %q", got)
	}
}

func TestResolveCmd(t *testing.T) {
	testenv.Isolate(t)
	got, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "cmd:printf 'example-value\\n'"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "example-value" {
		t.Errorf("Resolve = %q, want %q", got, "example-value")
	}
}

func TestResolveCmdFailureHidesStderr(t *testing.T) {
	testenv.Isolate(t)
	_, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "cmd:echo stderr-marker-text >&2; exit 3"})
	if err == nil {
		t.Fatal("Resolve succeeded for a failing command")
	}
	if strings.Contains(err.Error(), "stderr-marker-text") {
		t.Errorf("error %q contains the command's stderr", err)
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error %q does not name the secret", err)
	}
}

func TestResolveKeychainUsesFakeSecurity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain: lookups run on macOS only")
	}
	home := testenv.Isolate(t)
	// The fake prints its arguments, so the test sees the exact call.
	fake := filepath.Join(home, "bin", "security")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := securityPath
	securityPath = fake
	t.Cleanup(func() { securityPath = old })

	for ref, want := range map[string]string{
		"keychain:placeholder-service":                     "find-generic-password -s placeholder-service -w",
		"keychain:placeholder-account@placeholder-service": "find-generic-password -a placeholder-account -s placeholder-service -w",
		"keychain:user@example.com@placeholder-service":    "find-generic-password -a user@example.com -s placeholder-service -w",
	} {
		got, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: ref})
		if err != nil {
			t.Errorf("Resolve(%q): %v", ref, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) ran security %q, want %q", ref, got, want)
		}
	}
}

func TestErrorNeverContainsRef(t *testing.T) {
	home := testenv.Isolate(t)
	if runtime.GOOS == "darwin" {
		// Never reach the real Keychain: point lookups at a fake that fails.
		fake := filepath.Join(home, "bin", "security")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\necho stderr-marker-text >&2\nexit 44\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := securityPath
		securityPath = fake
		t.Cleanup(func() { securityPath = old })
	}
	empty := filepath.Join(home, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ref    string
		secret []string // strings from the ref that must not appear
	}{
		{"env:PLACEHOLDER_UNSET_VAR", []string{"PLACEHOLDER_UNSET_VAR"}},
		{"file:" + filepath.Join(home, "placeholder-missing.txt"), []string{home, "placeholder-missing"}},
		{"file:" + empty, []string{home, "empty.txt"}},
		{"cmd:echo stderr-marker-text >&2; placeholder-command-xyz", []string{"placeholder-command-xyz", "stderr-marker-text"}},
		{"cmd:printf ''", []string{"printf"}},
		{"cmd:printf placeholder-value; exit 3", []string{"placeholder-value", "printf"}},
		{"keychain:placeholder-account@placeholder-service", []string{"placeholder-account", "placeholder-service"}},
		{"keychain:placeholder-service", []string{"placeholder-service"}},
		{"vault:placeholder-item", []string{"vault", "placeholder-item"}},
		{"", nil},
	}
	for _, c := range cases {
		ref := config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: c.ref}
		_, err := Resolve(ref)
		if err == nil {
			t.Errorf("Resolve(%q) succeeded, want an error", c.ref)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "api_key") {
			t.Errorf("Resolve(%q) error %q does not name the secret", c.ref, msg)
		}
		if c.ref != "" && strings.Contains(msg, c.ref) {
			t.Errorf("Resolve(%q) error %q contains the ref", c.ref, msg)
		}
		for _, s := range c.secret {
			if strings.Contains(msg, s) {
				t.Errorf("Resolve(%q) error %q contains %q", c.ref, msg, s)
			}
		}
	}
}
