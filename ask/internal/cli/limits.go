package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
	"github.com/chadmayfield/agent-kit/ask/internal/config"
)

// limitRow is one rate limit window. Every tool's data is mapped to this
// shape, so all tools print and encode the same way.
type limitRow struct {
	// Models names the group of models that share the window.
	Models string `json:"models"`
	// Window is "5h" or "7d", from WindowMinutes, or the tool's own name
	// for a window ask does not know.
	Window        string    `json:"window"`
	WindowMinutes int       `json:"window_minutes,omitempty"`
	UsedPercent   float64   `json:"used_percent"`
	LeftPercent   float64   `json:"left_percent"`
	ResetsAt      time.Time `json:"resets_at"`
}

// toolLimits is what ask knows about one tool's limits.
type toolLimits struct {
	Tool      string `json:"tool"`
	Available bool   `json:"available"`
	// Source says where the reading came from.
	Source string `json:"source,omitempty"`
	// AsOf is when the tool reported the reading. Live readings use the
	// time ask asked.
	AsOf    *time.Time `json:"as_of,omitempty"`
	Live    bool       `json:"live,omitempty"`
	Plan    string     `json:"plan,omitempty"`
	Credits string     `json:"credits,omitempty"`
	Note    string     `json:"note,omitempty"`
	Rows    []limitRow `json:"windows,omitempty"`
}

// readLimits returns tool's current limits, or a note on why there are
// none.
func readLimits(tool string, tc config.ToolConfig, home, state string, now time.Time, output func(string, ...string) ([]byte, error)) toolLimits {
	var tl toolLimits
	switch tool {
	case "claude":
		tl = claudeLimits(filepath.Join(state, claudeLimitsFile))
	case "codex":
		tl = codexLimits(filepath.Join(home, ".codex", "sessions"))
	case "agy":
		tl = agyLimits(tc, now, output)
	default:
		tl = toolLimits{Note: "not available"}
	}
	tl.Tool = tool
	sort.SliceStable(tl.Rows, func(i, j int) bool {
		a, b := tl.Rows[i], tl.Rows[j]
		if a.Models != b.Models {
			return a.Models < b.Models
		}
		// Known windows by length, then unknown ones (0 minutes) by name.
		if (a.WindowMinutes == 0) != (b.WindowMinutes == 0) {
			return b.WindowMinutes == 0
		}
		if a.WindowMinutes != b.WindowMinutes {
			return a.WindowMinutes < b.WindowMinutes
		}
		return a.Window < b.Window
	})
	return tl
}

// newRow builds a row from a used percentage and fills in the rest.
func newRow(models string, minutes int, rawWindow string, used float64, resets time.Time) limitRow {
	window := rawWindow
	if minutes > 0 {
		window = windowName(minutes)
	}
	used = min(max(used, 0), 100)
	return limitRow{
		Models: models, Window: window, WindowMinutes: minutes,
		UsedPercent: used, LeftPercent: 100 - used, ResetsAt: resets.UTC(),
	}
}

// agyLimits asks agy for its quota. `agy --print /usage` answers from the
// quota service without starting an agent turn or spending quota.
func agyLimits(tc config.ToolConfig, now time.Time, output func(string, ...string) ([]byte, error)) toolLimits {
	bin, err := resolveBinary("agy", tc)
	if err != nil {
		return toolLimits{Note: "not available: agy is not installed"}
	}
	// An agy older than agyUsageSince would take "/usage" as a prompt and
	// start a real agent turn, so check the version first.
	ver, err := output(bin, "--version")
	if err != nil || !versionAtLeast(string(ver), agyUsageSince) {
		return toolLimits{Note: fmt.Sprintf("not available: needs agy %d.%d.%d or later", agyUsageSince[0], agyUsageSince[1], agyUsageSince[2])}
	}
	out, err := output(bin, "--print", "/usage", "--output-format", "json")
	if err != nil {
		return toolLimits{Note: "not available: agy /usage failed"}
	}
	rows, err := parseAgyUsage(out)
	if err != nil || len(rows) == 0 {
		return toolLimits{Note: "not available: agy /usage gave no limits"}
	}
	asOf := now.UTC()
	return toolLimits{Available: true, Live: true, AsOf: &asOf, Source: "agy /usage", Rows: rows}
}

// agyUsageSince is the first agy release whose print mode answers
// "/usage" itself, without an agent turn (agy changelog, 1.1.11).
var agyUsageSince = [3]int{1, 1, 11}

