package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// barWidth is how many cells a usage meter has.
const barWidth = 20

// style adds ANSI colors when color is on, and nothing otherwise. rich
// means the terminal has 256 colors, which orange needs.
type style struct{ color, rich bool }

// richColor reports whether the terminal named by env has 256 colors:
// COLORTERM is truecolor or 24bit, or TERM names 256color or direct.
func richColor(env func(string) string) bool {
	ct := strings.ToLower(env("COLORTERM"))
	term := strings.ToLower(env("TERM"))
	return ct == "truecolor" || ct == "24bit" ||
		strings.Contains(term, "256color") || strings.Contains(term, "direct")
}

func (s style) wrap(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) red(t string) string    { return s.wrap("31", t) }
func (s style) yellow(t string) string { return s.wrap("33", t) }
func (s style) green(t string) string  { return s.wrap("32", t) }

// orange is 256-color orange, or yellow on a terminal without 256 colors.
func (s style) orange(t string) string {
	if !s.rich {
		return s.yellow(t)
	}
	return s.wrap("38;5;208", t)
}

// level colors text by how much of a window is used: green below 50%,
// yellow below 70%, orange below 90% and red from 90%.
func (s style) level(used float64, t string) string {
	switch {
	case used >= 90:
		return s.red(t)
	case used >= 70:
		return s.orange(t)
	case used >= 50:
		return s.yellow(t)
	}
	return s.green(t)
}

// pad pads t with spaces to w visible characters.
func pad(t string, w int) string {
	if n := utf8.RuneCountInString(t); n < w {
		return t + strings.Repeat(" ", w-n)
	}
	return t
}

// meter draws a bar of barWidth cells, filled for the used share.
func meter(used float64) (filled, empty string) {
	n := int(used/100*barWidth + 0.5)
	n = min(max(n, 0), barWidth)
	if used > 0 && n == 0 {
		n = 1
	}
	return strings.Repeat("█", n), strings.Repeat("░", barWidth-n)
}

// shortCount writes a token count as 512, 16.5k or 1.25M.
func shortCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// writeUsage prints one block per tool: a header with where the reading
// came from, a meter row per window and, when runs is not nil, the tool's
// ask runs. Tools with limits come first; the rest follow, one line each
// unless they have runs. The output starts with a blank line.
func writeUsage(w io.Writer, limits []toolLimits, runs []runTotal, runsTitle string, now time.Time, term bool, st style) {
	clean := func(t string) string {
		if term {
			return forTerminal(t, nil)
		}
		return t
	}

	order := make([]toolLimits, len(limits))
	copy(order, limits)
	sort.SliceStable(order, func(i, j int) bool { return order[i].Available && !order[j].Available })

	// One width for every tool's columns, so the meters line up.
	toolW, modelW, winW := 0, 0, 0
	for _, tl := range order {
		toolW = max(toolW, utf8.RuneCountInString(tl.Tool))
		for _, r := range tl.Rows {
			modelW = max(modelW, utf8.RuneCountInString(clean(r.Models)))
			winW = max(winW, utf8.RuneCountInString(clean(r.Window)))
		}
	}

	fmt.Fprintln(w)
	for i, tl := range order {
		var tr []runTotal
		for _, r := range runs {
			if r.Tool == tl.Tool {
				tr = append(tr, r)
			}
		}
		if i > 0 && (tl.Available || len(tr) > 0 || order[i-1].Available) {
			fmt.Fprintln(w)
		}
		head := " " + st.bold(pad(tl.Tool, toolW)) + "  "
		if !tl.Available {
			fmt.Fprintln(w, head+st.dim(clean(tl.Note)))
		} else {
			fmt.Fprintln(w, head+st.dim(strings.Join(headerParts(tl, clean), " · ")))
			writeWindows(w, st, tl.Rows, modelW, winW, now, clean)
		}
		switch {
		case runs != nil && len(tr) > 0:
			writeRuns(w, st, runsTitle, tr, clean)
		case runs != nil && tl.Available:
			fmt.Fprintln(w, "   "+st.dim(runsTitle+": none"))
		}
	}
}

