// Package runner runs a tool CLI as a child process. It targets Unix only.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// termGrace is how long the group has to exit after SIGTERM before
	// it gets SIGKILL.
	termGrace = 2 * time.Second
	// pipeDelay bounds how long Run reads output after the group is
	// stopped.
	pipeDelay = 5 * time.Second
)

// Output caps. Tests lower them.
var (
	// maxStdout is how much of the CLI's stdout Run keeps, from the start.
	maxStdout = 64 << 20
	// maxStderr is how much of the CLI's stderr Run keeps, from the end.
	maxStderr = 1 << 20
)

// afterStart, when set by a test, runs after the CLI starts and before Run
// waits on it.
var afterStart func(exited <-chan struct{})

// Outcome is what happened to one run.
type Outcome struct {
	// ExitCode is the CLI's exit status, or -1 when it did not exit
	// normally (killed by a signal, or never started because ctx was
	// already done).
	ExitCode int
	// Signal is the signal number received on the signals channel when
	// Interrupted is true, and 0 when ctx caused the interrupt. When
	// Interrupted and TimedOut are false, it is the signal that killed the
	// CLI on its own, or 0. It is never the signal Run itself sent.
	Signal int
	// TimedOut is true when Run stopped the CLI because the timeout or
	// the ctx deadline passed.
	TimedOut bool
	// Interrupted is true when a signal arrived on the signals channel,
	// even as the CLI exited, or when ctx was cancelled before the CLI
	// exited.
	Interrupted bool
	// GuardErr is the error from the Invocation's Guard, or nil. When Line
	// returned it, Run stopped the CLI.
	GuardErr error
	Stdout   []byte
	Stderr   []byte
	// StdoutTruncated is true when stdout passed maxStdout and the rest
	// was dropped.
	StdoutTruncated bool
	// StderrTruncated is true when stderr passed maxStderr and its start
	// was dropped.
	StderrTruncated bool
	Duration        time.Duration
}

