package cli

import (
	"slices"
	"sort"
	"strings"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
)

// reviewTools are the tools ask review can run, in output order.
var reviewTools = []string{"claude", "codex", "agy"}

// reviewPick is one tool and the model ask review runs it with.
type reviewPick struct {
	Tool, Model string
}

// pickReviewers returns the tools to run, in reviewTools order. A model
// from flags wins over rc.Models. A tool with no model, or one not in
// known, is skipped. No pick at all is a usage error.
func pickReviewers(flags map[string]string, rc config.ReviewConfig, known []string) ([]reviewPick, error) {
	var picks []reviewPick
	for _, tool := range reviewTools {
		if !slices.Contains(known, tool) {
			continue
		}
		model := flags[tool]
		if model == "" {
			model = rc.Models[tool]
		}
		if model != "" {
			picks = append(picks, reviewPick{Tool: tool, Model: model})
		}
	}
	if len(picks) == 0 {
		return nil, usageErr("pick at least one reviewer with --claude, --codex or --agy, or set review.models in the config")
	}
	return picks, nil
}

// reviewPresets are the built-in --as kinds. Each text goes in front of
// the prompt.
var reviewPresets = map[string]string{
	"adversarial": "Review this adversarially. Try to break it: find inputs, sequences and states that make it fail, and show each with a concrete example. Report each finding with file:line, severity (high, medium or low) and how you verified it.",
	"code":        "Review this code for bugs, risky logic, missing error handling and missing tests. Report each finding with file:line, severity (high, medium or low) and a concrete case that triggers it.",
	"docs":        "Review this documentation against the code it describes. Find statements the code does not back up, missing steps and unclear text. Report each finding with file:line, severity (high, medium or low) and the correct statement.",
	"security":    "Review this for security problems: injection, secrets reaching logs or output, unsafe file and process handling, and missing input checks. Report each finding with file:line, severity (high, medium or low) and how an attacker would use it.",
}

// presetText returns the text for an --as kind. A config entry wins over
// a built-in one. An unknown kind is a usage error that lists the kinds.
func presetText(kind string, rc config.ReviewConfig) (string, error) {
	if text, ok := rc.Presets[kind]; ok && text != "" {
		return text, nil
	}
	if text, ok := reviewPresets[kind]; ok {
		return text, nil
	}
	kinds := make([]string, 0, len(reviewPresets)+len(rc.Presets))
	for k := range reviewPresets {
		kinds = append(kinds, k)
	}
	for k := range rc.Presets {
		if _, builtIn := reviewPresets[k]; !builtIn {
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	return "", usageErr("unknown --as kind %q; known kinds: %s", kind, strings.Join(kinds, ", "))
}
