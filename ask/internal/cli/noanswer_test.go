package cli

import (
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

// noAnswerGuard passes every line and reports no answer at End, as agy's
// plan guard does when agy wrote a plan.
type noAnswerGuard struct{}

func (noAnswerGuard) Line([]byte) error { return nil }
func (noAnswerGuard) End() error {
	return &adapter.NoAnswerError{Msg: "fake wrote a plan instead of an answer"}
}

// noAnswerAdapter is the fake adapter with a guard that reports no answer.
type noAnswerAdapter struct{ fakeAdapter }

func (a noAnswerAdapter) Build(req adapter.Request, tc config.ToolConfig) (runner.Invocation, error) {
	inv, err := a.fakeAdapter.Build(req, tc)
	inv.Guard = noAnswerGuard{}
	return inv, err
}

// TestNoAnswerIsAFailedRun checks that a run whose guard reports no answer
// exits 1 with the guard's message, prints no answer, saves no report, is
// not retried, and is not logged as a read-only guard stop.
func TestNoAnswerIsAFailedRun(t *testing.T) {
	f := newFake(t, `echo "please approve my plan"`, "", verified)
	f.d.Adapters = map[string]adapter.Adapter{"fake": noAnswerAdapter{fakeAdapter{ro: verified}}}

	code := run(t, f.d, "fake", "m", "hi")
	if code != ExitTool {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitTool, f.err.String())
	}
	if !strings.Contains(f.err.String(), "fake wrote a plan instead of an answer") {
		t.Errorf("stderr %q does not give the guard's message", f.err.String())
	}
	if strings.Contains(f.err.String(), "read-only guard") {
		t.Errorf("stderr %q calls it a guard stop", f.err.String())
	}
	if f.out.Len() != 0 {
		t.Errorf("stdout %q, want no answer", f.out.String())
	}
	if n := len(f.reports(t)); n != 0 {
		t.Errorf("%d reports saved, want none", n)
	}
	if f.runs(t) != 1 {
		t.Errorf("fake ran %d times, want 1", f.runs(t))
	}
	if strings.Contains(f.logText(t), `"guard":`) {
		t.Error("the log entry records a guard stop")
	}
}
