package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
	"github.com/chadmayfield/agent-kit/ask/internal/record"
)

const infoCfg = `{"tools": {
  "codex": {"models": {"sol": "gpt-6.1-sol", "old": "gpt-retired"}},
  "agy": {"models": {"hy": "gemini-4-hydrogen-high"}}
}}`

// newInfo sets up an isolated home with the real adapters, a codex model
// cache, and fake agy and opencode binaries whose list output comes from
// listOut.
func newInfo(t *testing.T, listOut map[string]string) *fakeRun {
	t.Helper()
	f := newFake(t, `echo answer`, infoCfg, verified)
	f.d.Adapters = adapter.Default()
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(f.home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".codex/models_cache.json", `{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6-astra"}]}`, 0o600)
	write("bin/agy", "#!/bin/sh\n", 0o755)
	write("bin/opencode", "#!/bin/sh\n", 0o755)
	f.d.Output = func(bin string, env []string, args ...string) ([]byte, error) {
		key := filepath.Base(bin)
		switch {
		case len(args) > 0 && args[0] == "--version":
			if v, ok := listOut[key+" version"]; ok {
				return []byte(v), nil
			}
			return []byte("1.5.9\n"), nil
		case len(args) > 0 && args[0] != "models":
			key += " usage"
		}
		return []byte(listOut[key]), nil
	}
	return f
}

