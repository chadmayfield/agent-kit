// Package cli builds the ask command and maps its errors to exit codes.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/chadmayfield/agent-kit/internal/adapter"
	"github.com/chadmayfield/agent-kit/internal/config"
	"github.com/chadmayfield/agent-kit/internal/prompt"
	"github.com/chadmayfield/agent-kit/internal/record"
	"github.com/chadmayfield/agent-kit/internal/runner"
	"github.com/chadmayfield/agent-kit/internal/secret"
)

// Exit codes shared by every tool.
const (
	ExitOK      = 0
	ExitTool    = 1
	ExitUsage   = 2
	ExitSetup   = 3
	ExitTimeout = 124
)

// Deps is everything ask reads from the outside world, so tests can swap it.
type Deps struct {
	Stdin           io.Reader
	StdinIsTerminal bool
	Stdout, Stderr  io.Writer
	// StdoutIsTerminal and StderrIsTerminal make ask remove control
	// characters from tool text printed there. Pipes and files get it
	// unchanged.
	StdoutIsTerminal, StderrIsTerminal bool
	Env                                func(string) string
	Now                                func() time.Time
	Pid                                int
	Adapters                           map[string]adapter.Adapter
	Signals                            <-chan os.Signal
	// Version returns the output of `<binary> --version`.
	Version func(binary string) (string, error)
	// Argv0 is the name ask was started as. ask-<tool> implies the tool.
	Argv0 string
	// Environ lists the whole environment as KEY=value pairs. Nil means
	// os.Environ.
	Environ func() []string
	// Resolve looks up one secret value. Nil means secret.Resolve.
	Resolve func(config.SecretRef) (string, error)
}

// exitError carries an exit code chosen by ask itself.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, args ...any) error {
	return &exitError{code: ExitUsage, err: fmt.Errorf(format, args...)}
}

func setupErr(format string, args ...any) error {
	return &exitError{code: ExitSetup, err: fmt.Errorf(format, args...)}
}

type options struct {
	write    bool
	dir      string
	timeout  time.Duration
	effort   string
	final    string
	dryRun   bool
	noReport bool
	verbose  bool
	files    []string
	// probeReadOnly skips the read-only gate. Only a readonlyprobe build
	// declares its flag, so it is always false in the default build.
	probeReadOnly bool
}

// registerProbe, when set by a build-tagged file, declares extra flags.
var registerProbe func(cmd *cobra.Command, o *options)

const longHelp = `ask sends one prompt to a coding agent CLI and prints its final message.

The prompt is the words after the model, plus any files given with -f.
Pass a lone - to also read the prompt from stdin. Without -, ask never
reads stdin. The prompt never goes on the CLI's command line.

Flags may appear anywhere. A -- ends the flags, so every word after it is
prompt text, even one that looks like a flag:
  ask codex some-model -- --verbose is a flag name

Without --write, ask runs a tool only if its read-only setup passed its
test on the installed version. Otherwise it refuses.

Tools: %s

Exit codes:
  0      ok
  1      the tool failed, gave no final message, or --final found no match
  2      bad arguments, or ask refused the run
  3      ask's own setup failed (config, secret lookup, missing binary or
         report save)
  124    timeout
  128+n  the tool or ask was killed by signal n

Full guide: docs/ask/README.md in the agent-kit repo. Man page: man ask
(source: docs/man/ask.1).`

