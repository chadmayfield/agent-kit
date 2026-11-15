package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/record"
	"github.com/chadmayfield/agent-kit/ask/internal/runner"
)

// listTimeout bounds a tool's own list command. Tests lower it.
var listTimeout = 30 * time.Second

// outputFunc runs a tool's own list command with env as its whole
// environment and returns its stdout.
type outputFunc func(binary string, env []string, args ...string) ([]byte, error)

// runOutput is the default outputFunc. It runs the command the way ask runs
// a tool: in its own process group, with listTimeout covering the whole
// group, a new private folder (mode 0700, removed afterwards) as its
// working folder, and signals passed on. A shared temp folder such as
// /tmp would let another local user plant a project config there for
// the tool to load.
func runOutput(signals <-chan os.Signal) outputFunc {
	return func(bin string, env []string, args ...string) ([]byte, error) {
		dir, err := os.MkdirTemp("", "ask-list-")
		if err != nil {
			return nil, fmt.Errorf("make a working folder: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("make a working folder: %w", err)
		}
		inv := runner.Invocation{Binary: bin, Args: args, Env: env, Dir: dir,
			Cleanup: func() { _ = os.RemoveAll(dir) }}
		out, err := runner.Run(context.Background(), inv, listTimeout, signals)
		switch {
		case err != nil:
			return nil, err
		case out.TimedOut:
			return nil, fmt.Errorf("timed out after %v", listTimeout)
		case out.Interrupted:
			// The signal was meant for ask: end the whole command.
			n := out.Signal
			if n == 0 {
				n = sigintNumber
			}
			return nil, &exitError{code: exitSignalBase + n, err: errors.New("interrupted")}
		case out.Signal > 0:
			return nil, fmt.Errorf("killed by signal %d", out.Signal)
		case out.ExitCode != 0:
			return nil, fmt.Errorf("exited with status %d", out.ExitCode)
		}
		return out.Stdout, nil
	}
}

// toolOutput returns the list command runner for one tool. The tool gets
// the base environment and its config's pass_env, never its secrets. When
// a signal ends the command, it is saved in *stop so the caller can end
// ask with it instead of showing the tool as unavailable.
func toolOutput(d Deps, tc config.ToolConfig, stop *error) func(string, ...string) ([]byte, error) {
	run := d.Output
	if run == nil {
		run = runOutput(d.Signals)
	}
	environ := os.Environ
	if d.Environ != nil {
		environ = d.Environ
	}
	return func(bin string, args ...string) ([]byte, error) {
		if *stop != nil {
			return nil, *stop
		}
		out, err := run(bin, childEnv(environ(), tc.PassEnv, nil), args...)
		var ee *exitError
		if errors.As(err, &ee) && ee.code > exitSignalBase {
			*stop = err
		}
		return out, err
	}
}

// addInfoCommands adds `ask models` and `ask usage` to root.
func addInfoCommands(root *cobra.Command, d Deps, tools []string) {
	var modelsJSON bool
	models := &cobra.Command{
		Use:   "models [tool]",
		Short: "List each tool's models and the config's short names",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			picked, err := pickTools(tools, args)
			if err != nil {
				return err
			}
			return runModels(d, picked, modelsJSON)
		},
	}
	models.Flags().BoolVar(&modelsJSON, "json", false, "print JSON instead of a table")

	var usageJSON, withRuns bool
	var since string
	usage := &cobra.Command{
		Use:   "usage [tool]",
		Short: "Show each tool's rate limit windows, and with --runs, ask's runs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			picked, err := pickTools(tools, args)
			if err != nil {
				return err
			}
			var from time.Time
			if since != "" {
				t, err := time.ParseInLocation("2006-01-02", since, time.Local)
				if err != nil {
					return usageErr("--since takes a date like 2026-11-15")
				}
				from = t
				withRuns = true
			}
			return runUsage(d, picked, from, withRuns, usageJSON)
		},
	}
	usage.Flags().BoolVar(&usageJSON, "json", false, "print JSON instead of text")
	usage.Flags().BoolVar(&withRuns, "runs", false, "add each tool's ask runs: count, failures, tokens and time per model")
	usage.Flags().StringVar(&since, "since", "", "count only runs on or after this date (YYYY-MM-DD, local time); implies --runs")

	for _, c := range []*cobra.Command{models, usage} {
		c.SilenceUsage, c.SilenceErrors = true, true
		c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
			return &exitError{code: ExitUsage, err: err}
		})
		root.AddCommand(c)
	}
	addReviewCommand(root, d, tools)
}

// pickTools returns every tool, or the one named in args.
func pickTools(tools, args []string) ([]string, error) {
	if len(args) == 0 {
		return tools, nil
	}
	if !slices.Contains(tools, args[0]) {
		return nil, usageErr("unknown tool %q; known tools: %s", args[0], strings.Join(tools, ", "))
	}
	return args[:1], nil
}

