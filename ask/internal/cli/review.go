package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chadmayfield/agent-kit/ask/internal/adapter"
	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/prompt"
	"github.com/chadmayfield/agent-kit/ask/internal/record"
	"github.com/chadmayfield/agent-kit/ask/internal/workspace"
)

// reviewCopyLimits bound the throwaway copy a tool without a verified
// read-only setup reviews.
var reviewCopyLimits = workspace.Limits{MaxBytes: 512 << 20, MaxFiles: 50000}

// reviewChangedShown is how many changed paths the text output lists.
const reviewChangedShown = 10

type reviewOptions struct {
	models  map[string]string
	as      string
	asJSON  bool
	dir     string
	timeout time.Duration
	files   []string
}

// reviewResult is one tool's review. Mode is "read-only" for a run in the
// real folder with a verified read-only setup, or "copy" for a run in a
// throwaway copy with an unverified read-only setup.
type reviewResult struct {
	Tool         string   `json:"tool"`
	Model        string   `json:"model"`
	ModelID      string   `json:"model_id"`
	Mode         string   `json:"mode"`
	Exit         int      `json:"exit"`
	Error        string   `json:"error,omitempty"`
	Final        string   `json:"final"`
	Report       string   `json:"report,omitempty"`
	DurationMS   int64    `json:"duration_ms"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	ChangedFiles []string `json:"changed_files,omitempty"`
}

// addReviewCommand adds `ask review` to root.
func addReviewCommand(root *cobra.Command, d Deps, tools []string) {
	ro := reviewOptions{models: map[string]string{}}
	var claudeModel, codexModel, agyModel string
	cmd := &cobra.Command{
		Use:   "review [prompt...]",
		Short: "Send one prompt to claude, codex and agy at once and show their reviews together",
		RunE: func(_ *cobra.Command, args []string) error {
			for tool, m := range map[string]string{"claude": claudeModel, "codex": codexModel, "agy": agyModel} {
				if m != "" {
					if !oneWord(m) {
						return usageErr("the --%s model must be one word", tool)
					}
					ro.models[tool] = m
				}
			}
			if ro.timeout < 0 {
				return usageErr("--timeout must not be negative")
			}
			return runReview(d, ro, tools, args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&claudeModel, "claude", "", "review with claude and this model")
	f.StringVar(&codexModel, "codex", "", "review with codex and this model")
	f.StringVar(&agyModel, "agy", "", "review with agy and this model")
	f.StringVar(&ro.as, "as", "", "put a preset instruction in front of the prompt: adversarial, code, docs, security, or a kind from the config")
	f.BoolVar(&ro.asJSON, "json", false, "print JSON instead of text")
	f.StringVarP(&ro.dir, "dir", "C", "", "review this folder (default: the current folder)")
	f.DurationVar(&ro.timeout, "timeout", 0, "stop each tool after this long (default: the tool's config value, or 10m)")
	f.StringArrayVarP(&ro.files, "file", "f", nil, "add a file's text to the prompt; repeat for more files")
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: ExitUsage, err: err}
	})
	root.AddCommand(cmd)
}

// runReview runs every picked tool at once on the same prompt and prints
// their answers in tool order. Every tool runs with its read-only setup
// and read-only secrets. A tool whose setup is verified for the installed
// version runs in the real folder; any other runs in a throwaway copy
// whose fingerprint must not change. A signal stops every tool and ends ask with 128+n.
func runReview(d Deps, ro reviewOptions, tools, words []string) error {
	env := d.Env
	if env == nil {
		env = os.Getenv
		d.Env = env
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	cfg, err := config.Load(config.DefaultPath(env))
	if err != nil {
		return setupErr("%v", err)
	}
	picks, err := pickReviewers(ro.models, cfg.Review, tools)
	if err != nil {
		return err
	}
	preset := ""
	if ro.as != "" {
		if preset, err = presetText(ro.as, cfg.Review); err != nil {
			return err
		}
	}

	text, err := reviewPrompt(d, words, ro.files)
	if err != nil {
		return err
	}
	if preset != "" {
		text = preset + "\n\n" + text
	}

	dir := ro.dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return setupErr("find the current folder: %v", err)
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return setupErr("find the review folder: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return usageErr("-C: %s is not a folder", dir)
	}
	state, err := stateDir(env)
	if err != nil {
		return setupErr("%v", err)
	}

	// Pass each signal to every tool, and remember the first one.
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	var firstSig os.Signal
	chans := make([]chan os.Signal, len(picks))
	for i := range chans {
		chans[i] = make(chan os.Signal, 1)
	}
	go func() {
		for {
			select {
			case s, ok := <-d.Signals:
				if !ok {
					return
				}
				once.Do(func() { firstSig = s; close(stop) })
				for _, ch := range chans {
					select {
					case ch <- s:
					default:
					}
				}
			case <-done:
				return
			}
		}
	}()

	results := make([]reviewResult, len(picks))
	var wg sync.WaitGroup
	for i, p := range picks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = reviewOne(d, cfg.Tools[p.Tool], p, dir, text, ro.timeout, chans[i], stop)
		}()
	}
	wg.Wait()
	close(done)

	select {
	case <-stop:
		return signalExit(firstSig)
	default:
	}

	if ro.asJSON {
		report := saveReviewReport(d, state, renderReview(results, false))
		enc := json.NewEncoder(d.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Reviews []reviewResult `json:"reviews"`
			Report  string         `json:"report,omitempty"`
		}{results, report}); err != nil {
			return err
		}
	} else {
		shown := renderReview(results, d.StdoutIsTerminal)
		report := saveReviewReport(d, state, renderReview(results, false))
		fmt.Fprint(d.Stdout, shown)
		if report != "" {
			fmt.Fprintf(d.Stdout, "report: %s\n", report)
		}
	}

	failed := 0
	for _, r := range results {
		if r.Exit != 0 {
			failed++
		}
	}
	if failed > 0 {
		return &exitError{code: ExitTool, err: fmt.Errorf("%d of %d reviews failed", failed, len(results))}
	}
	return nil
}

// reviewPrompt builds the prompt once, reading stdin for a lone "-" the
// way ask does, so every tool gets the same text.
func reviewPrompt(d Deps, words, files []string) (string, error) {
	var stdin io.Reader = strings.NewReader("")
	dashes := 0
	for _, w := range words {
		if w == "-" {
			dashes++
		}
	}
	if !d.StdinIsTerminal && dashes == 1 {
		src := d.Stdin
		if src == nil {
			src = strings.NewReader("")
		}
		data, err := readStdin(src, d.Signals)
		if err != nil {
			return "", err
		}
		stdin = bytes.NewReader(data)
	}
	text, err := prompt.Build(words, files, stdin, d.StdinIsTerminal)
	if err != nil {
		return "", usageErr("%v", err)
	}
	return text, nil
}

// reviewOne runs one tool and returns its result. It never returns an
// error: failures are recorded in the result.
func reviewOne(d Deps, tc config.ToolConfig, p reviewPick, dir, text string, timeout time.Duration, signals chan os.Signal, stop <-chan struct{}) reviewResult {
	res := reviewResult{Tool: p.Tool, Model: p.Model, ModelID: adapter.ResolveModel(p.Model, tc), Mode: "read-only"}
	fail := func(err error) reviewResult {
		res.Exit, res.Error = exitCode(err), err.Error()
		return res
	}
	select {
	case <-stop:
		return fail(errors.New("stopped before it started"))
	default:
	}
	a := d.Adapters[p.Tool]
	bin, err := resolveBinary(p.Tool, tc)
	if err != nil {
		return fail(err)
	}
	version := d.Version
	if version == nil {
		version = func(string) (string, error) { return "", errors.New("no version check") }
	}

	runDir, unverified := dir, false
	var before map[string]string
	if checkReadOnly(a, p.Tool, bin, version) != nil {
		res.Mode = "copy"
		copyDir, cleanup, err := workspace.Copy(dir, reviewCopyLimits)
		if err != nil {
			return fail(adapter.Refuse("%s: %v", p.Tool, err))
		}
		defer cleanup()
		if before, err = workspace.Fingerprint(copyDir); err != nil {
			return fail(setupErr("%v", err))
		}
		runDir, unverified = copyDir, true
	}

	var out, errb bytes.Buffer
	dd := d
	dd.Stdin = strings.NewReader(text)
	dd.StdinIsTerminal = false
	dd.Stdout, dd.Stderr = &out, &errb
	dd.StdoutIsTerminal, dd.StderrIsTerminal = false, false
	dd.Signals = signals
	// A tool whose read-only setup is unverified still runs with that
	// setup and its read-only secrets, never --write: in the copy, the
	// fingerprint catches the setup failing, and nothing gets the wider
	// write-mode access.
	o := options{dir: runDir, timeout: timeout, probeReadOnly: unverified}
	st, runErr := runAskStats(dd, o, []string{p.Tool, p.Model, "-"})
	res.Final = strings.TrimRight(st.final, "\n")
	res.Report = st.report
	res.DurationMS = st.duration.Milliseconds()
	res.InputTokens, res.OutputTokens = st.inTok, st.outTok
	if runErr != nil {
		res.Exit, res.Error = exitCode(runErr), runErr.Error()
	}

	if before != nil {
		after, err := workspace.Fingerprint(runDir)
		if err != nil {
			res.Exit, res.Error = ExitTool, err.Error()
			return res
		}
		if changed := workspace.Changed(before, after); len(changed) > 0 {
			res.ChangedFiles = changed
			if res.Exit == 0 {
				res.Exit = ExitTool
			}
			msg := "changed files in its copy: " + shortList(changed, reviewChangedShown)
			if res.Error == "" {
				res.Error = msg
			} else {
				res.Error += "; " + msg
			}
		}
	}
	return res
}

// shortList joins up to n items and says how many more there are.
func shortList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

// renderReview writes each tool's answer under a header, then a summary.
// With term set, tool text is made safe for a terminal.
func renderReview(results []reviewResult, term bool) string {
	clean := func(s string) string {
		if term {
			return forTerminal(s, nil)
		}
		return s
	}
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "== %s · %s ==\n", r.Tool, clean(r.Model))
		switch {
		case r.Final != "":
			b.WriteString(clean(r.Final))
			b.WriteString("\n")
		default:
			b.WriteString("(no answer)\n")
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", clean(r.Error))
		}
		b.WriteString("\n")
	}
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tMODEL\tRAN IN\tEXIT\tTIME\tTOKENS\tREPORT")
	for _, r := range results {
		report := r.Report
		if report == "" {
			report = "-"
		}
		took := (time.Duration(r.DurationMS) * time.Millisecond).Round(100 * time.Millisecond)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s in · %s out\t%s\n", r.Tool, clean(r.ModelID), r.Mode, r.Exit, took,
			shortCount(r.InputTokens), shortCount(r.OutputTokens), report)
	}
	_ = tw.Flush()
	return b.String()
}

// saveReviewReport saves the combined review and returns its path, or ""
// with a warning on stderr when it cannot.
func saveReviewReport(d Deps, state, text string) string {
	p, err := record.SaveReport(filepath.Join(state, "reports"), d.Now(), "review", d.Pid, text)
	if err != nil {
		fmt.Fprintf(d.Stderr, "ask: warning: %v\n", err)
		return ""
	}
	return p
}