func TestModelsListsEachToolWithShortNames(t *testing.T) {
	f := newInfo(t, map[string]string{
		"agy":      "Fetching available models...\ngemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\ngemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n",
		"opencode": "anthropic/claude-haiku-5-5\nopenai/gpt-6\n",
	})
	if code := run(t, f.d, "models"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	got := f.out.String()
	for _, want := range []string{
		"codex (~/.codex/models_cache.json)",
		"gpt-6.1-sol", "short name: sol",
		"gpt-retired", "short name: old (config only)",
		"gemini-4-hydrogen-high", "short name: hy",
		"gemini-3.8-flash-low",
		"anthropic/claude-haiku-5-5",
		"claude has no model list",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Fetching") {
		t.Errorf("output kept agy's header line:\n%s", got)
	}
}

func TestModelsOneToolJSON(t *testing.T) {
	f := newInfo(t, nil)
	if code := run(t, f.d, "models", "codex", "--json"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	var got []toolModels
	if err := json.Unmarshal(f.out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, f.out.String())
	}
	if len(got) != 1 || got[0].Tool != "codex" || len(got[0].Models) != 3 {
		t.Fatalf("got %+v", got)
	}
	if m := got[0].Models[0]; m.ID != "gpt-6.1-sol" || len(m.ShortNames) != 1 || m.ShortNames[0] != "sol" {
		t.Errorf("first model = %+v", m)
	}
}

func TestModelsUnknownTool(t *testing.T) {
	f := newInfo(t, nil)
	if code := run(t, f.d, "models", "nope"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestAliasTreatsModelsAsAModel(t *testing.T) {
	f := newFake(t, `echo answer`, "", verified)
	f.d.Adapters = map[string]adapter.Adapter{"codex": fakeAdapter{ro: verified}}
	writeConfig(t, `{"tools": {"codex": {"binary": "fake"}}}`)
	f.d.Argv0 = "ask-codex"
	if code := run(t, f.d, "--no-report", "models", "hi"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if f.runs(t) != 1 {
		t.Errorf("the tool ran %d times, want 1", f.runs(t))
	}
}

// codexSession writes a codex session file with one rate limit event.
func codexSession(t *testing.T, home, day, name, line string) {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const rateLine = `{"timestamp":"2026-11-07T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":12.0,"window_minutes":300,"resets_at":1794070800},"secondary":{"used_percent":45.0,"window_minutes":10080,"resets_at":1794500000},"credits":{"balance":"125.50"},"plan_type":"pro"}}}`

// agyUsage is trimmed `agy --print /usage --output-format json` output.
const agyUsage = `{"conversation_id":"","status":"SUCCESS","response":"x","command":{"name":"usage","data":{"groups":[` +
	`{"name":"Gemini Models","buckets":[` +
	`{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.99,"reset_time":"2026-11-14T19:24:35Z"},` +
	`{"id":"gemini-5h","window":"5h","remaining_fraction":0.75,"reset_time":"2026-11-07T18:24:35Z"}]},` +
	`{"name":"Gemini 4 Argon","buckets":[{"id":"argon-weekly","window":"weekly","remaining_fraction":1,"reset_time":"2026-11-14T19:24:35Z"}]}]}}}`

func TestUsageShowsCodexWindowAndRuns(t *testing.T) {
	f := newInfo(t, map[string]string{"agy usage": agyUsage})
	// The newest file has no rate limits, so the one before it is used.
	// An older reading: older timestamp, in an older session.
	older := strings.Replace(strings.Replace(rateLine, "12.0", "77.0", 1), "2026-11-07T15:00:00Z", "2026-11-06T15:00:00Z", 1)
	codexSession(t, f.home, "2026/11/06", "rollout-a.jsonl", older)
	codexSession(t, f.home, "2026/11/07", "rollout-b.jsonl", rateLine)
	codexSession(t, f.home, "2026/11/07", "rollout-c.jsonl", `{"type":"event_msg","payload":{"type":"token_count","rate_limits":null}}`)
	// The older reading's file was written last. The newest reading still
	// wins, since codex picks by the reading's own timestamp; this pins
	// that, independent of how close together the files were written.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(f.home, ".codex", "sessions", "2026/11/06", "rollout-a.jsonl"), later, later); err != nil {
		t.Fatal(err)
	}

	log := filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask", "log.jsonl")
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	for _, e := range []record.LogEntry{
		{Time: day.AddDate(0, 0, -3), Tool: "codex", Model: "sol", InputTokens: 1000, OutputTokens: 10, DurationMS: 1000},
		{Time: day, Tool: "codex", Model: "sol", InputTokens: 2000, OutputTokens: 20, DurationMS: 2000},
		{Time: day, Tool: "claude", Model: "haiku", Exit: 2, Guard: "x", InputTokens: 5},
	} {
		if err := record.AppendLog(log, e); err != nil {
			t.Fatal(err)
		}
	}

	if code := run(t, f.d, "usage"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	got := f.out.String()
	if !strings.HasPrefix(got, "\n ") {
		t.Errorf("output does not start with a blank line:\n%q", got)
	}
	for _, want := range []string{
		"all models", " 5h ", " 12% used", " 88% left", " 7d ", " 45% used",
		"plan pro · credits 125.50 · as of", "from last codex session",
		"Gemini Models", " 25% used", " 75% left", "Gemini 4 Argon", "100% left", "live · from agy /usage",
		"claude", "not available", "resets ", "█", "░",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"ask runs", "haiku", "\x1b["} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output without --runs has %q:\n%s", unwanted, got)
		}
	}
	// Tools with limits come before tools without.
	if strings.Index(got, " codex ") > strings.Index(got, " claude ") {
		t.Errorf("claude (no limits) is shown before codex:\n%s", got)
	}
	if strings.Contains(got, "77%") {
		t.Errorf("used an older session file:\n%s", got)
	}

	// Every meter starts in the same column.
	col := -1
	for _, line := range strings.Split(got, "\n") {
		if i := strings.IndexAny(line, "█░"); i >= 0 {
			if col >= 0 && i != col {
				t.Errorf("meter at byte %d, want %d: %q", i, col, line)
			}
			col = i
		}
	}

	f.out.Reset()
	if code := run(t, f.d, "usage", "--runs"); code != 0 {
		t.Fatalf("--runs: exit %d; stderr %q", code, f.err.String())
	}
	runsOut := f.out.String()
	for _, want := range []string{"ask runs", " sol ", "2 runs", "3.0k in · 30 out", "haiku", "1 run ", "1 guard stop"} {
		if !strings.Contains(runsOut, want) {
			t.Errorf("--runs output lacks %q:\n%s", want, runsOut)
		}
	}
	// agy rows come in model group order, 5h before 7d.
	if i, j := strings.Index(got, "Gemini 4 Argon"), strings.Index(got, "Gemini Models"); i < 0 || j < i {
		t.Errorf("agy groups out of order:\n%s", got)
	}

	f.out.Reset()
	if code := run(t, f.d, "usage", "codex", "--since", "2026-11-07", "--json"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	var js struct {
		Limits []toolLimits `json:"limits"`
		Runs   []runTotal   `json:"runs"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &js); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, f.out.String())
	}
	if len(js.Limits) != 1 || !js.Limits[0].Available || len(js.Limits[0].Rows) != 2 {
		t.Fatalf("limits = %+v", js.Limits)
	}
	if w := js.Limits[0].Rows[0]; w.WindowMinutes != 300 || w.Window != "5h" || w.UsedPercent != 12 || w.LeftPercent != 88 || w.ResetsAt.Unix() != 1794070800 {
		t.Errorf("5h window = %+v", w)
	}
	if len(js.Runs) != 1 || js.Runs[0].Runs != 1 || js.Runs[0].InputTokens != 2000 {
		t.Errorf("runs since 2026-11-07 = %+v", js.Runs)
	}
}

func TestUsageWithoutData(t *testing.T) {
	f := newInfo(t, nil)
	if code := run(t, f.d, "usage", "codex"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	got := f.out.String()
	if !strings.Contains(got, "codex  not available: no codex session with rate limits") || strings.Contains(got, "ask runs") {
		t.Errorf("output:\n%s", got)
	}
	if code := run(t, f.d, "usage", "--since", "yesterday"); code != 2 {
		t.Errorf("bad --since: exit %d, want 2", code)
	}
}

func TestWindowAndResetText(t *testing.T) {
	for m, want := range map[int]string{300: "5h", 10080: "7d", 90: "90m"} {
		if got := windowName(m); got != want {
			t.Errorf("windowName(%d) = %q, want %q", m, got, want)
		}
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	st := style{}
	if got := resetPart(st, now.Add(3*time.Hour+12*time.Minute), now); !strings.Contains(got, "· in 3h12m") || !strings.HasPrefix(got, "resets ") {
		t.Errorf("resetPart = %q", got)
	}
	if got := resetPart(st, now.Add(-time.Minute), now); !strings.Contains(got, "reading is old") {
		t.Errorf("resetPart past = %q", got)
	}
}

func TestUntilAndCredits(t *testing.T) {
	for d, want := range map[time.Duration]string{
		78*time.Hour + 35*time.Minute: "3d6h",
		3*time.Hour + 12*time.Minute:  "3h12m",
		5 * time.Minute:               "5m",
	} {
		if got := untilText(d); got != want {
			t.Errorf("untilText(%v) = %q, want %q", d, got, want)
		}
	}
	if got := shortCredits("125.4969825000"); got != "125.50" {
		t.Errorf("shortCredits = %q", got)
	}
	if got := shortCredits("unlimited"); got != "unlimited" {
		t.Errorf("shortCredits = %q", got)
	}
}

func TestParseAgyUsage(t *testing.T) {
	rows, err := parseAgyUsage([]byte(agyUsage))
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v, err %v", rows, err)
	}
	r := rows[1]
	if r.Models != "Gemini Models" || r.Window != "5h" || r.WindowMinutes != 300 || r.UsedPercent != 25 || r.LeftPercent != 75 {
		t.Errorf("5h row = %+v", r)
	}
	if rows[0].Window != "7d" || rows[0].WindowMinutes != 10080 {
		t.Errorf("weekly row = %+v", rows[0])
	}
	for _, bad := range []string{"", "not json", `{"status":"ERROR","command":{"name":"usage"}}`, `{"status":"SUCCESS","command":{"name":"model"}}`} {
		if _, err := parseAgyUsage([]byte(bad)); err == nil {
			t.Errorf("parseAgyUsage(%q) gave no error", bad)
		}
	}
	// An unknown window name passes through as written.
	odd := strings.Replace(agyUsage, `"window":"5h"`, `"window":"daily"`, 1)
	rows, _ = parseAgyUsage([]byte(odd))
	if rows[1].Window != "daily" || rows[1].WindowMinutes != 0 {
		t.Errorf("odd window row = %+v", rows[1])
	}
}

func TestUsageAgyFailureIsNotAvailable(t *testing.T) {
	f := newInfo(t, map[string]string{"agy usage": "garbage"})
	if code := run(t, f.d, "usage", "agy"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "not available: agy /usage gave no limits") {
		t.Errorf("output:\n%s", f.out.String())
	}
}

// writeClaudeLimits writes the file the status line command saves.
func writeClaudeLimits(t *testing.T, data string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "ask")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-limits.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageClaudeFromStatusLine(t *testing.T) {
	f := newInfo(t, nil)
	if code := run(t, f.d, "usage", "claude"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.out.String(), "not available: no status line reading yet") {
		t.Errorf("no file:\n%s", f.out.String())
	}

	// An Ultra plan reports only the weekly window.
	f.out.Reset()
	writeClaudeLimits(t, `{"written_at":"2026-11-07T14:00:00Z","rate_limits":{"seven_day":{"used_percentage":41.2,"resets_at":1794500000}}}`)
	if code := run(t, f.d, "usage", "claude", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var js struct {
		Limits []toolLimits `json:"limits"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &js); err != nil {
		t.Fatal(err)
	}
	tl := js.Limits[0]
	if !tl.Available || tl.Live || tl.AsOf == nil || len(tl.Rows) != 1 {
		t.Fatalf("claude limits = %+v", tl)
	}
	if r := tl.Rows[0]; r.Window != "7d" || r.UsedPercent != 41.2 || r.ResetsAt.Unix() != 1794500000 {
		t.Errorf("weekly row = %+v", r)
	}

	// A Max plan adds the 5-hour window; an unknown key shows by name.
	f.out.Reset()
	writeClaudeLimits(t, `{"written_at":"2026-11-07T14:00:00Z","rate_limits":{`+
		`"five_hour":{"used_percentage":23.5,"resets_at":1794070800},`+
		`"seven_day":{"used_percentage":41.2,"resets_at":1794500000},`+
		`"seven_day_opus":{"used_percentage":5,"resets_at":1794500000},`+
		`"broken":{"resets_at":1794500000}}}`)
	if code := run(t, f.d, "usage", "claude"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := f.out.String()
	for _, want := range []string{"24%", "76%", "41%", "59%", "seven_day_opus", "Nov 7"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	// 5h, then 7d, then the unknown window.
	i5, i7, iu := strings.Index(got, " 5h "), strings.Index(got, " 7d "), strings.Index(got, "seven_day_opus")
	if i5 < 0 || i7 < i5 || iu < i7 {
		t.Errorf("windows out of order:\n%s", got)
	}
	if strings.Contains(got, "broken") {
		t.Errorf("a window with no percentage was shown:\n%s", got)
	}

	f.out.Reset()
	writeClaudeLimits(t, `not json`)
	_ = run(t, f.d, "usage", "claude")
	if !strings.Contains(f.out.String(), "is not valid JSON") {
		t.Errorf("bad file:\n%s", f.out.String())
	}
}

func TestUsageViewHelpers(t *testing.T) {
	cases := map[float64]int{0: 0, 0.4: 1, 3: 1, 33: 7, 50: 10, 100: 20, 140: 20}
	for used, want := range cases {
		filled, empty := meter(used)
		if n := utf8.RuneCountInString(filled); n != want || n+utf8.RuneCountInString(empty) != barWidth {
			t.Errorf("meter(%v) = %d filled, %d empty; want %d filled", used, n, utf8.RuneCountInString(empty), want)
		}
	}
	for n, want := range map[int]string{71: "71", 16476: "16.5k", 1250000: "1.25M"} {
		if got := shortCount(n); got != want {
			t.Errorf("shortCount(%d) = %q, want %q", n, got, want)
		}
	}
	const (
		green  = "\x1b[32mx\x1b[0m"
		yellow = "\x1b[33mx\x1b[0m"
		orange = "\x1b[38;5;208mx\x1b[0m"
		red    = "\x1b[31mx\x1b[0m"
	)
	rich, basic, off := style{color: true, rich: true}, style{color: true}, style{rich: true}
	for used, want := range map[float64][2]string{
		0: {green, green}, 49.9: {green, green},
		50: {yellow, yellow}, 69.9: {yellow, yellow},
		70: {orange, yellow}, 89.9: {orange, yellow},
		90: {red, red}, 100: {red, red},
	} {
		if got := rich.level(used, "x"); got != want[0] {
			t.Errorf("256-color level(%v) = %q, want %q", used, got, want[0])
		}
		if got := basic.level(used, "x"); got != want[1] {
			t.Errorf("16-color level(%v) = %q, want %q", used, got, want[1])
		}
		if got := off.level(used, "x"); got != "x" {
			t.Errorf("no-color level(%v) = %q", used, got)
		}
	}
	for env, want := range map[[2]string]bool{
		{"truecolor", ""}: true, {"24bit", ""}: true, {"", "xterm-256color"}: true,
		{"", "xterm-direct"}: true, {"", "xterm"}: false, {"", ""}: false, {"", "screen"}: false,
	} {
		get := func(k string) string {
			if k == "COLORTERM" {
				return env[0]
			}
			if k == "TERM" {
				return env[1]
			}
			return ""
		}
		if got := richColor(get); got != want {
			t.Errorf("richColor(COLORTERM=%q TERM=%q) = %v, want %v", env[0], env[1], got, want)
		}
	}
}

func TestUsageColorOnlyOnTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	f := newInfo(t, map[string]string{"agy usage": agyUsage})
	f.d.StdoutIsTerminal = true
	if code := run(t, f.d, "usage", "agy"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.out.String(), "\x1b[") {
		t.Error("no color on a terminal")
	}
	t.Setenv("NO_COLOR", "1")
	f.out.Reset()
	if code := run(t, f.d, "usage", "agy"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(f.out.String(), "\x1b[") {
		t.Error("color despite NO_COLOR")
	}
}

func TestListCommandEnvIsBaseAndPassEnv(t *testing.T) {
	f := newInfo(t, nil)
	writeConfig(t, `{"tools": {"agy": {"pass_env": ["EXAMPLE_PASS"]}}}`)
	f.d.Environ = func() []string {
		return []string{"PATH=/usr/bin", "HOME=" + f.home, "EXAMPLE_PASS=yes", "PRIVATE_TOKEN=secret"}
	}
	var got []string
	f.d.Output = func(bin string, env []string, args ...string) ([]byte, error) {
		got = env
		return nil, nil
	}
	if code := run(t, f.d, "models", "agy"); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, f.err.String())
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "EXAMPLE_PASS=yes") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("env lacks pass_env or PATH: %q", got)
	}
	if strings.Contains(joined, "PRIVATE_TOKEN") {
		t.Errorf("env leaked PRIVATE_TOKEN: %q", got)
	}
}

func TestRunOutputStopsTheWholeGroup(t *testing.T) {
	old := listTimeout
	listTimeout = 300 * time.Millisecond
	t.Cleanup(func() { listTimeout = old })
	env := []string{"PATH=/usr/bin:/bin"}

	out, err := runOutput(nil)("/bin/sh", env, "-c", "echo hello")
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("plain run: %q, %v", out, err)
	}
	if _, err := runOutput(nil)("/bin/sh", env, "-c", "exit 4"); err == nil || !strings.Contains(err.Error(), "status 4") {
		t.Errorf("failed run: %v", err)
	}

	// The shell's child holds the pipes; the timeout must still end it.
	start := time.Now()
	_, err = runOutput(nil)("/bin/sh", env, "-c", "sleep 30 & wait")
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %v; the child kept the command alive", took)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
}

func TestAgyUsageNeedsNewEnoughAgy(t *testing.T) {
	for ver, ok := range map[string]bool{"1.1.11": true, "1.5.9": true, "2.0.0": true, "1.1.10": false, "1.0.99": false, "": false, "agy dev": false} {
		f := newInfo(t, map[string]string{"agy usage": agyUsage, "agy version": ver})
		var calls []string
		inner := f.d.Output
		f.d.Output = func(bin string, env []string, args ...string) ([]byte, error) {
			calls = append(calls, strings.Join(args, " "))
			return inner(bin, env, args...)
		}
		if code := run(t, f.d, "usage", "agy"); code != 0 {
			t.Fatalf("%q: exit %d", ver, code)
		}
		ranUsage := slices.Contains(calls, "--print /usage --output-format json")
		if ranUsage != ok {
			t.Errorf("agy %q: ran /usage = %v, want %v (calls %q)", ver, ranUsage, ok, calls)
		}
		if !ok && !strings.Contains(f.out.String(), "needs agy 1.1.11 or later") {
			t.Errorf("agy %q: output:\n%s", ver, f.out.String())
		}
	}
	for _, out := range []string{"1.2.0 1.1.0", "agy 1.1.10-beta\nUpdate available: 1.1.11", "1.2.0-rc1", "agy (1.1.10)\n1.1.11"} {
		if versionAtLeast(out, [3]int{1, 1, 11}) {
			t.Errorf("versionAtLeast(%q) = true, want false", out)
		}
	}
	if !versionAtLeast("agy (1.5.9)", [3]int{1, 1, 11}) {
		t.Error("versionAtLeast(\"agy (1.5.9)\") = false")
	}
}

func TestCodexPicksNewestReadingAcrossFiles(t *testing.T) {
	f := newInfo(t, nil)
	// A resumed old session (older folder and name) holds the newest
	// reading and was written last.
	newer := strings.Replace(strings.Replace(rateLine, "12.0", "55.0", 1), "2026-11-07T15:00:00Z", "2026-11-07T16:00:00Z", 1)
	codexSession(t, f.home, "2026/11/01", "rollout-old.jsonl", newer)
	codexSession(t, f.home, "2026/11/07", "rollout-new.jsonl", rateLine)
	old := filepath.Join(f.home, ".codex", "sessions", "2026/11/01", "rollout-old.jsonl")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(old, later, later); err != nil {
		t.Fatal(err)
	}
	if code := run(t, f.d, "usage", "codex"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.out.String(), " 55% used") {
		t.Errorf("did not pick the newest reading:\n%s", f.out.String())
	}
}

func TestUntrustedFilesAreBounded(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readSmallFile(fifo, 100); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was read as a file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO hung")
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, 101), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSmallFile(big, 100); err == nil {
		t.Error("an oversized file was read")
	}
	if data, err := readSmallFile(big, 101); err != nil || len(data) != 101 {
		t.Errorf("a file at the limit: %d bytes, %v", len(data), err)
	}

	var got []string
	input := "short\n" + strings.Repeat("x", 300) + "\nafter\nlast"
	if err := eachLine(strings.NewReader(input), 100, func(l []byte) { got = append(got, string(l)) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "short,after,last" {
		t.Errorf("eachLine = %q", got)
	}
}

func TestTotalRunsCounts(t *testing.T) {
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	got := totalRuns([]record.LogEntry{
		{Time: day, Tool: "codex", Model: "sol", ModelID: "gpt-6.1-sol", DurationMS: 900},
		{Time: day, Tool: "codex", Model: "gpt-6.1-sol", ModelID: "gpt-6.1-sol", Exit: 1, DurationMS: 900},
		{Time: day, Tool: "codex", Model: "sol", ModelID: "gpt-6.1-sol", Exit: 2},
		{Time: day, Tool: "codex", Model: "sol", ModelID: "gpt-6.1-sol", Exit: 3},
		{Time: day, Tool: "codex", Model: "sol", ModelID: "gpt-6.1-sol", Exit: 3, DurationMS: 900},
		{Time: day, Tool: "claude", Model: "haiku", Exit: 2, Guard: "x", DurationMS: 50},
		{Time: day, Tool: "claude", Model: "haiku", Exit: 124, DurationMS: 600000},
		{Time: day, Tool: "claude", Model: "haiku", Exit: 130},
	}, []string{"claude", "codex"}, time.Time{})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	cl, cx := got[0], got[1]
	// Exit 3 with tool time is a report-save failure after a real run.
	if cx.Model != "gpt-6.1-sol" || cx.Runs != 3 || cx.Failed != 2 || cx.NotStarted != 2 || cx.Guarded != 0 {
		t.Errorf("codex = %+v; want short and full name together, 3 runs, 2 failed, 2 not started", cx)
	}
	if cl.Runs != 2 || cl.Guarded != 1 || cl.Failed != 1 || cl.NotStarted != 1 {
		t.Errorf("claude = %+v; want 2 runs, 1 guard stop, 1 failed, 1 not started", cl)
	}
}

func TestInfoCommandsStopOnSignal(t *testing.T) {
	for _, args := range [][]string{{"models"}, {"usage"}} {
		f := newInfo(t, map[string]string{"agy usage": agyUsage})
		var calls int
		f.d.Output = func(bin string, env []string, a ...string) ([]byte, error) {
			calls++
			// The first tool command is interrupted, as Ctrl-C would.
			return nil, &exitError{code: exitSignalBase + sigintNumber, err: errors.New("interrupted")}
		}
		if code := run(t, f.d, args...); code != exitSignalBase+sigintNumber {
			t.Errorf("%v: exit %d, want %d", args, code, exitSignalBase+sigintNumber)
		}
		if calls != 1 {
			t.Errorf("%v: %d tool commands ran, want 1 (stop after the interrupt)", args, calls)
		}
		if strings.Contains(f.out.String(), "not available") {
			t.Errorf("%v: printed results after an interrupt:\n%s", args, f.out.String())
		}
	}
	// A signal already waiting stops ask before any tool runs.
	f := newInfo(t, nil)
	sig := make(chan os.Signal, 1)
	sig <- syscall.SIGTERM
	f.d.Signals = sig
	if code := run(t, f.d, "usage"); code != exitSignalBase+int(syscall.SIGTERM) {
		t.Errorf("waiting SIGTERM: exit %d", code)
	}
}

func TestCodexReadingEdgeCases(t *testing.T) {
	// Within one file the newest timestamp wins, not the last line.
	f := newInfo(t, nil)
	newer := strings.Replace(strings.Replace(rateLine, "12.0", "90.0", 1), "2026-11-07T15:00:00Z", "2026-11-07T16:00:00Z", 1)
	codexSession(t, f.home, "2026/11/07", "rollout-a.jsonl", newer+"\n"+rateLine)
	if code := run(t, f.d, "usage", "codex"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.out.String(), " 90% used") {
		t.Errorf("within-file pick:\n%s", f.out.String())
	}

	// A window with no reset time is left out, not shown as 1970.
	f = newInfo(t, nil)
	noReset := `{"timestamp":"2026-11-07T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":5,"window_minutes":300},"secondary":{"used_percent":38.0,"window_minutes":10080,"resets_at":1794500000}}}}`
	codexSession(t, f.home, "2026/11/07", "rollout-a.jsonl", noReset)
	if code := run(t, f.d, "usage", "codex"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := f.out.String(); strings.Contains(got, "1970") || strings.Contains(got, " 5h ") || !strings.Contains(got, " 7d ") {
		t.Errorf("no-reset window:\n%s", got)
	}

	// A reading older than the 20 newest files is still found.
	f = newInfo(t, nil)
	codexSession(t, f.home, "2026/10/01", "rollout-old.jsonl", rateLine)
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(f.home, ".codex", "sessions", "2026/10/01", "rollout-old.jsonl"), past, past)
	for i := range 25 {
		codexSession(t, f.home, "2026/11/07", fmt.Sprintf("rollout-%02d.jsonl", i), `{"type":"event_msg","payload":{"type":"agent_message"}}`)
	}
	if code := run(t, f.d, "usage", "codex"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.out.String(), " 12% used") {
		t.Errorf("older reading not found:\n%s", f.out.String())
	}
}

func TestRunOutputUsesPrivateFolder(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin"}
	// ls -ld works the same on macOS and Linux; stat's flags do not.
	out, err := runOutput(nil)("/bin/sh", env, "-c", "pwd; ls -ld .")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("output %q", out)
	}
	dir, perms := lines[0], strings.Fields(lines[1])[0]
	if !strings.Contains(filepath.Base(dir), "ask-list-") {
		t.Errorf("working folder %q is not a fresh ask folder", dir)
	}
	if !strings.HasPrefix(perms, "drwx------") {
		t.Errorf("working folder permissions %s, want drwx------", perms)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("working folder %q was not removed: %v", dir, err)
	}
}
