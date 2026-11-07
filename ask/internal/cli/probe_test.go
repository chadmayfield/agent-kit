//go:build readonlyprobe

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
)

func TestProbeRunsUnverifiedReadOnly(t *testing.T) {
	f := newFake(t, `printf '%s' "$EXAMPLE_RO_TOKEN" > "$HOME/ro-seen"
/usr/bin/env | /usr/bin/cut -d= -f1 > "$HOME/env-names"
printf '%s\n' "$@" > "$HOME/args"
echo answer`, secretsCfg, adapter.Verification{})
	// The probe skips the version check too.
	f.d.Version = func(string) (string, error) { return "", errors.New("no version") }
	// api_key has no file: a lookup of it would fail with exit 3.
	if err := os.WriteFile(filepath.Join(f.home, "ro-secret"), []byte("ro-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "--probe-readonly", "fake", "m", promptMarker); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 1 {
		t.Fatalf("CLI ran %d times, want 1", f.runs(t))
	}
	if got := f.out.String(); got != "answer\n" {
		t.Errorf("stdout %q, want answer", got)
	}
	if slices.Contains(strings.Fields(f.read(t, "args")), "--write") {
		t.Error("the probe built a write invocation")
	}
	names := strings.Fields(f.read(t, "env-names"))
	if slices.Contains(names, "EXAMPLE_API_KEY") {
		t.Error("a write-only secret reached a probe run")
	}
	if got := f.read(t, "ro-seen"); got != "ro-value" {
		t.Errorf("read-only secret is %q, want ro-value", got)
	}
	if n := len(f.reports(t)); n != 1 {
		t.Errorf("%d reports, want 1", n)
	}
	logText := f.logText(t)
	if !strings.Contains(logText, `"exit":0`) || strings.Contains(logText, promptMarker) {
		t.Errorf("log %q", logText)
	}
}

func TestProbeStillRefusedWithoutFlag(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, f.d, "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if f.runs(t) != 0 {
		t.Fatal("the CLI ran without the probe flag")
	}
}

func TestProbeFlagHidden(t *testing.T) {
	d, out, _ := testDeps(t)
	if code := run(t, d, "--help"); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	if strings.Contains(out.String(), "probe") {
		t.Errorf("help lists the probe flag:\n%s", out.String())
	}
	var buf bytes.Buffer
	cmd := NewRootCmd(d)
	cmd.SetOut(&buf)
	_ = cmd.Usage()
	if strings.Contains(buf.String(), "probe") {
		t.Error("usage lists the probe flag")
	}
}

func TestProbeWithWriteExits2(t *testing.T) {
	f := newFake(t, `echo answer`, "", verified)
	if code := run(t, f.d, "--probe-readonly", "--write", "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if f.runs(t) != 0 {
		t.Fatal("the CLI ran")
	}
	if !strings.Contains(f.err.String(), "--probe-readonly cannot be used with --write") {
		t.Errorf("stderr %q", f.err.String())
	}
}

func TestProbeDryRunSaysGateSkipped(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, f.d, "--probe-readonly", "--dry-run", "fake", "m", "hi"); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 0 {
		t.Fatal("the CLI ran")
	}
	if !strings.Contains(f.err.String(), "read-only: probe, gate skipped") {
		t.Errorf("stderr %q", f.err.String())
	}
}