// NewRootCmd builds the ask command.
func NewRootCmd(d Deps) *cobra.Command {
	var o options

	names := make([]string, 0, len(d.Adapters))
	for name := range d.Adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := strings.Join(names, ", ")
	alias := aliasTool(d.Argv0)
	use := "ask <tool> <model> [prompt...]"
	if alias != "" {
		use = "ask-" + alias + " <model> [prompt...]"
	}
	if tools == "" {
		tools = "none in this build"
	}

	cmd := &cobra.Command{
		Use:           use,
		Short:         "Send one prompt to a coding agent CLI",
		Long:          fmt.Sprintf(longHelp, tools),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if alias != "" {
				if len(args) < 1 {
					return usageErr("need a model; run %s --help for usage", "ask-"+alias)
				}
				return nil
			}
			if len(args) < 2 {
				return usageErr("need a tool and a model; run ask --help for usage")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if alias != "" {
				args = append([]string{alias}, args...)
			}
			tool := args[0]
			if _, ok := d.Adapters[tool]; !ok {
				return usageErr("unknown tool %q; known tools: %s", tool, tools)
			}
			if !oneWord(args[1]) {
				return usageErr("the model must be one word, for example ask claude haiku ...")
			}
			return runAsk(d, o, args)
		},
	}
	cmd.SetIn(d.Stdin)
	cmd.SetOut(d.Stdout)
	cmd.SetErr(d.Stderr)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: ExitUsage, err: err}
	})
	// Disable cobra's completion subcommand; ask's first argument is a tool.
	cmd.CompletionOptions.DisableDefaultCmd = true

	f := cmd.Flags()
	f.BoolVar(&o.write, "write", false, "let the tool edit files and run MCP servers and hooks")
	f.StringVarP(&o.dir, "dir", "C", "", "run the tool in this folder (default: the current folder)")
	f.DurationVar(&o.timeout, "timeout", 0, "stop the tool after this long (default: the tool's config value, or 10m)")
	f.StringVar(&o.effort, "effort", "", "reasoning effort, for tools that support it")
	f.StringVar(&o.final, "final", "", "print only the last line matching this regular expression, plus the report path")
	f.BoolVar(&o.dryRun, "dry-run", false, "print the command and run nothing; secrets show as config key names, the prompt as its length")
	f.BoolVar(&o.noReport, "no-report", false, "do not save the final message to a report file")
	f.BoolVar(&o.verbose, "verbose", false, "print tool, model, model ID, reported model, duration, tokens, exit code and report path on stderr; after a failed run, also the last 4 KiB of the tool's stderr, with secrets redacted")
	f.StringArrayVarP(&o.files, "file", "f", nil, "add a file's text to the prompt; repeat for more files")
	if registerProbe != nil {
		registerProbe(cmd, &o)
	}

	return cmd
}

// Execute runs cmd, prints any error on its stderr, and returns the exit code.
func Execute(cmd *cobra.Command) int {
	err := cmd.Execute()
	if err == nil {
		return ExitOK
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "ask: %v\n", err)
	return exitCode(err)
}

func exitCode(err error) int {
	var refused *adapter.RefusedError
	if errors.As(err, &refused) {
		return ExitUsage
	}
	var setup *adapter.SetupError
	if errors.As(err, &setup) {
		return ExitSetup
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return ExitTool
}

// Exit codes for a signal: 128 plus the signal number.
const (
	exitSignalBase = 128
	sigintNumber   = 2
)

// defaultTimeout applies when neither --timeout nor the config sets one.
const defaultTimeout = 10 * time.Minute

// defaultReportDays is how long reports are kept when the config does not
// say.
const defaultReportDays = 30

// baseEnvNames are the variables a child CLI gets from ask's environment,
// besides every XDG_* variable, the config's pass_env and the secrets.
var baseEnvNames = []string{"PATH", "HOME", "USER", "LANG", "TERM", "TMPDIR", "SHELL"}

// BaseEnv returns the entries of environ that every child gets: the
// variables in baseEnvNames and every XDG_* variable.
func BaseEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(baseEnvNames, name) || strings.HasPrefix(name, "XDG_") {
			out = append(out, kv)
		}
	}
	return out
}

// aliasTools are the tools that have an ask-<tool> program name.
var aliasTools = []string{"codex", "agy", "opencode", "claude"}

