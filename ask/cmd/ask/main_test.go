package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadmayfield/agent-kit/ask/internal/testenv"
)

// TestSIGINTKillsGroupAndCleansUp builds ask with the asktest fake tool,
// interrupts a run, and checks that ask exits 130, the fake CLI's child is
// gone and the temp folder its Build made is removed.
func TestSIGINTKillsGroupAndCleansUp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	// Read the real Go caches before Isolate moves HOME, for the build below.
	caches, err := exec.Command(goTool, "env", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	cacheDirs := strings.Split(strings.TrimSpace(string(caches)), "\n")
	if len(cacheDirs) != 2 {
		t.Fatalf("go env printed %q", caches)
	}

	state := t.TempDir()
	// Placeholder tool binary on the isolated PATH. The asktest fake writes
	// its real script in Build.
	placeholder := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(placeholder, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	testenv.Isolate(t, placeholder)
	bin := filepath.Join(t.TempDir(), "ask")
	build := exec.Command(goTool, "build", "-trimpath", "-tags", "asktest", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOCACHE="+cacheDirs[0], "GOMODCACHE="+cacheDirs[1])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "--write", "fake", "some-model", "hello")
	cmd.Env = append(os.Environ(), "ASK_FAKE_STATE="+state)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	childFile := filepath.Join(state, "child")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(childFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake CLI never started; stderr %q", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || child <= 0 {
		t.Fatalf("child pid %q: %v", data, err)
	}
	dirData, err := os.ReadFile(filepath.Join(state, "tmpdir"))
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := string(dirData)
	if fi, err := os.Stat(tmpDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("temp folder before the signal: %v %v", fi, err)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ask did not exit after SIGINT")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("exit: %v, want status 130; stderr %q", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want empty", stdout.String())
	}

	gone := false
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			gone = true
			break
		}
	}
	if !gone {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Errorf("fake CLI child %d still running", child)
	}
	if _, err := os.Stat(tmpDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp folder still there: %v", err)
	}
}
