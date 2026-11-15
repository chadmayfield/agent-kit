package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
)

func TestPickReviewers(t *testing.T) {
	known := []string{"agy", "claude", "codex", "opencode"}
	rc := config.ReviewConfig{Models: map[string]string{"codex": "gpt-6.1-sol", "nope": "x"}}
	got, err := pickReviewers(map[string]string{"claude": "opus"}, rc, known)
	if err != nil {
		t.Fatal(err)
	}
	want := []reviewPick{{Tool: "claude", Model: "opus"}, {Tool: "codex", Model: "gpt-6.1-sol"}}
	if !slices.Equal(got, want) {
		t.Errorf("picks = %+v, want %+v", got, want)
	}

	// A flag wins over the config, and the order is claude, codex, agy.
	got, _ = pickReviewers(map[string]string{"agy": "flash", "codex": "mini"}, rc, known)
	want = []reviewPick{{Tool: "codex", Model: "mini"}, {Tool: "agy", Model: "flash"}}
	if !slices.Equal(got, want) {
		t.Errorf("picks = %+v, want %+v", got, want)
	}

	// A tool this build does not know is skipped.
	got, _ = pickReviewers(map[string]string{"claude": "opus", "agy": "flash"}, config.ReviewConfig{}, []string{"claude"})
	if !slices.Equal(got, []reviewPick{{Tool: "claude", Model: "opus"}}) {
		t.Errorf("picks with an unknown tool = %+v", got)
	}

	if _, err := pickReviewers(nil, config.ReviewConfig{}, known); err == nil {
		t.Error("no picks gave no error")
	} else if exitCode(err) != ExitUsage {
		t.Errorf("no picks: exit %d, want %d", exitCode(err), ExitUsage)
	}
}

func TestPresetText(t *testing.T) {
	got, err := presetText("adversarial", config.ReviewConfig{})
	if err != nil || got != reviewPresets["adversarial"] || got == "" {
		t.Errorf("adversarial = %q, %v", got, err)
	}
	for _, kind := range []string{"adversarial", "code", "docs", "security"} {
		if text := reviewPresets[kind]; !strings.Contains(text, "file:line") || !strings.Contains(text, "severity") {
			t.Errorf("preset %s does not ask for file:line and severity: %q", kind, text)
		}
	}
	got, err = presetText("adversarial", config.ReviewConfig{Presets: map[string]string{"adversarial": "X"}})
	if err != nil || got != "X" {
		t.Errorf("config override = %q, %v", got, err)
	}
	got, err = presetText("plan", config.ReviewConfig{Presets: map[string]string{"plan": "Check the plan."}})
	if err != nil || got != "Check the plan." {
		t.Errorf("config kind = %q, %v", got, err)
	}
	_, err = presetText("nope", config.ReviewConfig{})
	if err == nil || !strings.Contains(err.Error(), "adversarial, code, docs, security") || exitCode(err) != ExitUsage {
		t.Errorf("unknown kind: %v", err)
	}
}