// aliasTool returns the tool named by an ask-<tool> program name, or "".
// Only the names in aliasTools count, so a name such as ask-check is not
// an alias.
func aliasTool(argv0 string) string {
	tool, ok := strings.CutPrefix(filepath.Base(argv0), "ask-")
	if !ok || !slices.Contains(aliasTools, tool) {
		return ""
	}
	return tool
}

// maxStdin is the most prompt text ask reads from stdin.
const maxStdin = 16 << 20

// readStdin reads all of r, up to maxStdin bytes, while it watches
// signals. A signal ends the wait at once with 128+n.
func readStdin(r io.Reader, signals <-chan os.Signal) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, maxStdin+1))
		got <- result{data, err}
	}()
	for {
		select {
		case res := <-got:
			if res.err != nil {
				return nil, setupErr("read stdin: %v", res.err)
			}
			if len(res.data) > maxStdin {
				return nil, usageErr("stdin is larger than 16 MiB")
			}
			return res.data, nil
		case s, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			return nil, signalExit(s)
		}
	}
}

// resolveBinary returns the absolute path of the tool's CLI and checks
// that it is an executable file. Errors never name the path.
func resolveBinary(tool string, tc config.ToolConfig) (string, error) {
	bin, err := config.BinaryFor(tool, tc)
	if err != nil {
		return "", &adapter.SetupError{Err: err}
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return "", &adapter.SetupError{Err: fmt.Errorf("%s: the binary is missing or not executable", tool)}
	}
	return bin, nil
}

// buildErr maps an adapter Build error: a refusal stays a refusal (exit 2),
// anything else is ask's own setup failing (exit 3).
func buildErr(err error) error {
	var refused *adapter.RefusedError
	if errors.As(err, &refused) {
		return err
	}
	return &exitError{code: ExitSetup, err: err}
}

// secretsFor returns the secrets a run passes: all of them with --write,
// and only those named in ReadOnlySecrets without it.
func secretsFor(tc config.ToolConfig, write bool) []config.SecretRef {
	if write {
		return tc.Secrets
	}
	var out []config.SecretRef
	for _, s := range tc.Secrets {
		if slices.Contains(tc.ReadOnlySecrets, s.Name) {
			out = append(out, s)
		}
	}
	return out
}

