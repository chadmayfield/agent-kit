package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadmayfield/agent-kit/ask/internal/testenv"
)

// fake returns the absolute path of a script in testdata.
func fake(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// childEnv isolates the test and returns a small, explicit child environment.
func childEnv(t *testing.T) []string {
	t.Helper()
	home := testenv.Isolate(t)
	return []string{"PATH=/usr/bin:/bin", "HOME=" + home}
}

// gone reports whether no process with this pid exists.
func gone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitGone polls until pid no longer exists or the deadline passes.
func waitGone(pid int, deadline time.Time) bool {
	for {
		if gone(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// trackPids reads "<pgid>" or "<pgid> <child pid>" from path. It registers
// a cleanup that kills both if the test failed.
func trackPids(t *testing.T, path string) func() (pgid, child int) {
	t.Helper()
	read := func() (int, int) {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, 0
		}
		f := strings.Fields(string(data))
		if len(f) == 0 || len(f) > 2 {
			return 0, 0
		}
		a, _ := strconv.Atoi(f[0])
		b := 0
		if len(f) == 2 {
			b, _ = strconv.Atoi(f[1])
		}
		return a, b
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		pgid, child := read()
		if pgid > 1 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}
		if child > 1 {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})
	// child is 0 when the fake was stopped before it started its child;
	// the group check in assertGroupGone still covers that case.
	return func() (int, int) {
		t.Helper()
		pgid, child := read()
		if pgid <= 1 {
			t.Fatalf("pid file %s missing or malformed", path)
		}
		return pgid, child
	}
}

// waitPidFile polls until the file at path holds at least fields
// whitespace-separated values, or d passes. It is safe to call from a
// goroutine.
func waitPidFile(path string, fields int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if data, err := os.ReadFile(path); err == nil && len(strings.Fields(string(data))) >= fields {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// watchPidFile starts waiting for the pid file in parallel with Run. The
// returned func reports whether the fake wrote at least its group id.
func watchPidFile(path string) func() bool {
	seen := make(chan bool, 1)
	go func() { seen <- waitPidFile(path, 1, 10*time.Second) }()
	return func() bool { return <-seen }
}

func assertGroupGone(t *testing.T, pgid, child int, deadline time.Time) {
	t.Helper()
	for !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Errorf("process group %d still has members", pgid)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !waitGone(pgid, deadline) {
		t.Errorf("group leader %d still running", pgid)
	}
	if child > 1 && !waitGone(child, deadline) {
		t.Errorf("background child %d still running", child)
	}
}

func TestRunPassesStdin(t *testing.T) {
	inv := Invocation{Binary: fake(t, "echo_stdin.sh"), Env: childEnv(t), Stdin: "hello\nworld\n"}
	out, err := Run(context.Background(), inv, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(out.Stdout); got != "hello\nworld\n" {
		t.Errorf("stdout = %q, want stdin echoed", got)
	}
	if out.ExitCode != 0 || out.TimedOut || out.Interrupted || out.Signal != 0 {
		t.Errorf("outcome = %+v, want clean exit 0", out)
	}
}

func TestRunEnvNamesAreExactlyInvEnv(t *testing.T) {
	env := append(childEnv(t), "EXAMPLE_API_KEY=placeholder")
	// A variable in ask's own environment must not reach the child.
	t.Setenv("AGENT_KIT_TEST_PARENT_ONLY", "1")

	names := func(list []string) []string {
		var out []string
		for _, kv := range list {
			if kv == "" {
				continue
			}
			name, _, _ := strings.Cut(kv, "=")
			out = append(out, name)
		}
		slices.Sort(out)
		return out
	}

	cases := []struct {
		name string
		env  []string
	}{
		{"explicit", env},
		{"empty", []string{}},
		{"nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// /usr/bin/env with no arguments prints exactly its environment.
			inv := Invocation{Binary: "/usr/bin/env", Env: tc.env}
			out, err := Run(context.Background(), inv, 10*time.Second, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := names(strings.Split(string(out.Stdout), "\n"))
			want := names(tc.env)
			if !slices.Equal(got, want) {
				t.Errorf("child env names = %v, want %v", got, want)
			}
		})
	}
}

func TestRunTimeoutKillsGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pids")
	pids := trackPids(t, pidfile)
	inv := Invocation{Binary: fake(t, "group.sh"), Args: []string{pidfile}, Env: childEnv(t)}

	// Wait for both the leader and the background child.
	both := make(chan bool, 1)
	go func() { both <- waitPidFile(pidfile, 2, 10*time.Second) }()
	out, err := Run(context.Background(), inv, time.Second, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !<-both {
		t.Fatal("the fake never wrote its child pid")
	}
	if !out.TimedOut || out.Interrupted {
		t.Errorf("outcome = %+v, want TimedOut only", out)
	}
	if out.Signal != 0 {
		t.Errorf("Signal = %d, want 0 on timeout", out.Signal)
	}
	pgid, child := pids()
	if child <= 1 {
		t.Fatalf("child pid %d, want the background child", child)
	}
	assertGroupGone(t, pgid, child, time.Now().Add(5*time.Second))
}

func TestRunKeepsSignalPendingAtExit(t *testing.T) {
	env := childEnv(t)
	// Hold Run until the CLI has exited, so the exit and the waiting
	// signal are both ready when Run first selects.
	afterStart = func(exited <-chan struct{}) {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	}
	t.Cleanup(func() { afterStart = nil })
	for i := 0; i < 40; i++ {
		sigs := make(chan os.Signal, 1)
		sigs <- syscall.SIGTERM
		out, err := Run(context.Background(), Invocation{Binary: fake(t, "exit_ok.sh"), Env: env}, 10*time.Second, sigs)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !out.Interrupted || out.Signal != int(syscall.SIGTERM) || out.ExitCode != 0 {
			t.Fatalf("try %d: outcome = %+v, want Interrupted, Signal 15, exit 0", i, out)
		}
	}
}

func TestRunCapsOutput(t *testing.T) {
	if maxStdout != 64<<20 || maxStderr != 1<<20 {
		t.Errorf("caps are %d and %d, want 64 MiB and 1 MiB", maxStdout, maxStderr)
	}
	oldOut, oldErr := maxStdout, maxStderr
	maxStdout, maxStderr = 1000, 100
	t.Cleanup(func() { maxStdout, maxStderr = oldOut, oldErr })

	script := `i=0; while [ $i -lt 200 ]; do printf '%04d:56789\n' $i; printf '%04d:abcdef\n' $i >&2; i=$((i+1)); done`
	inv := Invocation{Binary: "/bin/sh", Args: []string{"-c", script}, Env: childEnv(t)}
	out, err := Run(context.Background(), inv, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var wantOut, wantErr strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&wantOut, "%04d:56789\n", i)
		fmt.Fprintf(&wantErr, "%04d:abcdef\n", i)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit %d, want 0", out.ExitCode)
	}
	if string(out.Stdout) != wantOut.String()[:1000] || !out.StdoutTruncated {
		t.Errorf("stdout %d bytes, truncated %v; want the first 1000 bytes", len(out.Stdout), out.StdoutTruncated)
	}
	full := wantErr.String()
	if string(out.Stderr) != full[len(full)-100:] || !out.StderrTruncated {
		t.Errorf("stderr %d bytes, truncated %v; want the last 100 bytes", len(out.Stderr), out.StderrTruncated)
	}

	// Output under the caps is kept whole and not marked.
	inv.Args = []string{"-c", "echo out; echo err >&2"}
	out, err = Run(context.Background(), inv, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out.Stdout) != "out\n" || string(out.Stderr) != "err\n" || out.StdoutTruncated || out.StderrTruncated {
		t.Errorf("small outcome = %+v", out)
	}
}

func TestRunEscalatesToKill(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pids")
	pids := trackPids(t, pidfile)
	inv := Invocation{Binary: fake(t, "ignore_term.sh"), Args: []string{pidfile}, Env: childEnv(t)}

	start := time.Now()
	out, err := Run(context.Background(), inv, time.Second, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.TimedOut {
		t.Errorf("outcome = %+v, want TimedOut", out)
	}
	// Timeout, then the TERM grace, then KILL: never shorter than 1 s + 2 s.
	if elapsed < 2900*time.Millisecond {
		t.Errorf("Run returned after %s; the TERM grace was skipped", elapsed)
	}
	pgid, child := pids()
	// Gone within the 1 s timeout, the 2 s grace and 3 s more.
	assertGroupGone(t, pgid, child, start.Add(6*time.Second))
}

func TestRunReportsSignal(t *testing.T) {
	inv := Invocation{Binary: fake(t, "self_signal.sh"), Env: childEnv(t)}
	out, err := Run(context.Background(), inv, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Signal != int(syscall.SIGTERM) {
		t.Errorf("Signal = %d, want %d", out.Signal, int(syscall.SIGTERM))
	}
	if out.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a signal death", out.ExitCode)
	}
	if out.TimedOut || out.Interrupted {
		t.Errorf("outcome = %+v, want neither TimedOut nor Interrupted", out)
	}
}

func TestRunClosedSignalsIsNotInterrupt(t *testing.T) {
	signals := make(chan os.Signal)
	close(signals)
	inv := Invocation{Binary: fake(t, "echo_stdin.sh"), Env: childEnv(t), Stdin: "ok"}
	out, err := Run(context.Background(), inv, 10*time.Second, signals)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Interrupted || string(out.Stdout) != "ok" {
		t.Errorf("outcome = %+v, want a normal run", out)
	}
}

func TestRunMissingBinaryIsSetupError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-cli")
	_, err := Run(context.Background(), Invocation{Binary: missing, Env: childEnv(t)}, 10*time.Second, nil)
	if err == nil {
		t.Fatal("Run with a missing binary returned no error")
	}
}

func TestRunCallsCleanup(t *testing.T) {
	type setup func(t *testing.T, inv *Invocation, ctx *context.Context) (timeout time.Duration, signals <-chan os.Signal)
	cases := []struct {
		name    string
		prepare setup
		wantErr bool
	}{
		{"success", func(t *testing.T, inv *Invocation, ctx *context.Context) (time.Duration, <-chan os.Signal) {
			inv.Binary = fake(t, "exit_ok.sh")
			return 10 * time.Second, nil
		}, false},
		{"timeout", func(t *testing.T, inv *Invocation, ctx *context.Context) (time.Duration, <-chan os.Signal) {
			pidfile := filepath.Join(t.TempDir(), "pids")
			trackPids(t, pidfile)
			inv.Binary = fake(t, "group.sh")
			inv.Args = []string{pidfile}
			return 500 * time.Millisecond, nil
		}, false},
		{"interrupt", func(t *testing.T, inv *Invocation, ctx *context.Context) (time.Duration, <-chan os.Signal) {
			pidfile := filepath.Join(t.TempDir(), "pids")
			trackPids(t, pidfile)
			inv.Binary = fake(t, "group.sh")
			inv.Args = []string{pidfile}
			sig := make(chan os.Signal, 1)
			go func() {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(pidfile); err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				sig <- os.Interrupt
			}()
			return 30 * time.Second, sig
		}, false},
		{"context cancel", func(t *testing.T, inv *Invocation, ctx *context.Context) (time.Duration, <-chan os.Signal) {
			pidfile := filepath.Join(t.TempDir(), "pids")
			trackPids(t, pidfile)
			inv.Binary = fake(t, "group.sh")
			inv.Args = []string{pidfile}
			var cancel context.CancelFunc
			*ctx, cancel = context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(pidfile); err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				cancel()
			}()
			return 30 * time.Second, nil
		}, false},
		{"setup error", func(t *testing.T, inv *Invocation, ctx *context.Context) (time.Duration, <-chan os.Signal) {
			inv.Binary = filepath.Join(t.TempDir(), "no-such-cli")
			return 10 * time.Second, nil
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			inv := Invocation{Env: childEnv(t), Cleanup: func() { calls++ }}
			ctx := context.Background()
			timeout, signals := tc.prepare(t, &inv, &ctx)
			_, err := Run(ctx, inv, timeout, signals)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run error = %v, wantErr %v", err, tc.wantErr)
			}
			if calls != 1 {
				t.Errorf("Cleanup ran %d times, want 1", calls)
			}
		})
	}
}

func TestRunInterruptCleansUp(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "prompt-dir")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "prompt"), []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "pids")
	pids := trackPids(t, pidfile)
	inv := Invocation{
		Binary:  fake(t, "group.sh"),
		Args:    []string{pidfile},
		Env:     childEnv(t),
		Cleanup: func() { _ = os.RemoveAll(tmp) },
	}

	signals := make(chan os.Signal, 1)
	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := Run(context.Background(), inv, 30*time.Second, signals)
		done <- result{out, err}
	}()

	if !waitPidFile(pidfile, 2, 5*time.Second) {
		signals <- os.Interrupt
		<-done
		t.Fatal("the fake did not write both pids within 5 s")
	}
	signals <- os.Interrupt

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10 s of the interrupt")
	}
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if !r.out.Interrupted || r.out.TimedOut {
		t.Errorf("outcome = %+v, want Interrupted only", r.out)
	}
	if r.out.Signal != int(syscall.SIGINT) {
		t.Errorf("Signal = %d, want %d", r.out.Signal, int(syscall.SIGINT))
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp dir still present after interrupt (stat err %v)", err)
	}
	pgid, child := pids()
	assertGroupGone(t, pgid, child, time.Now().Add(5*time.Second))
}

