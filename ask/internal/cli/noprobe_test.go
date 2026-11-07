//go:build !readonlyprobe

package cli

import (
	"testing"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
)

func TestProbeFlagAbsentByDefault(t *testing.T) {
	f := newFake(t, `echo answer`, "", adapter.Verification{})
	if code := run(t, f.d, "--probe-readonly", "fake", "m", "hi"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if f.runs(t) != 0 {
		t.Fatal("the CLI ran")
	}
	if NewRootCmd(f.d).Flags().Lookup("probe-readonly") != nil {
		t.Error("the default build declares --probe-readonly")
	}
}