// stateDir returns $XDG_STATE_HOME/ask, or $HOME/.local/state/ask.
func stateDir(env func(string) string) (string, error) {
	base := env("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home := env("HOME")
		if !filepath.IsAbs(home) {
			return "", errors.New("no state folder; set HOME or XDG_STATE_HOME to an absolute path")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ask"), nil
}

// checkReadOnly refuses a read-only run unless the adapter's read-only
// setup passed its test on the installed version of the CLI.
func checkReadOnly(a adapter.Adapter, tool, bin string, version func(string) (string, error)) error {
	ro := a.ReadOnly()
	if !ro.OK {
		return adapter.Refuse("read-only setup for %s is not verified; ask refuses to run it without --write", tool)
	}
	ver, err := version(bin)
	if err != nil {
		return adapter.Refuse("cannot read the %s version, so its read-only test does not apply", tool)
	}
	if ver != ro.TestedVersion {
		return adapter.Refuse("%s version %q is not the tested %q; read-only is refused until it is tested again", tool, ver, ro.TestedVersion)
	}
	return nil
}

// childEnv builds the child's whole environment: the base variables, the
// config's pass_env, then the secrets. A later entry replaces an earlier
// one with the same name.
func childEnv(environ []string, passEnv []string, secrets map[string]string) []string {
	vals := map[string]string{}
	var order []string
	set := func(name, val string) {
		if _, seen := vals[name]; !seen {
			order = append(order, name)
		}
		vals[name] = val
	}
	for _, kv := range BaseEnv(environ) {
		name, val, _ := strings.Cut(kv, "=")
		set(name, val)
	}
	for _, name := range passEnv {
		for _, kv := range environ {
			if n, val, ok := strings.Cut(kv, "="); ok && n == name {
				set(name, val)
			}
		}
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		set(name, secrets[name])
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+vals[name])
	}
	return out
}

// redact replaces each non-empty secret value in text with
// <redacted:KEY>, longest value first. A short value can also hide
// ordinary text.
func redact(text string, byKey map[string]string) string {
	keys := make([]string, 0, len(byKey))
	for k, v := range byKey {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(byKey[keys[i]]) != len(byKey[keys[j]]) {
			return len(byKey[keys[i]]) > len(byKey[keys[j]])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		text = strings.ReplaceAll(text, byKey[k], "<redacted:"+k+">")
	}
	return text
}

// pendingSignal returns an exit error for 128+n when a signal is already
// waiting on signals, and nil otherwise. It never blocks.
func pendingSignal(signals <-chan os.Signal) error {
	select {
	case s, ok := <-signals:
		if !ok {
			return nil
		}
		return signalExit(s)
	default:
		return nil
	}
}

// signalExit returns the exit error for s: 128 plus its number, or 130
// when s is not a Unix signal.
func signalExit(s os.Signal) error {
	n := sigintNumber
	if sig, isSys := s.(syscall.Signal); isSys && sig > 0 {
		n = int(sig)
	}
	return &exitError{code: exitSignalBase + n, err: errors.New("interrupted")}
}

// stderrShown is how much of the CLI's stderr --verbose prints after a
// failed run.
const stderrShown = 4096

// lastMatch returns the last line of text that re matches.
func lastMatch(re *regexp.Regexp, text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSuffix(lines[i], "\r")
		if re.MatchString(line) {
			return line, true
		}
	}
	return "", false
}

// runStats is what one run records for the log.
type runStats struct {
	duration      time.Duration
	inTok, outTok int
	// modelID is the model string passed to the CLI.
	modelID string
	// reportedModel is the model the CLI named in its output, or empty.
	reportedModel string
	// dryRun is true for a --dry-run, which starts nothing and is not
	// logged.
	dryRun bool
}

// runAsk runs the tool and appends one log entry for the exit, whatever
// it was. The tool and model are known here; the prompt never goes in the
// log.
func runAsk(d Deps, o options, args []string) error {
	var st runStats
	err := runTool(d, o, args, &st)
	if st.dryRun {
		return err
	}
	env := d.Env
	if env == nil {
		env = os.Getenv
	}
	state, serr := stateDir(env)
	if serr != nil {
		return err
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	entry := record.LogEntry{
		Time: now().UTC(), Tool: args[0], Model: args[1],
		ModelID: st.modelID, ReportedModel: st.reportedModel,
		DurationMS: st.duration.Milliseconds(), Exit: ExitOK,
		InputTokens: st.inTok, OutputTokens: st.outTok,
	}
	if err != nil {
		entry.Exit = exitCode(err)
	}
	if lerr := record.AppendLog(filepath.Join(state, "log.jsonl"), entry); lerr != nil {
		fmt.Fprintf(d.Stderr, "ask: warning: %v\n", lerr)
	}
	return err
}

// runTool is the whole run path for one tool. args is tool, model, then
// the prompt words. It fills st for the log.
func runTool(d Deps, o options, args []string, st *runStats) error {
	tool, model, words := args[0], args[1], args[2:]
	a := d.Adapters[tool]
	st.dryRun = o.dryRun

	if o.probeReadOnly && o.write {
		return usageErr("--probe-readonly cannot be used with --write")
	}
	var finalRE *regexp.Regexp
	if o.final != "" {
		if o.noReport {
			return usageErr("--final prints the report path, so it cannot be used with --no-report")
		}
		re, err := regexp.Compile(o.final)
		if err != nil {
			return usageErr("--final: %v", err)
		}
		finalRE = re
	}
	if o.timeout < 0 {
		return usageErr("--timeout must not be negative")
	}

	version := d.Version
	if version == nil {
		version = func(string) (string, error) { return "", errors.New("no version check") }
	}
	resolve := d.Resolve
	if resolve == nil {
		resolve = secret.Resolve
	}
	env := d.Env
	if env == nil {
		env = os.Getenv
	}
	environ := os.Environ
	if d.Environ != nil {
		environ = d.Environ
	}
	cfg, err := config.Load(config.DefaultPath(env))
	if err != nil {
		return setupErr("%v", err)
	}
	tc := cfg.Tools[tool]
	modelID := adapter.ResolveModel(model, tc)
	st.modelID = modelID
	state, err := stateDir(env)
	if err != nil {
		return setupErr("%v", err)
	}

	if err := adapter.CheckEffort(a, o.effort); err != nil {
		return err
	}
	if !a.Prompt().OK {
		return adapter.Refuse("passing the prompt to %s off the command line is not verified", tool)
	}
	// With one lone "-" and a piped stdin, read stdin here, where a
	// signal can end the wait and the size is capped. Otherwise
	// prompt.Build refuses without reading.
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
			return err
		}
		stdin = bytes.NewReader(data)
	}
	text, err := prompt.Build(words, o.files, stdin, d.StdinIsTerminal)
	if err != nil {
		return usageErr("%v", err)
	}

	dir := o.dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return setupErr("find the current folder: %v", err)
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return setupErr("find the working folder: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return usageErr("-C: %s is not a folder", dir)
	}

	// A missing binary is a setup failure (exit 3), checked before the
	// read-only gate can refuse (exit 2). The checked absolute path is the
	// one that runs.
	bin, err := resolveBinary(tool, tc)
	if err != nil {
		return err
	}
	// Build gets the checked absolute path. BinaryFor returns an absolute
	// path unchanged, so an adapter that calls it runs exactly this file.
	tc.Binary = bin
	if o.probeReadOnly && o.dryRun {
		fmt.Fprintln(d.Stderr, "read-only: probe, gate skipped")
	}
	if !o.write && !o.probeReadOnly {
		if o.dryRun {
			// A dry run starts nothing, not even `--version`.
			if ro := a.ReadOnly(); ro.OK {
				fmt.Fprintf(d.Stderr, "read-only: verified for %q\n", ro.TestedVersion)
			} else {
				fmt.Fprintln(d.Stderr, "read-only: not verified")
			}
		} else {
			if err := pendingSignal(d.Signals); err != nil {
				return err
			}
			gateErr := checkReadOnly(a, tool, bin, version)
			// A signal during the version check wins over its result.
			if err := pendingSignal(d.Signals); err != nil {
				return err
			}
			if gateErr != nil {
				return gateErr
			}
		}
	}

	req := adapter.Request{Model: model, Prompt: text, Dir: dir, Effort: o.effort, Write: o.write}
	secrets := secretsFor(tc, o.write)

	if o.dryRun {
		var labels []string
		for _, s := range secrets {
			labels = append(labels, s.EnvVar+"="+s.Name)
		}
		for _, name := range tc.PassEnv {
			labels = append(labels, name+"=")
		}
		line, err := adapter.DryRun(a, req, tc, labels)
		if err != nil {
			return buildErr(err)
		}
		fmt.Fprintln(d.Stdout, line)
		return nil
	}

	if err := pendingSignal(d.Signals); err != nil {
		return err
	}
	values := make(map[string]string, len(secrets))
	byKey := make(map[string]string, len(secrets))
	for _, s := range secrets {
		v, err := resolve(s)
		if err != nil {
			// A signal during the lookup wins over its failure.
			if sigErr := pendingSignal(d.Signals); sigErr != nil {
				return sigErr
			}
			return setupErr("%v", err)
		}
		values[s.EnvVar] = v
		byKey[s.Name] = v
	}
	if err := pendingSignal(d.Signals); err != nil {
		return err
	}
	childEnviron := childEnv(environ(), tc.PassEnv, values)

	timeout := o.timeout
	if timeout == 0 && tc.TimeoutSeconds > 0 {
		timeout = time.Duration(tc.TimeoutSeconds) * time.Second
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}

	// Read-only runs get one retry when the CLI fails with no answer.
	attempts := 1
	if !o.write {
		attempts = 2
	}
	var (
		out      runner.Outcome
		final    string
		parseErr error
		inTok    int
		outTok   int
		reported string
		duration time.Duration
		early    error
		ran      bool
	)
	defer func() {
		st.duration, st.inTok, st.outTok = duration, inTok, outTok
		st.reportedModel = reported
	}()
	for i := 0; i < attempts; i++ {
		if early = pendingSignal(d.Signals); early != nil {
			if i == 0 {
				return early
			}
			break
		}
		inv, err := a.Build(req, tc)
		if err != nil {
			return buildErr(err)
		}
		inv.Env = childEnviron
		out, err = runner.Run(context.Background(), inv, timeout, d.Signals)
		if err != nil {
			return setupErr("%v", err)
		}
		ran = true
		duration += out.Duration
		res, perr := a.Parse(out.Stdout)
		parseErr = perr
		final = res.Final
		if perr != nil {
			final = ""
		}
		inTok += res.InputTokens
		outTok += res.OutputTokens
		if res.Model != "" {
			reported = redact(res.Model, byKey)
		}
		stopped := out.TimedOut || out.Interrupted || out.Signal > 0
		if stopped || out.ExitCode == 0 || final != "" {
			break
		}
		if i+1 < attempts && o.verbose {
			fmt.Fprintf(d.Stderr, "ask: %s exited with status %d and no answer; retrying once\n", tool, out.ExitCode)
		}
	}

	final = redact(final, byKey)

	code := ExitOK
	var runErr error
	var ee *exitError
	switch {
	case errors.As(early, &ee):
		// A signal arrived between the first attempt and the retry.
		code, runErr = ee.code, ee.err
		final = ""
	case out.TimedOut:
		code, runErr = ExitTimeout, fmt.Errorf("%s timed out after %v", tool, timeout)
	case out.Interrupted:
		code = exitSignalBase + sigintNumber
		if out.Signal > 0 {
			code = exitSignalBase + out.Signal
		}
		runErr = errors.New("interrupted")
	case out.Signal > 0:
		code, runErr = exitSignalBase+out.Signal, fmt.Errorf("%s was killed by signal %d", tool, out.Signal)
	case out.ExitCode != 0:
		code, runErr = ExitTool, fmt.Errorf("%s exited with status %d", tool, out.ExitCode)
	case parseErr != nil:
		code, runErr = ExitTool, parseErr
		if msg := parseErr.Error(); !strings.HasPrefix(msg, tool+":") && !strings.HasPrefix(msg, tool+" ") {
			runErr = fmt.Errorf("%s: %v", tool, parseErr)
		}
	case final == "":
		code, runErr = ExitTool, errors.New("no final message")
	}

	if o.verbose && ran && code != ExitOK {
		showStderr(d.Stderr, d.StderrIsTerminal, tool, out, byKey)
	}

	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	report := ""
	if !o.noReport && final != "" {
		reports := filepath.Join(state, "reports")
		p, err := record.SaveReport(reports, now(), tool, d.Pid, final)
		if err != nil {
			fmt.Fprintf(d.Stderr, "ask: %v\n", err)
			if code == ExitOK {
				code, runErr = ExitSetup, errors.New("the report was not saved")
			}
		} else {
			report = p
		}
		days := cfg.ReportDays
		if days == 0 {
			days = defaultReportDays
		}
		if err := record.Prune(reports, now(), days); err != nil {
			fmt.Fprintf(d.Stderr, "ask: warning: %v\n", err)
		}
	}

	switch {
	case finalRE != nil && final != "":
		line, ok := lastMatch(finalRE, final)
		switch {
		case !ok:
			if code == ExitOK {
				code, runErr = ExitTool, fmt.Errorf("no line of the final message matched --final %q", o.final)
			}
		case report != "":
			if d.StdoutIsTerminal {
				line = forTerminal(line, byKey)
			}
			fmt.Fprintf(d.Stdout, "%s\n%s\n", line, report)
		}
	case final != "" && finalRE == nil:
		shown := final
		if d.StdoutIsTerminal {
			shown = forTerminal(final, byKey)
		}
		fmt.Fprint(d.Stdout, shown)
		if !strings.HasSuffix(shown, "\n") {
			fmt.Fprintln(d.Stdout)
		}
	}

	if o.verbose {
		shown := report
		if shown == "" {
			shown = "none"
		}
		shownReported := reported
		if shownReported == "" {
			shownReported = "none"
		}
		shownID := modelID
		if d.StderrIsTerminal {
			shownReported = forTerminal(shownReported, byKey)
			shownID = forTerminal(shownID, byKey)
		}
		fmt.Fprintf(d.Stderr, "ask: tool=%s model=%s model_id=%s reported_model=%s duration=%s tokens=%d/%d exit=%d report=%s\n",
			tool, model, shownID, shownReported, duration.Round(time.Millisecond), inTok, outTok, code, shown)
	}

	if code == ExitOK {
		return nil
	}
	return &exitError{code: code, err: runErr}
}