// headerParts lists what a tool's header line says after its name.
func headerParts(tl toolLimits, clean func(string) string) []string {
	var parts []string
	if tl.Plan != "" {
		parts = append(parts, "plan "+clean(tl.Plan))
	}
	if tl.Credits != "" {
		parts = append(parts, "credits "+clean(shortCredits(tl.Credits)))
	}
	switch {
	case tl.Live:
		parts = append(parts, "live")
	case tl.AsOf != nil:
		parts = append(parts, "as of "+tl.AsOf.Local().Format("Jan 2 15:04"))
	default:
		parts = append(parts, "as of unknown")
	}
	if tl.Source != "" {
		parts = append(parts, "from "+tl.Source)
	}
	return parts
}

// writeWindows prints one meter row per window. A model group's name shows
// on its first row only.
func writeWindows(w io.Writer, st style, rows []limitRow, modelW, winW int, now time.Time, clean func(string) string) {
	prev := ""
	for _, r := range rows {
		name := clean(r.Models)
		shown := name
		if name == prev {
			shown = ""
		}
		prev = name
		filled, empty := meter(r.UsedPercent)
		used := fmt.Sprintf("%3.0f%% used", r.UsedPercent)
		left := fmt.Sprintf("%3.0f%% left", r.LeftPercent)
		fmt.Fprintf(w, "   %s  %s  %s%s  %s  %s  %s\n",
			pad(shown, modelW), pad(clean(r.Window), winW),
			st.level(r.UsedPercent, filled), st.dim(empty),
			st.level(r.UsedPercent, used), st.dim(left), resetPart(st, r.ResetsAt, now))
	}
}

// resetPart says when a window resets, or warns that it already has, which
// means the reading is out of date.
func resetPart(st style, at, now time.Time) string {
	local := at.Local().Format("Jan 2 15:04")
	if !at.After(now) {
		return st.yellow("reset " + local + " · reading is old")
	}
	return "resets " + local + st.dim(" · in "+untilText(at.Sub(now)))
}

// writeRuns prints a tool's ask runs, one row per model.
func writeRuns(w io.Writer, st style, title string, runs []runTotal, clean func(string) string) {
	modelW, countW, tokW, tookW := 0, 0, 0, 0
	tokens := make([]string, len(runs))
	counts := make([]string, len(runs))
	took := make([]string, len(runs))
	for i, r := range runs {
		modelW = max(modelW, utf8.RuneCountInString(clean(r.Model)))
		counts[i] = fmt.Sprintf("%d run", r.Runs)
		if r.Runs != 1 {
			counts[i] += "s"
		}
		countW = max(countW, len(counts[i]))
		tokens[i] = shortCount(r.InputTokens) + " in · " + shortCount(r.OutputTokens) + " out"
		tokW = max(tokW, utf8.RuneCountInString(tokens[i]))
		took[i] = (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second).String()
		tookW = max(tookW, len(took[i]))
	}
	fmt.Fprintln(w, "   "+st.dim(title))
	for i, r := range runs {
		var trouble []string
		if r.Failed > 0 {
			trouble = append(trouble, fmt.Sprintf("%d failed", r.Failed))
		}
		if r.Guarded > 0 {
			trouble = append(trouble, plural(r.Guarded, "guard stop"))
		}
		if r.NotStarted > 0 {
			trouble = append(trouble, fmt.Sprintf("%d not started", r.NotStarted))
		}
		line := fmt.Sprintf("     %s  %s  %s", pad(clean(r.Model), modelW),
			pad(counts[i], countW), pad(tokens[i], tokW))
		if len(trouble) > 0 {
			line += "  " + st.dim(pad(took[i], tookW)) + "  " + st.yellow(strings.Join(trouble, " · "))
		} else {
			line += "  " + st.dim(took[i])
		}
		fmt.Fprintln(w, line)
	}
}

// plural writes "1 guard stop" or "2 guard stops".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