// Run starts inv in its own session and process group and waits for it.
//
// A timeout of 0 or less means no timeout. signals may be nil; a closed
// channel means no more signals. On a timeout, a signal or a done ctx, Run
// sends SIGTERM to the group, waits up to 2 s, sends SIGKILL and reaps the
// CLI. Leftover group members after a normal exit are stopped the same way,
// and ExitCode keeps the CLI's status. A signal already waiting when the CLI
// exits still sets Interrupted and Signal. Run never re-raises a signal.
//
// Run keeps the first 64 MiB of stdout and the last 1 MiB of stderr. It
// returns an error only when the CLI could not start. inv.Cleanup runs once
// on every path, after the group is gone.
func Run(ctx context.Context, inv Invocation, timeout time.Duration, signals <-chan os.Signal) (out Outcome, err error) {
	if inv.Cleanup != nil {
		defer inv.Cleanup()
	}
	if ctx.Err() != nil {
		out.ExitCode = -1
		markContext(&out, ctx)
		return out, nil
	}

	cmd := exec.Command(inv.Binary, inv.Args...)
	// A nil Env would inherit ask's environment; use an empty list instead.
	cmd.Env = inv.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = inv.Dir
	setProcAttr(cmd)

	// Real pipe files let cmd.Wait return once the CLI is reaped, even when
	// a leftover process still holds the pipes.
	p, err := openPipes(inv.Stdin != "")
	if err != nil {
		return Outcome{}, startError(inv.Binary, err)
	}
	defer p.closeAll()
	// With no input the child reads the null device, never ask's stdin.
	if p.stdinR != nil {
		cmd.Stdin = p.stdinR
	}
	cmd.Stdout = p.stdoutW
	cmd.Stderr = p.stderrW

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Outcome{}, startError(inv.Binary, err)
	}
	// With Setsid the child leads a new session and process group, so its
	// pid is the group id.
	pgid := cmd.Process.Pid
	p.closeChildEnds()

	stdout := &headBuffer{max: maxStdout}
	stderr := &tailBuffer{max: maxStderr}
	var stdoutSink io.Writer = stdout
	var lines *lineGuard
	var tripped <-chan error
	if inv.Guard != nil {
		lines = &lineGuard{guard: inv.Guard, tripped: make(chan error, 1)}
		stdoutSink = io.MultiWriter(stdout, lines)
		tripped = lines.tripped
	}
	copied := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stdoutSink, p.stdoutR); copied <- struct{}{} }()
	go func() { _, _ = io.Copy(stderr, p.stderrR); copied <- struct{}{} }()
	if p.stdinW != nil {
		go func() {
			_, _ = io.WriteString(p.stdinW, inv.Stdin)
			_ = p.stdinW.Close()
		}()
	}

	exited := make(chan struct{})
	go func() {
		// The error repeats what ProcessState says; it is not a setup
		// failure.
		_ = cmd.Wait()
		close(exited)
	}()
	if afterStart != nil {
		afterStart(exited)
	}

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	stoppedByRun := false
wait:
	for {
		select {
		case <-exited:
			// select picks at random among ready cases, so a signal
			// that came in with the exit may still be waiting.
			takePendingSignal(signals, &out)
			break wait
		case gerr := <-tripped:
			out.GuardErr = gerr
			stoppedByRun = !hasExited(exited)
			break wait
		case <-timer:
			if hasExited(exited) {
				takePendingSignal(signals, &out)
				break wait
			}
			out.TimedOut = true
			stoppedByRun = true
			break wait
		case s, ok := <-signals:
			if !ok {
				// A closed channel means no more signals, not a signal.
				signals = nil
				continue
			}
			// Record the signal even if the CLI exited at the same moment.
			// Stop the group only if the CLI is still running.
			out.Interrupted = true
			out.Signal = signalNumber(s)
			stoppedByRun = !hasExited(exited)
			break wait
		case <-ctx.Done():
			if hasExited(exited) {
				takePendingSignal(signals, &out)
				break wait
			}
			markContext(&out, ctx)
			stoppedByRun = true
			break wait
		}
	}
	// One cancellation path: after a timeout, signal or ctx, or after a
	// normal exit that left processes behind in the group.
	if stoppedByRun || groupAlive(pgid) {
		stopGroup(pgid, exited)
	}
	out.Duration = time.Since(start)

	code, sig := exitStatus(cmd.ProcessState)
	out.ExitCode = code
	if !stoppedByRun && !out.Interrupted {
		out.Signal = sig
	}

	// Collect output. Only a process that left the group can still hold
	// the pipes; stop reading after pipeDelay.
	drain := time.NewTimer(pipeDelay)
	defer drain.Stop()
	for n := 0; n < 2; n++ {
		select {
		case <-copied:
		case <-drain.C:
			p.closeReadEnds()
			<-copied
		}
	}
	out.Stdout, out.StdoutTruncated = stdout.buf.Bytes(), stdout.dropped
	out.Stderr, out.StderrTruncated = stderr.bytes()
	if lines != nil {
		out.GuardErr = lines.finish(out.GuardErr, stoppedByRun || out.Interrupted || out.TimedOut, out.ExitCode)
	}
	return out, nil
}

// maxGuardLine is the longest stdout line a Guard checks. It matches
// maxStdout, the most the parser ever reads. A longer line trips the guard,
// since it cannot be checked. Tests lower it.
var maxGuardLine = 64 << 20

// errLineTooLong trips the guard on a line it cannot check.
var errLineTooLong = errors.New("a stdout line was too long for the read-only guard to check")

// lineGuard splits stdout into lines for a Guard. Write always reports
// success, so the copy keeps draining the pipe after a trip.
type lineGuard struct {
	guard   Guard
	tripped chan error
	pending []byte
	err     error
}

func (g *lineGuard) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && g.err == nil {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			g.add(p)
			break
		}
		g.add(p[:i])
		if g.err == nil {
			g.check(g.pending)
		}
		g.pending = g.pending[:0]
		p = p[i+1:]
	}
	return n, nil
}

// add appends part of a line, and trips the guard once the line passes
// maxGuardLine.
func (g *lineGuard) add(p []byte) {
	if len(g.pending)+len(p) > maxGuardLine {
		g.pending = nil
		g.trip(errLineTooLong)
		return
	}
	g.pending = append(g.pending, p...)
}

func (g *lineGuard) check(line []byte) {
	if err := g.guard.Line(line); err != nil {
		g.trip(err)
	}
}

// trip records the first error and tells Run. tripped has room for one
// send, and g.err keeps it to one.
func (g *lineGuard) trip(err error) {
	if g.err == nil {
		g.err = err
		g.tripped <- err
	}
}

