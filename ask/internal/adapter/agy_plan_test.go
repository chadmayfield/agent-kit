package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
)

const agyTestID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

// TestAgyReadOnlyAddsPlanNote checks that a read-only run puts the plan
// note in front of the prompt and gets the plan guard, and that a --write
// run gets neither.
func TestAgyReadOnlyAddsPlanNote(t *testing.T) {
	home := aoIsolate(t, "agy")
	a := NewAgy()

	inv, err := a.Build(Request{Model: "m", Prompt: "review " + aoMarker}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer inv.Cleanup()
	data, err := os.ReadFile(agyPromptFile(t, inv.Args))
	if err != nil {
		t.Fatal(err)
	}
	if want := agyPlanNote + "\n\nreview " + aoMarker; string(data) != want {
		t.Fatalf("prompt file = %q, want the note, a blank line, then the prompt", data)
	}
	if !slices.Contains(inv.Args, "--sandbox") || !slices.Contains(inv.Args, "plan") {
		t.Fatalf("Args = %q, want --sandbox --mode plan", inv.Args)
	}
	g, ok := inv.Guard.(*agyPlanGuard)
	if !ok {
		t.Fatalf("Guard = %T, want *agyPlanGuard", inv.Guard)
	}
	if g.home != home {
		t.Fatalf("guard home = %q, want %q", g.home, home)
	}

	inv, err = a.Build(Request{Model: "m", Prompt: "edit", Write: true}, config.ToolConfig{})
	if err != nil {
		t.Fatalf("Build --write: %v", err)
	}
	defer inv.Cleanup()
	data, err = os.ReadFile(agyPromptFile(t, inv.Args))
	if err != nil || string(data) != "edit" {
		t.Fatalf("--write prompt file = %q, %v, want the prompt alone", data, err)
	}
	if inv.Guard != nil {
		t.Fatalf("--write Guard = %T, want none", inv.Guard)
	}
}

// writeAgyPlan creates the plan file agy writes for conversation id.
func writeAgyPlan(t *testing.T, home, id string) string {
	t.Helper()
	p := agyPlanPath(home, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runAgyGuard feeds lines to a plan guard for home and returns End's error.
func runAgyGuard(home string, lines ...string) error {
	g := &agyPlanGuard{home: home}
	for _, l := range lines {
		if err := g.Line([]byte(l)); err != nil {
			return err
		}
	}
	return g.End()
}

func TestAgyPlanGuard(t *testing.T) {
	home := t.TempDir()
	result := `{"conversation_id":"` + agyTestID + `","status":"SUCCESS","response":"Please approve the plan."}`

	if err := runAgyGuard(home, result); err != nil {
		t.Fatalf("no plan file: End = %v, want nil", err)
	}

	plan := writeAgyPlan(t, home, agyTestID)
	err := runAgyGuard(home, result)
	var na *NoAnswerError
	if !errors.As(err, &na) {
		t.Fatalf("plan file: End = %v, want *NoAnswerError", err)
	}
	if !strings.Contains(na.Msg, plan) || strings.Contains(na.Msg, "approve") {
		t.Fatalf("message %q must name the plan file and never quote the response", na.Msg)
	}

	// The JSON object may span several lines.
	if err := runAgyGuard(home, "{", `  "conversation_id": "`+agyTestID+`",`, `  "status": "SUCCESS"`, "}"); !errors.As(err, &na) {
		t.Fatalf("multi-line output: End = %v, want *NoAnswerError", err)
	}

	// Nothing that is not a plain conversation ID reaches a path.
	writeAgyPlan(t, home, "x")
	for name, lines := range map[string][]string{
		"traversal":  {`{"conversation_id":"../brain/x"}`},
		"not a uuid": {`{"conversation_id":"x"}`},
		"no id":      {`{"status":"SUCCESS","response":"ok"}`},
		"not json":   {"not json"},
		"empty":      nil,
	} {
		if err := runAgyGuard(home, lines...); err != nil {
			t.Errorf("%s: End = %v, want nil", name, err)
		}
	}
}

// TestAgyPlanGuardTooLong checks that output past the cap is dropped and
// passes, since the guard cannot read it.
func TestAgyPlanGuardTooLong(t *testing.T) {
	old := maxAgyPlanGuardOutput
	maxAgyPlanGuardOutput = 64
	t.Cleanup(func() { maxAgyPlanGuardOutput = old })
	home := t.TempDir()
	writeAgyPlan(t, home, agyTestID)
	long := `{"conversation_id":"` + agyTestID + `","response":"` + strings.Repeat("x", 100) + `"}`
	if err := runAgyGuard(home, long); err != nil {
		t.Fatalf("End = %v, want nil for output past the cap", err)
	}
}