// showStderr prints the last stderrShown bytes of the CLI's stderr, with
// every secret value redacted first, so a cut cannot leave part of one.
// When the runner kept only the tail, it drops the partial first line
// before redacting. When term is true, it removes control characters
// before redacting, so the header counts the bytes it prints.
func showStderr(w io.Writer, term bool, tool string, out runner.Outcome, byKey map[string]string) {
	if out.StdoutTruncated {
		fmt.Fprintf(w, "ask: %s printed more than 64 MiB on stdout; the rest was dropped\n", tool)
	}
	raw := string(out.Stderr)
	if out.StderrTruncated {
		raw = ""
		if nl := strings.IndexByte(string(out.Stderr), '\n'); nl >= 0 {
			raw = string(out.Stderr[nl+1:])
		}
	}
	if term {
		raw = adapter.StripControl(raw)
	}
	text := redact(raw, byKey)
	if text == "" {
		return
	}
	cut := out.StderrTruncated || len(text) > stderrShown
	text = tailText(text, stderrShown)
	if cut {
		fmt.Fprintf(w, "ask: last %d bytes of %s stderr:\n", len(text), tool)
	} else {
		fmt.Fprintf(w, "ask: %s stderr:\n", tool)
	}
	fmt.Fprint(w, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(w)
	}
}

// maxModelLen is the longest model word ask accepts.
const maxModelLen = 200

// oneWord reports whether model is a non-empty word of at most
// maxModelLen bytes with no space or control characters.
func oneWord(model string) bool {
	if model == "" || len(model) > maxModelLen {
		return false
	}
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// forTerminal removes control characters from tool text bound for a
// terminal, then redacts again, so a secret that an escape sequence split
// is still caught.
func forTerminal(text string, byKey map[string]string) string {
	return redact(adapter.StripControl(text), byKey)
}

// redactedPrefix starts every marker that redact writes.
const redactedPrefix = "<redacted:"

// tailText returns at most the last n bytes of text. The cut moves forward
// to a rune boundary, and past the end of a <redacted:KEY> marker when it
// lands inside one, so the result never starts mid-rune or mid-marker.
func tailText(text string, n int) string {
	if len(text) <= n {
		return text
	}
	start := len(text) - n
	for i := 0; ; {
		k := strings.Index(text[i:], redactedPrefix)
		if k < 0 || i+k >= start {
			break
		}
		m := i + k
		end := strings.IndexByte(text[m:], '>')
		if end < 0 {
			break
		}
		if m+end+1 > start {
			start = m + end + 1
			break
		}
		i = m + end + 1
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