func TestRunNoControllingTerminal(t *testing.T) {
	inv := Invocation{Binary: fake(t, "tty.sh"), Env: childEnv(t)}
	out, err := Run(context.Background(), inv, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.TimedOut {
		t.Fatal("fake blocked on /dev/tty until the timeout")
	}
	if out.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7 (no tty); stdout %q", out.ExitCode, out.Stdout)
	}
}

func TestRunStopsLeftoversAfterNormalExit(t *testing.T) {
	cases := []struct{ name, script string }{
		{"child holds stdout", "leave_child.sh"},
		{"child detached from output", "leave_child_quiet.sh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "pids")
			pids := trackPids(t, pidfile)
			inv := Invocation{Binary: fake(t, tc.script), Args: []string{pidfile}, Env: childEnv(t)}

			start := time.Now()
			out, err := Run(context.Background(), inv, 30*time.Second, nil)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if out.ExitCode != 0 || out.TimedOut || out.Interrupted || out.Signal != 0 {
				t.Errorf("outcome = %+v, want the CLI's clean exit 0", out)
			}
			// The leftover dies on SIGTERM, so no grace period and no
			// 5 s pipe wait.
			if elapsed > 4500*time.Millisecond {
				t.Errorf("Run took %s; want under the 5 s pipe wait", elapsed)
			}
			_, child := pids()
			if !gone(child) {
				t.Errorf("leftover child %d still running when Run returned", child)
			}
		})
	}
}

