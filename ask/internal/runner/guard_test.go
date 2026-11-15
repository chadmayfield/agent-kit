package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recordGuard records the lines it sees and trips on a line equal to trip.
type recordGuard struct {
	trip  string
	lines []string
	ended bool
	end   error
}

func (g *recordGuard) Line(line []byte) error {
	g.lines = append(g.lines, string(line))
	if g.trip != "" && string(line) == g.trip {
		return errors.New("tripped")
	}
	return nil
}

func (g *recordGuard) End() error {
	g.ended = true
	return g.end
}

func TestGuardTripStopsTheCLI(t *testing.T) {
	g := &recordGuard{trip: "BAD"}
	inv := Invocation{
		Binary: "/bin/sh",
		Args:   []string{"-c", "echo ok; echo BAD; exec sleep 30"},
		Env:    childEnv(t),
		Guard:  g,
	}
	start := time.Now()
	out, err := Run(context.Background(), inv, 60*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Run took %v; the guard did not stop the CLI", took)
	}
	if out.GuardErr == nil || out.GuardErr.Error() != "tripped" {
		t.Fatalf("GuardErr = %v, want tripped", out.GuardErr)
	}
	if out.TimedOut {
		t.Error("TimedOut is set")
	}
	if g.ended {
		t.Error("End ran after a trip")
	}
}

func TestGuardSeesLinesAndEnd(t *testing.T) {
	g := &recordGuard{end: errors.New("end failed")}
	script := "printf 'a\\nb\\nlast'"
	inv := Invocation{Binary: "/bin/sh", Args: []string{"-c", script}, Env: childEnv(t), Guard: g}
	out, err := Run(context.Background(), inv, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(g.lines, ",") != "a,b,last" {
		t.Errorf("lines = %q, want a, b, last", g.lines)
	}
	if !g.ended {
		t.Error("End did not run")
	}
	if out.GuardErr == nil || out.GuardErr.Error() != "end failed" {
		t.Errorf("GuardErr = %v, want the End error", out.GuardErr)
	}
}

func TestGuardTripsOnLongLine(t *testing.T) {
	old := maxGuardLine
	maxGuardLine = 1024
	t.Cleanup(func() { maxGuardLine = old })
	g := &recordGuard{}
	// A long line, then a line the guard would trip on if it saw it, then
	// a long wait the trip must cut short.
	script := fmt.Sprintf("printf 'ok\\n'; head -c %d /dev/zero | tr '\\0' x; printf '\\nhidden\\n'; exec sleep 30", 4096)
	inv := Invocation{Binary: "/bin/sh", Args: []string{"-c", script}, Env: childEnv(t), Guard: g}
	start := time.Now()
	out, err := Run(context.Background(), inv, 60*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the long line did not stop the CLI")
	}
	if !errors.Is(out.GuardErr, errLineTooLong) {
		t.Fatalf("GuardErr = %v, want errLineTooLong", out.GuardErr)
	}
	if strings.Join(g.lines, ",") != "ok" {
		t.Errorf("lines = %q, want only ok", g.lines)
	}
}

func TestGuardEndSkippedAfterFailure(t *testing.T) {
	g := &recordGuard{end: errors.New("end failed")}
	inv := Invocation{Binary: "/bin/sh", Args: []string{"-c", "echo oops >&2; exit 3"}, Env: childEnv(t), Guard: g}
	out, err := Run(context.Background(), inv, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", out.ExitCode)
	}
	if g.ended || out.GuardErr != nil {
		t.Errorf("End ran after a failed exit: ended=%v GuardErr=%v", g.ended, out.GuardErr)
	}
}

func TestGuardEndSkippedAfterTimeout(t *testing.T) {
	g := &recordGuard{end: errors.New("end failed")}
	inv := Invocation{Binary: "/bin/sh", Args: []string{"-c", "exec sleep 30"}, Env: childEnv(t), Guard: g}
	out, err := Run(context.Background(), inv, 200*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut {
		t.Fatal("not timed out")
	}
	if g.ended || out.GuardErr != nil {
		t.Errorf("End ran after a timeout: ended=%v GuardErr=%v", g.ended, out.GuardErr)
	}
}

func TestGuardIgnoresCutLineWhenStopped(t *testing.T) {
	// A timeout cuts a line in half: that is a timeout, not a guard stop.
	g := &recordGuard{trip: "{", end: errors.New("end failed")}
	inv := Invocation{Binary: "/bin/sh", Args: []string{"-c", "printf 'ok\\n{'; exec sleep 30"}, Env: childEnv(t), Guard: g}
	out, err := Run(context.Background(), inv, 300*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut || out.GuardErr != nil {
		t.Errorf("TimedOut=%v GuardErr=%v; want a timeout and no guard error", out.TimedOut, out.GuardErr)
	}

	// A CLI that fails on its own with a cut last line keeps its failure.
	g = &recordGuard{trip: "{", end: errors.New("end failed")}
	inv = Invocation{Binary: "/bin/sh", Args: []string{"-c", "printf 'ok\\n{'; exit 5"}, Env: childEnv(t), Guard: g}
	out, err = Run(context.Background(), inv, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 5 || out.GuardErr != nil {
		t.Errorf("ExitCode=%d GuardErr=%v; want 5 and no guard error", out.ExitCode, out.GuardErr)
	}

	// After a clean exit the cut last line is still checked.
	g = &recordGuard{trip: "{"}
	inv = Invocation{Binary: "/bin/sh", Args: []string{"-c", "printf 'ok\\n{'"}, Env: childEnv(t), Guard: g}
	out, err = Run(context.Background(), inv, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.GuardErr == nil {
		t.Error("a cut last line after exit 0 was not checked")
	}
}