// home returns $HOME, or an error when it is not an absolute path.
func home(env func(string) string) (string, error) {
	h := env("HOME")
	if !filepath.IsAbs(h) {
		return "", setupErr("HOME is not set to an absolute path")
	}
	return h, nil
}

// modelEntry is one model a tool lists, with the config short names that
// point at it.
type modelEntry struct {
	ID         string   `json:"id"`
	ShortNames []string `json:"short_names,omitempty"`
	// ConfigOnly is true for a config target the tool did not list.
	ConfigOnly bool `json:"config_only,omitempty"`
}

type toolModels struct {
	Tool   string       `json:"tool"`
	Source string       `json:"source"`
	Models []modelEntry `json:"models"`
	Error  string       `json:"error,omitempty"`
}

func runModels(d Deps, tools []string, asJSON bool) error {
	env := d.Env
	if env == nil {
		env = os.Getenv
	}
	cfg, err := config.Load(config.DefaultPath(env))
	if err != nil {
		return setupErr("%v", err)
	}
	h, err := home(env)
	if err != nil {
		return err
	}
	var all []toolModels
	var stop error
	for _, tool := range tools {
		if err := pendingSignal(d.Signals); err != nil {
			return err
		}
		tc := cfg.Tools[tool]
		tm := toolModels{Tool: tool}
		ids, source, lerr := listModels(tool, tc, h, toolOutput(d, tc, &stop))
		if stop != nil {
			return stop
		}
		tm.Source = source
		if lerr != nil {
			tm.Error = lerr.Error()
		}
		tm.Models = mergeShortNames(ids, tc.Models)
		all = append(all, tm)
	}

	if asJSON {
		enc := json.NewEncoder(d.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}
	clean := func(s string) string {
		if d.StdoutIsTerminal {
			return forTerminal(s, nil)
		}
		return s
	}
	for i, tm := range all {
		if i > 0 {
			fmt.Fprintln(d.Stdout)
		}
		fmt.Fprintf(d.Stdout, "%s (%s)\n", tm.Tool, clean(tm.Source))
		if tm.Error != "" {
			fmt.Fprintf(d.Stdout, "  error: %s\n", clean(tm.Error))
		}
		if len(tm.Models) == 0 {
			fmt.Fprintln(d.Stdout, "  no models listed")
			continue
		}
		tw := tabwriter.NewWriter(d.Stdout, 0, 4, 2, ' ', 0)
		for _, m := range tm.Models {
			note := ""
			if len(m.ShortNames) > 0 {
				note = "short name: " + strings.Join(m.ShortNames, ", ")
				if m.ConfigOnly {
					note += " (config only)"
				}
			}
			if note == "" {
				fmt.Fprintf(tw, "  %s\n", clean(m.ID))
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\n", clean(m.ID), clean(note))
		}
		_ = tw.Flush()
	}
	return nil
}

// listModels returns the models a tool knows and where the list came from.
func listModels(tool string, tc config.ToolConfig, home string, output func(string, ...string) ([]byte, error)) ([]string, string, error) {
	switch tool {
	case "codex":
		path := filepath.Join(home, ".codex", "models_cache.json")
		ids, err := codexModels(path)
		return ids, "~/.codex/models_cache.json", err
	case "claude":
		return nil, "claude has no model list; pass an alias such as opus or sonnet, or a full model name", nil
	case "agy", "opencode":
		bin, err := resolveBinary(tool, tc)
		if err != nil {
			return nil, tool + " models", err
		}
		out, err := output(bin, "models")
		if err != nil {
			return nil, tool + " models", fmt.Errorf("%s models failed: %v", tool, err)
		}
		return parseModelLines(out), tool + " models", nil
	}
	return nil, "no model list", nil
}

// codexModels reads the slugs in codex's model cache.
func codexModels(path string) ([]string, error) {
	data, err := readSmallFile(path, maxModelCache)
	if err != nil {
		return nil, fmt.Errorf("read the codex model cache: %w", err)
	}
	var cache struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, errors.New("the codex model cache is not valid JSON")
	}
	var ids []string
	for _, m := range cache.Models {
		if m.Slug != "" {
			ids = append(ids, m.Slug)
		}
	}
	return ids, nil
}

// parseModelLines takes the first field of each line that is one model ID,
// as `agy models` (ID, a tab, a display name) and `opencode models` (one ID
// per line) print them. Lines whose first field has no "-" or "/", such as
// a "Fetching..." header, are skipped.
func parseModelLines(out []byte) []string {
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id := fields[0]
		if strings.Contains(line, "\t") || len(fields) == 1 {
			if strings.ContainsAny(id, "-/") && !strings.HasSuffix(id, ":") {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// mergeShortNames pairs listed IDs with config short names. A short name
// whose target is not listed adds that target as config only.
func mergeShortNames(ids []string, shorts map[string]string) []modelEntry {
	byID := map[string][]string{}
	for short, full := range shorts {
		if full != "" {
			byID[full] = append(byID[full], short)
		}
	}
	var out []modelEntry
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		names := byID[id]
		sort.Strings(names)
		out = append(out, modelEntry{ID: id, ShortNames: names})
	}
	var extra []string
	for full := range byID {
		if !seen[full] {
			extra = append(extra, full)
		}
	}
	sort.Strings(extra)
	for _, full := range extra {
		names := byID[full]
		sort.Strings(names)
		out = append(out, modelEntry{ID: full, ShortNames: names, ConfigOnly: true})
	}
	return out
}

// runTotal sums the log entries for one tool and model ID.
type runTotal struct {
	Tool string `json:"tool"`
	// Model is the model ID passed to the tool, so a short name and its
	// full name count together.
	Model string `json:"model"`
	// Runs counts runs that started the tool. NotStarted counts the ones
	// that ended before the tool ran: a refusal, bad arguments, a setup
	// failure such as a missing binary or secret, or a signal.
	Runs       int `json:"runs"`
	NotStarted int `json:"not_started"`
	// Failed counts runs that failed other than by a guard stop; Guarded
	// counts guard stops.
	Failed       int   `json:"failed"`
	Guarded      int   `json:"guard_stops"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	DurationMS   int64 `json:"duration_ms"`
}

func runUsage(d Deps, tools []string, since time.Time, withRuns, asJSON bool) error {
	env := d.Env
	if env == nil {
		env = os.Getenv
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	h, err := home(env)
	if err != nil {
		return err
	}
	state, err := stateDir(env)
	if err != nil {
		return setupErr("%v", err)
	}

	cfg, err := config.Load(config.DefaultPath(env))
	if err != nil {
		return setupErr("%v", err)
	}
	limits := make([]toolLimits, 0, len(tools))
	var stop error
	for _, tool := range tools {
		if err := pendingSignal(d.Signals); err != nil {
			return err
		}
		tc := cfg.Tools[tool]
		limits = append(limits, readLimits(tool, tc, h, state, now(), toolOutput(d, tc, &stop)))
		if stop != nil {
			return stop
		}
	}
	if err := pendingSignal(d.Signals); err != nil {
		return err
	}

	// totals stays nil without --runs, which tells writeUsage to skip runs.
	var totals []runTotal
	bad := 0
	if withRuns {
		entries, n, err := record.ReadLog(filepath.Join(state, "log.jsonl"))
		if err != nil {
			return setupErr("%v", err)
		}
		bad = n
		totals = totalRuns(entries, tools, since)
	}

	if asJSON {
		enc := json.NewEncoder(d.Stdout)
		enc.SetIndent("", "  ")
		out := struct {
			Limits     []toolLimits `json:"limits"`
			Runs       *[]runTotal  `json:"runs,omitempty"`
			BadLogRows int          `json:"bad_log_rows,omitempty"`
		}{Limits: limits, BadLogRows: bad}
		if withRuns {
			out.Runs = &totals
		}
		return enc.Encode(out)
	}

	runsTitle := "ask runs"
	if !since.IsZero() {
		runsTitle += " since " + since.Format("2006-01-02")
	}
	st := style{color: d.StdoutIsTerminal && env("NO_COLOR") == "", rich: richColor(env)}
	writeUsage(d.Stdout, limits, totals, runsTitle, now(), d.StdoutIsTerminal, st)
	if bad > 0 {
		fmt.Fprintf(d.Stderr, "ask: warning: skipped %d unreadable lines in the run log\n", bad)
	}
	return nil
}

// totalRuns groups log entries by tool and model.
func totalRuns(entries []record.LogEntry, tools []string, since time.Time) []runTotal {
	byKey := map[[2]string]*runTotal{}
	for _, e := range entries {
		if !slices.Contains(tools, e.Tool) || (!since.IsZero() && e.Time.Before(since)) {
			continue
		}
		model := e.ModelID
		if model == "" {
			model = e.Model
		}
		k := [2]string{e.Tool, model}
		r := byKey[k]
		if r == nil {
			r = &runTotal{Tool: e.Tool, Model: model}
			byKey[k] = r
		}
		switch {
		case e.Guard != "":
			r.Runs++
			r.Guarded++
		case e.Exit != 0 && e.DurationMS == 0:
			// A failure with no tool time: the tool never started.
			r.NotStarted++
		case e.Exit != 0:
			r.Runs++
			r.Failed++
		default:
			r.Runs++
		}
		r.InputTokens += e.InputTokens
		r.OutputTokens += e.OutputTokens
		r.DurationMS += e.DurationMS
	}
	out := make([]runTotal, 0, len(byKey)) // never nil: an empty list still means --runs
	for _, r := range byKey {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		return out[i].Model < out[j].Model
	})
	return out
}