// versionAtLeast reports whether s names exactly one version, it is not a
// pre-release, and it is at least min. Output with a second version, such
// as an update notice, fails, since ask cannot tell which one is installed.
func versionAtLeast(s string, min [3]int) bool {
	vs := adapter.FindVersions(s)
	if len(vs) != 1 || vs[0].Pre {
		return false
	}
	for i := range 3 {
		if vs[0].Nums[i] != min[i] {
			return vs[0].Nums[i] > min[i]
		}
	}
	return true
}

// agyWindows maps agy's window names to minutes.
var agyWindows = map[string]int{"5h": 300, "weekly": 10080}

// parseAgyUsage reads the groups and buckets in `agy --print /usage
// --output-format json`.
func parseAgyUsage(out []byte) ([]limitRow, error) {
	var resp struct {
		Status  string `json:"status"`
		Command struct {
			Name string `json:"name"`
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						Window            string    `json:"window"`
						RemainingFraction *float64  `json:"remaining_fraction"`
						ResetTime         time.Time `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, errors.New("agy /usage output is not valid JSON")
	}
	if resp.Status != "SUCCESS" || resp.Command.Name != "usage" {
		return nil, errors.New("agy /usage did not succeed")
	}
	var rows []limitRow
	for _, g := range resp.Command.Data.Groups {
		for _, b := range g.Buckets {
			if b.RemainingFraction == nil || b.ResetTime.IsZero() {
				continue
			}
			used := (1 - *b.RemainingFraction) * 100
			rows = append(rows, newRow(g.Name, agyWindows[b.Window], b.Window, used, b.ResetTime))
		}
	}
	return rows, nil
}

// claudeLimitsFile is where a Claude Code status line command saves the
// rate_limits object it receives, in ask's state folder.
const claudeLimitsFile = "claude-limits.json"

// claudeWindows maps Claude Code's window keys to minutes. Any other key
// is shown under its own name.
var claudeWindows = map[string]int{"five_hour": 300, "seven_day": 10080}

// claudeLimits reads the file a status line command writes:
// {"written_at": "<RFC 3339>", "rate_limits": {"<window>": {
// "used_percentage": N, "resets_at": <Unix seconds>}}}. Claude Code sends
// rate_limits only to Pro, Max and Ultra plans, after a model reply, and
// only for the windows the plan has.
func claudeLimits(path string) toolLimits {
	data, err := readSmallFile(path, maxClaudeLimitsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return toolLimits{Note: "not available: no status line reading yet"}
	}
	if err != nil {
		return toolLimits{Note: "not available: " + claudeLimitsFile + " cannot be read"}
	}
	var rec struct {
		WrittenAt  time.Time `json:"written_at"`
		RateLimits map[string]*struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       int64    `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return toolLimits{Note: "not available: " + claudeLimitsFile + " is not valid JSON"}
	}
	var rows []limitRow
	for key, w := range rec.RateLimits {
		if w == nil || w.UsedPercentage == nil || w.ResetsAt <= 0 {
			continue
		}
		rows = append(rows, newRow("all models", claudeWindows[key], key, *w.UsedPercentage, time.Unix(w.ResetsAt, 0)))
	}
	if len(rows) == 0 {
		return toolLimits{Note: "not available: the status line reading has no windows"}
	}
	tl := toolLimits{Available: true, Source: "Claude Code status line", Rows: rows}
	if !rec.WrittenAt.IsZero() {
		asOf := rec.WrittenAt
		tl.AsOf = &asOf
	}
	return tl
}

// codexSessionsChecked is how many of the most recently written session
// files codexLimits compares. When none of them has a reading, it keeps
// looking, up to codexSessionsMax files.
const (
	codexSessionsChecked = 20
	codexSessionsMax     = 200
)

// codexLimits returns the newest rate limit reading, by its own timestamp,
// in the most recently written codex session files. A resumed session
// writes to its old file, so file names and folders do not give the order.
// Session files sit in sessions/YYYY/MM/DD.
func codexLimits(sessions string) toolLimits {
	var best toolLimits
	found := false
	for i, f := range recentFiles(sessions, codexSessionsMax) {
		if i >= codexSessionsChecked && found {
			break
		}
		tl, ok := lastRateLimits(f)
		if ok && (!found || tl.AsOf.After(*best.AsOf)) {
			best, found = tl, true
		}
	}
	if !found {
		return toolLimits{Note: "not available: no codex session with rate limits"}
	}
	return best
}

// recentFiles returns up to n regular .jsonl files three folders below
// dir, most recently written first. Symlinks are skipped.
func recentFiles(dir string, n int) []string {
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	var walk func(string, int)
	walk = func(d string, depth int) {
		ents, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range ents {
			p := filepath.Join(d, e.Name())
			switch {
			case depth < 3 && e.IsDir():
				walk(p, depth+1)
			case depth == 3 && e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl"):
				if fi, err := e.Info(); err == nil {
					files = append(files, file{p, fi.ModTime()})
				}
			}
		}
	}
	walk(dir, 0)
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	out := make([]string, 0, min(n, len(files)))
	for _, f := range files[:min(n, len(files))] {
		out = append(out, f.path)
	}
	return out
}

type codexLimit struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type codexRateEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Primary   *codexLimit `json:"primary"`
			Secondary *codexLimit `json:"secondary"`
			Credits   *struct {
				Balance string `json:"balance"`
			} `json:"credits"`
			PlanType string `json:"plan_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// lastRateLimits returns the newest token_count event, by timestamp, in a
// codex session file that carries rate limits. Windows without a reset
// time are left out.
func lastRateLimits(path string) (toolLimits, bool) {
	f, err := openRegular(path)
	if err != nil {
		return toolLimits{}, false
	}
	defer f.Close()
	var last *codexRateEvent
	err = eachLine(f, maxSessionLine, func(line []byte) {
		if !bytes.Contains(line, []byte(`"rate_limits"`)) {
			return
		}
		var ev codexRateEvent
		if json.Unmarshal(line, &ev) != nil || ev.Payload.Type != "token_count" || ev.Payload.RateLimits == nil {
			return
		}
		// Keep the newest reading by its own timestamp, not the last line,
		// in case the clock went back during the session.
		if last == nil || !ev.Timestamp.Before(last.Timestamp) {
			last = &ev
		}
	})
	if err != nil || last == nil {
		return toolLimits{}, false
	}
	rl := last.Payload.RateLimits
	asOf := last.Timestamp
	tl := toolLimits{Available: true, AsOf: &asOf, Plan: rl.PlanType, Source: "last codex session"}
	for _, l := range []*codexLimit{rl.Primary, rl.Secondary} {
		if l != nil && l.WindowMinutes > 0 && l.ResetsAt > 0 {
			tl.Rows = append(tl.Rows, newRow("all models", l.WindowMinutes, "", l.UsedPercent, time.Unix(l.ResetsAt, 0)))
		}
	}
	if rl.Credits != nil {
		tl.Credits = rl.Credits.Balance
	}
	if len(tl.Rows) == 0 {
		return toolLimits{}, false
	}
	return tl, true
}

// windowName turns a window length into "5h" or "7d".
func windowName(minutes int) string {
	switch {
	case minutes%1440 == 0:
		return fmt.Sprintf("%dd", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

// untilText writes a duration as "3h12m", or "3d6h" from one day up.
func untilText(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	days, hours, mins := m/1440, m%1440/60, m%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// shortCredits rounds a decimal balance such as "125.4969825000" to two
// places. Anything else is returned unchanged.
func shortCredits(s string) string {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return s
}

// File size limits for what ask reads from other tools.
const (
	maxClaudeLimitsFile = 64 << 10
	maxModelCache       = 16 << 20
	// maxSessionLine is the longest codex session line ask reads. Longer
	// lines, such as large tool output, are skipped; rate limit events are
	// small.
	maxSessionLine = 4 << 20
)

// openRegular opens path for reading and fails unless it is a regular file.
// It opens without blocking, so a FIFO in its place cannot hang ask.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	return f, nil
}

// readSmallFile reads a regular file of at most limit bytes.
func readSmallFile(path string, limit int64) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

// eachLine calls fn with each line of r, without its newline. Lines longer
// than limit are skipped.
func eachLine(r io.Reader, limit int, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skipping {
			if len(line)+len(chunk) > limit {
				line, skipping = nil, true
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == nil || err == io.EOF:
			if !skipping && len(line) > 0 {
				fn(bytes.TrimSuffix(line, []byte("\n")))
			}
			line, skipping = line[:0], false
			if err == io.EOF {
				return nil
			}
		default:
			return err
		}
	}
}