func TestRunTimeoutAfterExitIsNotTimedOut(t *testing.T) {
	// The CLI exits 0 at once. Its leftover ignores SIGTERM, so stopping it
	// takes the 2 s grace, and the 1 s timeout passes during that time.
	pidfile := filepath.Join(t.TempDir(), "pids")
	pids := trackPids(t, pidfile)
	inv := Invocation{Binary: fake(t, "leave_term_child.sh"), Args: []string{pidfile}, Env: childEnv(t)}

	start := time.Now()
	out, err := Run(context.Background(), inv, time.Second, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed < time.Second {
		t.Fatalf("Run returned after %s, before the timeout; the test proves nothing", elapsed)
	}
	if out.TimedOut || out.Interrupted || out.ExitCode != 0 || out.Signal != 0 {
		t.Errorf("outcome = %+v, want the CLI's clean exit 0", out)
	}
	_, child := pids()
	if !waitGone(child, start.Add(6*time.Second)) {
		t.Errorf("leftover child %d still running", child)
	}
}

func TestRunContextEndsRun(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		pidfile := filepath.Join(t.TempDir(), "pids")
		pids := trackPids(t, pidfile)
		inv := Invocation{Binary: fake(t, "group.sh"), Args: []string{pidfile}, Env: childEnv(t)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(pidfile); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
		}()

		out, err := Run(ctx, inv, 30*time.Second, nil)
		if err != nil {
			t.Fatalf("Run error = %v, want nil on cancel", err)
		}
		if !out.Interrupted || out.TimedOut || out.Signal != 0 {
			t.Errorf("outcome = %+v, want Interrupted with Signal 0", out)
		}
		pgid, child := pids()
		assertGroupGone(t, pgid, child, time.Now().Add(5*time.Second))
	})
	t.Run("deadline", func(t *testing.T) {
		pidfile := filepath.Join(t.TempDir(), "pids")
		pids := trackPids(t, pidfile)
		inv := Invocation{Binary: fake(t, "group.sh"), Args: []string{pidfile}, Env: childEnv(t)}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		written := watchPidFile(pidfile)
		out, err := Run(ctx, inv, 30*time.Second, nil)
		if err != nil {
			t.Fatalf("Run error = %v, want nil on deadline", err)
		}
		if !written() {
			t.Fatal("the fake never wrote its pid file")
		}
		if !out.TimedOut || out.Interrupted || out.Signal != 0 {
			t.Errorf("outcome = %+v, want TimedOut with Signal 0", out)
		}
		pgid, child := pids()
		assertGroupGone(t, pgid, child, time.Now().Add(5*time.Second))
	})
	t.Run("already done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		inv := Invocation{Binary: fake(t, "exit_ok.sh"), Env: childEnv(t), Cleanup: func() { calls++ }}
		out, err := Run(ctx, inv, 10*time.Second, nil)
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !out.Interrupted || out.Signal != 0 || calls != 1 {
			t.Errorf("outcome = %+v, cleanup calls %d; want Interrupted, Signal 0, one cleanup", out, calls)
		}
	})
}