// finish returns the run's guard error after the copy is done. A trip
// during the run always counts. Otherwise, a CLI that Run stopped or that
// failed on its own keeps that result, so the caller can report it and
// retry. After a clean exit, a last line with no newline is checked and
// then End runs.
func (g *lineGuard) finish(prev error, stopped bool, exitCode int) error {
	if prev != nil {
		return prev
	}
	if g.err != nil {
		return g.err
	}
	// A CLI that Run stopped, or that failed on its own, keeps that result.
	// Its last line may be cut short, so it is not checked.
	if stopped || exitCode != 0 {
		return nil
	}
	if len(g.pending) > 0 {
		g.check(g.pending)
		if g.err != nil {
			return g.err
		}
	}
	return g.guard.End()
}

// takePendingSignal records a signal that is already waiting on signals,
// without blocking. A closed or nil channel records nothing.
func takePendingSignal(signals <-chan os.Signal, out *Outcome) {
	select {
	case s, ok := <-signals:
		if ok {
			out.Interrupted = true
			out.Signal = signalNumber(s)
		}
	default:
	}
}

// headBuffer keeps the first max bytes written to it and drops the rest.
// Write always reports success, so the copy keeps draining the pipe.
type headBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped bool
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - h.buf.Len(); room < len(p) {
		h.dropped = true
		if room > 0 {
			h.buf.Write(p[:room])
		}
		return len(p), nil
	}
	h.buf.Write(p)
	return len(p), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf     []byte
	max     int
	dropped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	// Trim only when the buffer reaches twice the cap.
	if len(t.buf) > 2*t.max {
		t.trim()
	}
	return len(p), nil
}

func (t *tailBuffer) trim() {
	if len(t.buf) > t.max {
		t.dropped = true
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
}

// bytes returns the kept bytes and whether any were dropped.
func (t *tailBuffer) bytes() ([]byte, bool) {
	t.trim()
	return t.buf, t.dropped
}

// startError reports why the CLI did not start. It names the binary's base
// name and the inner error text, never the full path.
func startError(binary string, err error) error {
	var pathErr *fs.PathError
	var execErr *exec.Error
	switch {
	case errors.As(err, &pathErr):
		err = pathErr.Err
	case errors.As(err, &execErr):
		err = execErr.Err
	}
	return fmt.Errorf("start %s: %w", filepath.Base(binary), err)
}

// markContext records why ctx ended the run.
func markContext(out *Outcome, ctx context.Context) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.TimedOut = true
		return
	}
	out.Interrupted = true
}

// hasExited reports whether the CLI has already been reaped.
func hasExited(exited <-chan struct{}) bool {
	select {
	case <-exited:
		return true
	default:
		return false
	}
}

// stopGroup sends SIGTERM to the group, waits up to termGrace for the
// group to go, sends SIGKILL, then waits until the CLI is reaped.
func stopGroup(pgid int, exited <-chan struct{}) {
	_ = signalGroup(pgid, sigTerm)
	if !waitGroupGone(pgid, termGrace) {
		_ = signalGroup(pgid, sigKill)
	}
	<-exited
}

// waitGroupGone polls until no process in the group is left or d passes.
// The Wait goroutine reaps the leader, so a zombie leader does not count.
func waitGroupGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !groupAlive(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pipes holds both ends of the child's stdin, stdout and stderr pipes.
type pipes struct {
	stdinR, stdinW   *os.File
	stdoutR, stdoutW *os.File
	stderrR, stderrW *os.File
}

func openPipes(withStdin bool) (*pipes, error) {
	p := &pipes{}
	var err error
	if withStdin {
		if p.stdinR, p.stdinW, err = os.Pipe(); err != nil {
			p.closeAll()
			return nil, err
		}
	}
	if p.stdoutR, p.stdoutW, err = os.Pipe(); err != nil {
		p.closeAll()
		return nil, err
	}
	if p.stderrR, p.stderrW, err = os.Pipe(); err != nil {
		p.closeAll()
		return nil, err
	}
	return p, nil
}

// closeChildEnds closes ask's copies of the ends the child uses, so the
// readers see EOF once every process holding them is gone.
func (p *pipes) closeChildEnds() {
	closeFiles(p.stdinR, p.stdoutW, p.stderrW)
}

func (p *pipes) closeReadEnds() {
	closeFiles(p.stdoutR, p.stderrR)
}

func (p *pipes) closeAll() {
	closeFiles(p.stdinR, p.stdinW, p.stdoutR, p.stdoutW, p.stderrR, p.stderrW)
}

// closeFiles closes each non-nil file. Closing one twice is harmless.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