func TestRunStartErrorNamesBaseOnly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-cli")
	_, err := Run(context.Background(), Invocation{Binary: missing, Env: childEnv(t)}, 10*time.Second, nil)
	if err == nil {
		t.Fatal("Run with a missing binary returned no error")
	}
	if !strings.HasPrefix(err.Error(), "start no-such-cli: ") {
		t.Errorf("error = %q, want it to start with %q", err, "start no-such-cli: ")
	}
	if strings.Contains(err.Error(), filepath.Dir(missing)) {
		t.Errorf("error = %q, want no folder path in it", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %q, want it to wrap os.ErrNotExist", err)
	}
}

func TestRunStartErrorFromLookupHasNoPath(t *testing.T) {
	// A bare name goes through exec.LookPath, which fails with *exec.Error.
	_, err := Run(context.Background(), Invocation{Binary: "no-such-cli-on-path", Env: childEnv(t)}, 10*time.Second, nil)
	if err == nil {
		t.Fatal("Run with an unknown bare name returned no error")
	}
	if !strings.HasPrefix(err.Error(), "start no-such-cli-on-path: ") || strings.Count(err.Error(), "no-such-cli-on-path") != 1 {
		t.Errorf("error = %q, want the name once, in the prefix only", err)
	}
}

func TestRunPipeLimitWhenChildLeavesGroup(t *testing.T) {
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("escape.sh needs /usr/bin/perl")
	}
	// The fake's child calls setsid and keeps stdout open, so Run cannot
	// stop it. Run must stop reading after the 5 s pipe limit and keep
	// what the CLI printed.
	pidfile := filepath.Join(t.TempDir(), "escaped")
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidfile); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(data))); pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	inv := Invocation{Binary: fake(t, "escape.sh"), Args: []string{pidfile}, Env: childEnv(t)}

	start := time.Now()
	out, err := Run(context.Background(), inv, 30*time.Second, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed < 4900*time.Millisecond || elapsed > 8*time.Second {
		t.Errorf("Run took %s, want 5 to 8 s", elapsed)
	}
	if out.ExitCode != 0 || out.TimedOut || out.Interrupted {
		t.Errorf("outcome = %+v, want the CLI's clean exit 0", out)
	}
	if !strings.Contains(string(out.Stdout), "leader output") {
		t.Errorf("stdout = %q, want the CLI's output kept", out.Stdout)
	}
}
