# ask

`ask` sends one prompt to a coding agent CLI and prints the tool's final
message. It knows four tools: codex, claude, agy and opencode. Every tool
gets the same command line and the same exit codes, so a script or another
agent can call any of them the same way.

The man page is [docs/ask.1](docs/ask.1). Read it with
`man ./docs/ask.1` from this folder.

## Contents

- [Overview](#overview)
- [Install](#install)
- [Quick start](#quick-start)
- [What works per tool](#what-works-per-tool)
- [Read-only and --write](#read-only-and---write)
- [Prompt input](#prompt-input)
- [Models and short names](#models-and-short-names)
- [ask models and ask usage](#ask-models-and-ask-usage)
- [ask review](#ask-review)
- [Flags](#flags)
- [Output](#output)
- [Config file](#config-file)
- [Reports and log](#reports-and-log)
- [Exit codes](#exit-codes)
- [Security notes](#security-notes)
- [Troubleshooting](#troubleshooting)

## Overview

```sh
ask <tool> <model> [prompt...] [flags]
ask-<tool> <model> [prompt...] [flags]
```

One run of `ask` does this, in order:

1. It reads the config file, if there is one.
2. It builds the prompt from the words after the model, stdin when you
   pass `-`, and any `-f` files.
3. It finds the tool's binary.
4. Without `--write`, it checks that the tool's read-only setup passed its
   test on the installed major version. If not, it refuses with exit 2.
   For claude, a guard then checks the run as it streams.
5. It looks up the secrets the config allows for this mode.
6. It starts the tool in its own process group, with the prompt off the
   command line, and waits for it to finish or time out.
7. It reads the tool's output, prints the final message, saves it to a
   report file and adds one line to the log.

`ask` never runs a tool's `login` command. Sign in to each tool on its own
first.

## Install

`ask` needs Go to build. `go install` puts the binary in
`$(go env GOPATH)/bin`, or in `GOBIN` when that is set:

```sh
go install -trimpath github.com/chadmayfield/agent-kit/ask/cmd/ask@latest
```

Or build it from a clone of this repo. From the repo root:

```sh
cd ask && go build -trimpath -o ~/.local/bin/ask ./cmd/ask
```

`ask` runs on macOS and Linux.

Each tool also has a short name. `ask-codex`, `ask-claude`, `ask-agy` and
`ask-opencode` take the model as their first word, so you leave out the
tool name. Make each one as a symlink to `ask`. The example uses
`~/.local/bin`; after `go install`, use that folder instead. The subshell
keeps your current folder for the next step:

```sh
(cd ~/.local/bin && ln -s ask ask-codex && ln -s ask ask-claude &&
  ln -s ask ask-agy && ln -s ask ask-opencode)
```

Only these four names count. A link named `ask-anything-else` behaves like
plain `ask`.

To read the man page with `man ask`, copy it into a `man1` folder that
`man` searches. From the `ask` folder of a clone:

```sh
mkdir -p ~/.local/share/man/man1 && cp docs/ask.1 ~/.local/share/man/man1/
```

If `man ask` still finds nothing, add `~/.local/share/man` to `MANPATH`.

## Quick start

Ask claude a question, read-only:

```sh
ask claude haiku "What does src/parser.go do?"
```

The same run through the short name:

```sh
ask-claude haiku "What does src/parser.go do?"
```

Review a diff. The diff comes in on stdin through `-`, so it never shows in
the process list:

```sh
git diff main | ask claude sonnet "Review this diff for bugs." -
```

Send a prompt kept in a file, and run in another folder. The `-f` path is
read from the folder you start `ask` in, not the `-C` folder:

```sh
ask claude haiku -f review-prompt.md -C ~/src/project
```

Let codex edit files with `--write`:

```sh
ask codex my-model --write "Add a unit test for the parse function."
```

Print only the last line that starts with `VERDICT:`, plus the report path:

```sh
ask claude haiku --final '^VERDICT:' -f review-prompt.md
```

Show the command `ask` would start, without running anything:

```sh
ask opencode --write --dry-run ollama/example-model "Fix the typo."
```

Show run details on stderr, with a five-minute limit:

```sh
ask claude haiku --verbose --timeout 5m "Summarize the README."
```

## What works per tool

| Tool | Read-only (no `--write`) | `--write` |
|---|---|---|
| claude | Runs on any `2.x.x` version, with a guard on each run. | Runs. |
| codex | Runs on any `0.163.x` version. | Runs. |
| agy | Refused. Its read-only test failed: hooks and MCP servers load. | Runs. |
| opencode | Refused. It has no read-only setup. | Runs. |

claude read-only passed its test on `2.1.325 (Claude Code)`. `ask` runs
`claude --version` before each read-only run and accepts any version with
the same major number, 2. A `3.x.x` release is refused until it passes the
same test. `--write` runs do not check the version.

codex read-only passed its test on `codex-cli 0.163.0`. codex is below
1.0, where a minor release can change behavior, so `ask` accepts the same
major and minor version only: any `0.163.x`. A `0.164.0` is refused until
it passes the same test.

Each read-only claude run also has a guard. `ask` adds
`--include-hook-events` and reads claude's output as it arrives. It stops
claude, discards its answer and exits 2 when any of these happen:

- The run does not start with claude's `init` event, or that event has no
  `tools` or `mcp_servers` list.
- The `init` event lists a tool other than Read, Grep and Glob, or any MCP
  server.
- A hook event appears.
- An event other than a `result` arrives before `init`, or a JSON event has
  an unexpected shape.
- A stdout line is longer than 64 MiB.
- claude exits 0 after a stdout line that is not JSON, or without an
  `init` event.

When claude fails on its own, for example on a bad flag or a login error,
that stays an ordinary failure, exit 1 with one retry, even if it printed
an error line or a `result` event first. A timeout or Ctrl-C that cuts a
line in half stays a timeout (exit 124) or an interrupt (exit 130).

The guard checks claude's output while claude keeps running; it does not
hold claude back. It sees what claude loaded only after claude reports it,
so a hook or MCP server that leaked in may have started once before `ask`
stops the run. The model's first reply takes far longer than the check, so
in practice the run stops before the model acts, but nothing enforces that
order.

[docs/read-only-tests.md](docs/read-only-tests.md) describes the test and
what it found for each tool.

## Read-only and --write

Read-only is the default. `--write` turns it off.

| Tool | Read-only run | `--write` run |
|---|---|---|
| claude | Only the Read, Grep and Glob tools. No hooks, no MCP servers, no browser tools. The session is saved under `~/.claude/projects` like any other. Your user settings and the repo's `CLAUDE.md` are not loaded. | `--permission-mode acceptEdits`: claude may edit files in the working folder. claude runs without `--safe-mode`, so your user settings load, and `ask` does not turn off hooks or MCP servers. |
| codex | The `read-only` sandbox, with your user config and rules files skipped. Hooks, MCP servers and file writes stay off; user skills still load. | The `workspace-write` sandbox. Your user config and rules load. |
| agy | Refused. Its read-only test failed. | Runs with `--mode accept-edits`. |
| opencode | Refused. `ask` has no read-only setup for it. | Runs `opencode run` with `--auto`. |

What changes between the two modes in `ask` itself:

- Secrets. A `--write` run gets every secret in the tool's config. A
  read-only run gets only the secrets named in `read_only_secrets`.
- Retry. A read-only run that exits non-zero with no final message runs
  once more. A `--write` run never retries, because the first attempt may
  already have changed files.
- Version check. Only a read-only run checks the tool's version.

In read-only mode claude does not load the repo's `CLAUDE.md`. If the task
needs it, pass it with `-f CLAUDE.md`.

## Prompt input

The prompt has up to three parts, joined with a blank line between them,
in this order:

1. The words after the model, joined with spaces.
2. Stdin, only when the words hold a lone `-`.
3. Each `-f` file, in the order given. Each one starts with a line
   `--- <file name> ---`, then the file's text.

```sh
ask claude haiku "Check these two files." -f a.go -f b.go
printf 'long prompt text' | ask claude haiku -
```

Rules for stdin:

- Without `-`, `ask` never reads stdin, so a pipe that never closes cannot
  make it wait.
- `-` may appear only once.
- `-` with stdin on a terminal is an error. Pipe the text in.
- `ask` reads at most 16 MiB from stdin.

An empty prompt is an error. Give words, a `-f` file, or `-` with piped
text.

A `-f` path is relative to the folder you start `ask` in, not the `-C`
folder.

Flags may appear anywhere on the line. A `--` ends the flags, so every word
after it is prompt text, even one that looks like a flag:

```sh
ask claude haiku -- --verbose is a flag name
```

### What shows in the process list

The prompt never goes on the tool's command line. How each tool gets it:

| Tool | How the prompt reaches the tool |
|---|---|
| claude | stdin |
| codex | stdin, through the `-` prompt argument |
| agy | a private temp file, named in the fixed instruction `Read the file <path> and follow the instructions in it.` |
| opencode | a private temp file, attached with `-f <path>`, plus the fixed instruction `Follow the instructions in the attached file.` |

For agy and opencode, `ask` writes the prompt to a file in a new temp
folder. The folder has mode 0700 and the file 0600, so only your user can
read them. `ask` removes the folder when the run ends, on every path,
including a timeout, a signal and `--dry-run`. The tool's command line
holds only the fixed instruction and the file path, never the prompt.

Prompt words you type as arguments do show in `ask`'s own command line.
Any user on the machine can see them with `ps`. Use `-` or `-f` for text
that should stay private.

## Models and short names

The model is the second word, or the first word with an `ask-<tool>` name.
`ask` passes it to the tool unchanged, unless the config maps it.

### Short names in the config

Each tool's `models` map turns a short name into a full model name:

```json
{ "tools": { "codex": { "models": { "m": "example-model" } } } }
```

With that config, `ask codex m ...` runs codex with `example-model`. A name
that is not in the map passes through as typed.

### claude aliases

claude takes its own aliases, such as `haiku`, `sonnet` and `opus`. The
installed claude CLI decides which model an alias means, and that can
change when claude is upgraded. To pin one model, give its full model ID
instead of the alias.

### opencode model IDs

opencode takes `provider/model` IDs, such as `ollama/example-model`. Run
`opencode models` to list the IDs your install knows.

### What the log records

Each log line has three model fields:

| Field | What it holds |
|---|---|
| `model` | The model word as you typed it. |
| `model_id` | The exact model passed to the tool, after the config short-name lookup. |
| `reported_model` | The model the tool says it used. Today only claude reports one; for the other tools it is empty. |

For a claude alias, `reported_model` shows which model the alias meant on
that run.

## ask models and ask usage

`ask models [tool]` lists the models each tool knows, and marks the ones a
config short name points at. A short name whose target the tool does not
list shows as `config only`. `--json` prints JSON instead.

| Tool | Where the list comes from |
|---|---|
| codex | `~/.codex/models_cache.json` |
| agy | `agy models` |
| opencode | `opencode models` |
| claude | No list. claude takes aliases such as `opus` and `sonnet`, or a full model name. |

`ask usage [tool]` prints each tool's rate limit windows. `--runs` adds
`ask`'s own runs, and `--json` prints the same data as JSON.

```
 codex     plan pro · credits 125.50 · as of Nov 3 09:40 · from last codex session
   all models  5h  ████░░░░░░░░░░░░░░░░   20% used   80% left  resets Nov 3 12:10 · in 2h30m
               7d  █████████░░░░░░░░░░░   45% used   55% left  resets Nov 6 08:00 · in 2d22h
```

- Limits. One block per tool. The header line names the plan and credits
  when the tool reports them, when the reading was taken (`live` or
  `as of`) and where it came from. Each window gets one row: the group of
  models that share it, its length (`5h` or `7d`), a meter, the percent
  used and left, and when it resets. A reset time that has passed means
  the reading is out of date. Tools with limits come first. On a
  terminal the meter is green below 50% used, yellow from 50%, orange
  from 70% and red from 90%. Orange needs a 256-color terminal
  (`COLORTERM` is `truecolor` or `24bit`, or `TERM` contains `256color`
  or `direct`); other terminals show yellow instead. `NO_COLOR` turns
  color off.

  | Tool | Where the limits come from |
  |---|---|
  | agy | `agy --print /usage --output-format json`, read live. From agy 1.1.11 on, agy answers it without starting an agent turn or spending quota, so `ask` checks `agy --version` first and shows `not available` on an older agy. Each model group has its own windows. |
  | codex | The newest session file in `~/.codex/sessions` that has rate limits, plus the plan and credit balance. The reading is as fresh as that session. A `RESETS` time that has already passed means the reading is out of date. |
  | claude | `<state>/claude-limits.json`, which a status line command writes. Claude Code passes its plan windows only to the status line command, only for Pro, Max and Ultra plans, and only after a model reply. Each plan shows the windows it has: Ultra has no 5-hour window, Max does. Without the file, claude shows `not available`. |
  | opencode | `not available`. |
- `--runs` adds, inside each tool's block, the runs `ask` made per model
  ID from its own log: runs, input and output tokens, total time,
  failures, guard stops, and runs that ended before the tool started (a
  refusal, bad arguments, a setup failure or a signal).
  A short name and its full model name count together.
  `--since YYYY-MM-DD` counts only runs on or after that date, and turns
  on `--runs`.

`ask models` and `ask usage` run the tools' own commands (`agy models`,
`opencode models`, `agy --version`, `agy --print /usage`) the way `ask`
runs a tool: in their own process group, with only the base environment
and the tool's `pass_env` (never its secrets), in a new private folder
that `ask` removes afterwards, and stopped after 30 seconds. Files `ask` reads from other tools must be
regular files under a size limit: 64 KiB for `claude-limits.json` and
16 MiB for codex's model cache. Codex session lines over 4 MiB are
skipped.

### Claude limits from the status line

Claude Code gives its 5-hour and weekly windows only to a status line
command, as `rate_limits` in the JSON on its stdin. To let `ask usage` read
them, have that command save the object to `<state>/claude-limits.json`:

```json
{"written_at": "2026-11-03T16:40:00Z",
 "rate_limits": {"seven_day": {"used_percentage": 25, "resets_at": 1794470400}}}
```

`written_at` is when the status line saw the values, in UTC. `resets_at` is
in Unix seconds, as Claude Code sends it. `ask` shows `five_hour` as `5h`
and `seven_day` as `7d`, and any other key under its own name. The reading
is as fresh as the last status line update in an interactive session.

### Sessions are saved

`ask` lets every tool save its session where it normally does, so usage
trackers that read those files count `ask` runs too. claude saves to
`~/.claude/projects`, codex to `~/.codex/sessions`, agy to
`~/.gemini/antigravity-cli/conversations` and opencode to its
`opencode.db`. Those transcripts hold the prompt, and the runs show up in
each tool's list of sessions to resume.

## ask review

`ask review` sends one prompt to claude, codex and agy at once and prints
their answers together:

```sh
ask review --claude opus --codex example-codex-model --agy example-agy-model \
  --as code "Review internal/workspace for bugs."
```

Any prompt works, built the same way as for `ask`: the words after the
flags, `-f` files, and a lone `-` for stdin. Each tool gets the same text.

Pass the files to review with `-f` when agy is one of the tools. In a
headless run, agy's read-only mode refuses shell commands, so an agy that
tries to read a file with one gives no answer. With the text attached, it
does not need to.

agy's read-only setup runs it with `--mode plan`, which tells it to write a
plan and wait for approval. A headless run cannot approve, so agy can end
with a plan instead of a review. `ask` puts a short note in front of agy's
prompt that tells it the answer is the plan and rules out shell commands
and other tools. agy therefore reviews only the text of its prompt and the
files attached with `-f`; it does not open other files in the folder.
If agy still writes a plan, the review fails with `agy wrote a plan instead
of an answer` and the plan's path. See
[docs/read-only-tests.md](docs/read-only-tests.md#agy).

| Flag | What it does |
|---|---|
| `--claude <model>`, `--codex <model>`, `--agy <model>` | Review with that tool and model. Short names from the tool's `models` map work. A tool without a model from a flag or from `review.models` in the config is left out. At least one is needed. |
| `--as <kind>` | Put a preset instruction in front of the prompt: `adversarial`, `code`, `docs` or `security`, or a kind from `review.presets`. Without it, the prompt goes as written. |
| `--json` | Print JSON instead of text. |
| `-C`, `--dir <folder>` | Review this folder. Default: the current folder. |
| `--timeout <duration>` | Stop each tool after this long. Default: the tool's `timeout_seconds`, or 10m. |
| `-f`, `--file <file>` | Add a file's text to the prompt. Repeat for more files. |

The config can set default models and add or change presets:

```json
{ "review": {
    "models": { "claude": "opus", "codex": "example-codex-model" },
    "presets": { "plan": "Review this plan for gaps and risky steps." } } }
```

### How each tool runs

Every tool runs with its read-only setup and its read-only secrets, never
with `--write`.

- A tool whose read-only setup is verified for the installed version runs
  in the real folder. Today that is claude, with its guard, and codex.
- Any other tool runs in a throwaway copy of the folder: a new private
  temp folder (mode 0700) that `ask` removes afterwards. Its read-only
  setup is unverified, so `ask` takes a fingerprint of the copy before and
  after. If anything changed, that review fails with `changed files` and
  lists up to 10 of them.

What goes into the copy:

- Files and folders keep their permission bits. FIFOs, sockets and devices
  are left out.
- A symlink is kept only if it is relative and resolves inside the folder.
  It is written as the plain relative path to its target. Absolute,
  dangling and outward links are left out.
- `.git` is copied, so a reviewer can read history and diffs. A git
  folder is found by the name `.git` in any case, or by holding `HEAD`
  together with `objects`, `refs` or `commondir` (a bare repository or a
  worktree's git folder, say). In each one, every `config` and
  `config.worktree` is rebuilt to keep only the settings that describe how
  the repository is stored (`core.repositoryformatversion`, `core.bare`,
  `extensions.objectformat` and a few more). Remotes, branch remotes, URL
  rewrites, credential helpers, includes, `core.worktree`, hooks paths,
  fsmonitor, diff and filter commands are all gone. The `hooks` folder,
  git's older push destinations `remotes` and `branches`, `commondir`,
  `gitdir`, `objects/info/alternates`, a `.git` file or symlink, and any
  symlink inside a git folder are left out. Git file names match in any
  case, as git finds them on a case-insensitive disk.
- Git in the copy runs with `GIT_CEILING_DIRECTORIES` set to the copy's
  temp folder, so it cannot find a repository above it, and with
  `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`, so your
  global and system git config do not apply. Together, git in the copy
  has no configured remote to push to and runs no configured commands.
- Folders and links named `.agents` or `.agent`, in any case and at any
  depth, are left out. agy runs the hooks, plugins and MCP servers they
  name, even in a folder it does not trust, so a reviewed repository could
  otherwise run its own commands. agy still loads the plugins and MCP
  servers in your own agy config; see
  [docs/read-only-tests.md](docs/read-only-tests.md#agy).
- A folder over 512 MiB or 50,000 entries is refused for that tool with
  exit 2. Other copy failures, such as an unreadable file, fail that tool
  with exit 3. Messages name paths relative to the folder.

The copy only sees changes made inside it. A tool still has network
access, and where its own sandbox allows, it could push to an explicit
URL with your SSH keys or write to an absolute path, including a path in
the original folder. Only a verified read-only setup closes that.

### Output

Text output has one block per tool, in the order claude, codex, agy, then
a summary: tool, model ID, where it ran (`read-only` or `copy`), exit
code, time, tokens and report path. Each tool's run is logged and saves
its own report as usual, and the whole review is saved as a combined
report whose path ends the output.

`--json` prints `{"reviews": [...], "report": "<path>"}`. Each review has
`tool`, `model`, `model_id`, `mode`, `exit`, `error`, `final`, `report`,
`duration_ms`, `input_tokens`, `output_tokens` and `changed_files`.

Exit codes: 0 when every review succeeded, 1 when any failed (the others
still print), 2 for bad arguments, 3 when `ask`'s own setup fails before
any tool starts, and 128+n when a signal stops the review. A signal stops
every tool, including a copy or fingerprint in progress; answers that had
already finished still print and go into the combined report. Warnings
from a tool's run, and the end of a failed tool's own stderr, print on
stderr with the tool's name in front, with secrets redacted. Changed file
names are redacted the same way, since a file name can hold a secret.

## Flags

| Flag | Default | What it does |
|---|---|---|
| `--write` | off | Let the tool edit files and run MCP servers and hooks. See [Read-only and --write](#read-only-and---write). |
| `-C`, `--dir <folder>` | the current folder | Run the tool in this folder. The folder must exist. |
| `--timeout <duration>` | the tool's `timeout_seconds`, or 10m | Stop the tool after this long. Takes Go durations such as `90s`, `5m` or `1h30m`. `0` means the default. A negative value is an error. |
| `--effort <level>` | none | Reasoning effort. claude and agy take `low`, `medium`, `high`, `xhigh` or `max`. codex and opencode take none, and `ask` refuses the flag for them. |
| `--final <regex>` | none | Print only the last line of the final message that matches the regular expression, then the report path. Exit 1 when no line matches. Cannot be used with `--no-report`. |
| `--dry-run` | off | Show the command `ask` would start, and run nothing. Secrets show as config key names, the prompt as its length in bytes. |
| `--no-report` | off | Do not save the final message to a report file. |
| `--verbose` | off | After a run that started the tool, print a summary line on stderr: tool, model, model ID, reported model, duration, tokens, exit code and report path. After a failed run, also print the last 4 KiB of the tool's stderr, with secrets redacted. |
| `-f`, `--file <file>` | none | Add a file's text to the prompt. Repeat for more files. |
| `-h`, `--help` | | Print the help text. |

The regular expression for `--final` uses Go syntax. It matches each line
on its own, so `^` and `$` mark the start and end of a line.

### --dry-run output

`--dry-run` prints one line on stdout that shows what a real run would
start, for example:

```
EXAMPLE_API_KEY=<api_key> /usr/local/bin/codex exec --json ... - <prompt: 42 bytes>
```

The line is for reading, not for pasting into a shell. Each argument is
shell-quoted, but the markers in angle brackets stand in for values `ask`
does not show:

- A secret shows as `ENV_VAR=<config key>`, never its value.
- A `pass_env` variable shows as `NAME=<pass_env>`.
- The prompt shows as `<prompt: N bytes>` for claude and codex, which read
  it from stdin.
- For agy and opencode it shows as `<prompt: N bytes, in a temp file>`.
  The temp file path in the line no longer exists: `ask` writes the file
  to build the command, then removes it before it exits.
- Without `--write`, stderr also gets `read-only: verified for <range> (last
  tested "<version>")`, where the range is `2.x` or, for a 0.x tool,
  `0.163.x`, or `read-only: not verified`. A dry run does not run the version check,
  so it may print a command that a real run would refuse.

A dry run looks up no secrets, writes no report and adds no log line.

## Output

By default `ask` prints the tool's final message on stdout and nothing
else. A final message without a trailing newline gets one.

Tool text printed to a terminal has control characters and escape
sequences removed. Piped or redirected output and the report file keep the
exact bytes.

With `--final <regex>`, stdout gets two lines: the last matching line of
the final message, then the path of the report file that holds the whole
message.

With `--verbose`, stderr gets one summary line after a run that started
the tool. A refusal, bad arguments or a setup failure print only the
error, with no summary line:

```
ask: tool=claude model=haiku model_id=haiku reported_model=claude-example-model duration=4.213s tokens=5372/4 exit=0 report=/path/to/report.md
```

- `model` is the model word as you typed it.
- `model_id` is the model passed to the tool, after the config short-name
  lookup.
- `reported_model` is the model the tool says it used.
  `reported_model=none` means the tool named no model. Only claude names
  one today.
- `tokens` is input tokens, then output tokens.
- `report=none` means no report was saved.

After a failed run, `--verbose` also prints the last 4 KiB of the tool's
stderr, with every secret value replaced by `<redacted:KEY>`. The cut never
splits a character or a redaction marker. `ask` keeps at most the last
1 MiB of a tool's stderr and the first 64 MiB of its stdout. When stdout
went past 64 MiB, `--verbose` says the rest was dropped.

With `--verbose`, a read-only run that is about to retry prints
`ask: <tool> exited with status N and no answer; retrying once`.

Errors go to stderr, starting with `ask:`.

## Config file

The config file is `$XDG_CONFIG_HOME/agent-kit/config.json`, or
`~/.config/agent-kit/config.json` when `XDG_CONFIG_HOME` is unset or not an
absolute path.

Without a config file, `ask` finds each tool on PATH and passes no
secrets. The config never holds a secret value, only where to find one.
`ask` rejects an unknown key, so a misspelled key is an error. A
misspelled tool name under `tools` is not caught: `ask` ignores that
entry, and the tool runs as if it had no config.

### Top-level keys

| Key | Type | Meaning |
|---|---|---|
| `report_days` | integer | Delete report files older than this many days. `0` or missing means 30. Negative is an error. |
| `tools` | object | One entry per tool name: `codex`, `claude`, `agy` or `opencode`. |

### Per-tool keys

| Key | Type | Meaning |
|---|---|---|
| `binary` | string | The tool's program. A path with a `/` is used as given; a relative one is relative to the folder you run `ask` in, not `-C`. A bare name is looked up on PATH. Missing means the tool's own name on PATH. |
| `models` | object of strings | Short name to full model name. See [Models and short names](#models-and-short-names). |
| `timeout_seconds` | integer | The default timeout for this tool. `0` or missing means 10 minutes. `--timeout` overrides it. |
| `secrets` | array | Secrets to pass to the tool as environment variables. |
| `read_only_secrets` | array of strings | The `name`s of the secrets a read-only run gets. Every name must be in `secrets`. |
| `pass_env` | array of strings | Variables from your environment to pass to the tool unchanged. |
| `auth` | string | Accepted, but `ask` does nothing with it today. |

### Secret entries

| Key | Meaning |
|---|---|
| `name` | The config key name. Errors, `--dry-run` and redaction markers use it. Must be unique within the tool. |
| `env_var` | The environment variable the tool reads the value from. Must be unique within the tool. |
| `ref` | Where `ask` finds the value. |

A `ref` starts with one of four schemes:

| Ref | Where the value comes from |
|---|---|
| `keychain:<account>@<service>` | The macOS login Keychain, through `security find-generic-password`. The account part is optional: `keychain:<service>` also works. macOS only; on Linux the lookup fails. |
| `env:<NAME>` | An environment variable of `ask`. |
| `file:<path>` | A file. A leading `~/` means your home folder. |
| `cmd:<command>` | The stdout of a shell command, run with `/bin/sh -c`. |

For every scheme, `ask` drops one trailing newline from the value. An
empty value is an error. A `keychain:` or `cmd:` lookup that takes longer
than 30 seconds fails. A `cmd:` command runs in its own process group, and
`ask` stops the whole group when the command ends, so no child of it keeps
running.

### What the tool's environment holds

The tool does not inherit `ask`'s whole environment. It gets:

- `PATH`, `HOME`, `USER`, `LANG`, `TERM`, `TMPDIR` and `SHELL`.
- Every `XDG_*` variable.
- The variables named in `pass_env`.
- The secrets for this mode, each in its `env_var`.

A later entry in that list replaces an earlier one with the same name. Any
other variable, such as an API key exported in your shell, reaches the
tool only if you list it in `pass_env` or add it as a secret.

### Full example

Every value here is a placeholder:

```json
{
  "report_days": 30,
  "tools": {
    "codex": {
      "models": { "m": "example-model" },
      "timeout_seconds": 600,
      "secrets": [
        {
          "name": "api_key",
          "env_var": "EXAMPLE_API_KEY",
          "ref": "keychain:placeholder-account@placeholder-service"
        }
      ],
      "read_only_secrets": [],
      "pass_env": []
    },
    "claude": {
      "binary": "/opt/example/bin/claude",
      "timeout_seconds": 900
    },
    "opencode": {
      "models": { "local": "ollama/example-model" },
      "secrets": [
        { "name": "api_key", "env_var": "EXAMPLE_API_KEY", "ref": "env:EXAMPLE_SOURCE_VAR" },
        { "name": "gateway_token", "env_var": "EXAMPLE_GATEWAY_TOKEN", "ref": "cmd:example-secret-tool get token" }
      ],
      "read_only_secrets": ["api_key"],
      "pass_env": ["EXAMPLE_PASS_VAR"]
    },
    "agy": {
      "secrets": [
        { "name": "api_key", "env_var": "EXAMPLE_API_KEY", "ref": "file:~/.config/example/api_key" }
      ]
    }
  }
}
```

[config.example.json](config.example.json) holds a similar example.

## Reports and log

Both live in the state folder: `$XDG_STATE_HOME/ask`, or
`~/.local/state/ask` when `XDG_STATE_HOME` is unset or not an absolute path.

### Reports

Each run with a final message saves it to
`<state>/reports/<time>-<tool>-<pid>-<n>.md`. `<time>` is UTC, such as
`20260101T120000Z`, and `<n>` counts up from 1 so two saves in the same
second never collide.

- A report holds only the final message, with secrets redacted.
- Each file has mode 0600. The `reports` folder has mode 0700.
- `ask` creates each file new and never overwrites one. It refuses a
  symlinked file or folder.
- Each run that saves a report also deletes report files older than
  `report_days` (30 by default), judged by the file's last modified time.
- `--no-report` skips the report, and so skips the cleanup on that run.

If the report cannot be saved, `ask` still prints the final message, then
exits 3. With `--final`, it prints nothing on stdout, because there is no
report path to print.

### Log

Each run adds one JSON line to `<state>/log.jsonl`. The file has mode 0600
and `ask` never deletes or trims it.

| Field | What it holds |
|---|---|
| `time` | When the run ended, in UTC. |
| `tool` | The tool name. |
| `model` | The model word as typed. |
| `model_id` | The model passed to the tool, after the short-name lookup. |
| `reported_model` | The model the tool says it used. Only claude reports one today. |
| `duration_ms` | How long the tool ran, in milliseconds. A retry adds to it. |
| `exit` | `ask`'s exit code. |
| `input_tokens` | Input tokens, as the tool reports them. |
| `output_tokens` | Output tokens, as the tool reports them. |
| `guard` | Why the read-only guard stopped the run. Only present after a guard stop. |

A refused run is logged too, with its exit code, and so are most bad
arguments, such as an empty prompt or a bad `--final` regex. These runs add
no line: `--dry-run`, a missing tool or model, an unknown tool, an unknown
flag, and a flag value that does not parse.
The log never holds the prompt or the final message.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | The tool failed, gave no final message, or `--final` found no match. |
| 2 | Bad arguments, or `ask` refused the run. |
| 3 | `ask`'s own setup failed: config, secret lookup, missing binary or report save. |
| 124 | Timeout. |
| 128+n | The tool or `ask` was killed by signal n, such as 130 for Ctrl-C. |

On a timeout or a signal, `ask` stops the tool's whole process group, so
no child of the tool keeps running. A timeout is always 124.

## Security notes

- The prompt never goes on the tool's command line. See [What shows in
  the process list](#what-shows-in-the-process-list).
- A secret value never goes on any command line, in the log, in a report,
  in `--dry-run` output or in an error. Errors name the secret's config
  key, never its value, ref, file path, command or Keychain item.
- A secret reaches the tool only as an environment variable.
- `ask` replaces every secret value it looked up with `<redacted:KEY>` in
  the final message, the report and the stderr tail. A short secret value
  may also hide matching ordinary text.
- A `keychain:` or `cmd:` lookup puts the item name or the command in the
  `security` or shell process's own command line for a moment.
- The log holds no prompt and no final message. Reports hold the final
  message, readable only by you.
- Temp prompt files for agy and opencode are readable only by you and are
  removed after each run.

## Troubleshooting

Each message below appears on stderr after `ask: `.

### Refusals (exit 2)

- `read-only setup for <tool> is not verified; ask refuses to run it without --write`

  The tool has no verified read-only setup. Add `--write` if the tool may
  edit files, or use claude for a read-only run.

- `<tool> version "<found>" is outside the tested range <range> (last tested "<tested>"); read-only is refused until the test in ask/docs/read-only-tests.md passes on it`

  The installed tool is outside the versions its read-only setup was
  tested on. The range is the tested major version, such as `2.x`, or for
  a 0.x tool the major and minor version, such as `0.163.x`. Use
  `--write` until the test in
  [docs/read-only-tests.md](docs/read-only-tests.md) passes on the new
  version.

- `read-only guard stopped <tool> and discarded its output: <reason>`

  The guard saw a hook, an extra tool, an MCP server or an unexpected event
  in a read-only claude run. `ask` does not retry. Check your claude
  settings and plugins, and see whether a claude update changed what
  `--safe-mode` and `--restricted` turn off.

- `cannot read the <tool> version, so its read-only test does not apply`

  `<tool> --version` failed or took more than 10 seconds. Check that the
  binary runs.

- `effort not supported by <tool>`

  The tool does not take that `--effort` value, or any. See [Flags](#flags).

- `opencode has no read-only setup`

  opencode runs only with `--write`. A `--dry-run` without `--write`
  skips the read-only check, so it shows this message instead of
  `read-only setup for opencode is not verified`.

### Bad arguments (exit 2)

- `need a tool and a model; run ask --help for usage`

  Give both, as in `ask claude haiku "..."`.

- `need a model; run ask-<tool> --help for usage`

  An `ask-<tool>` name still needs the model as its first word.

- `unknown tool "<name>"; known tools: agy, claude, codex, opencode`

  Check the spelling of the first word.

- `the model must be one word, for example ask claude haiku ...`

  The model was empty, held a space or control character, or ran past
  200 characters. This usually means the model was left out and the
  prompt took its place.

- `unknown flag: --<name>`

  Not one of the flags `ask` knows. To send a word that looks like a flag
  as prompt text, put it after `--`.

- `empty prompt: give prompt text, a -f file, or "-" with piped stdin`

  Nothing to send. Add words, `-f`, or pipe text and pass `-`.

- `"-" reads the prompt from stdin, but stdin is a terminal; pipe the text in`

  Pipe text into `ask`, or drop the `-`.

- `"-" can appear only once`

  Pass a single `-`.

- `stdin is larger than 16 MiB`

  Put the text in a file and pass it with `-f`.

- `read prompt file: ...`

  A `-f` file is missing or not readable.

- `-C: <folder> is not a folder`

  The `-C` folder does not exist.

- `--final prints the report path, so it cannot be used with --no-report`

  Drop one of the two.

- `--final: ...`

  The regular expression does not compile.

- `--timeout must not be negative`

  Give a positive duration, or `0` for the default.

### Setup failures (exit 3)

- `<tool>: not found on PATH and no binary set in config`

  Install the tool, add it to PATH, or set `binary` in the config.

- `<tool>: the configured binary is not found on PATH`

  The config's `binary` is a bare name that PATH does not have.

- `<tool>: the binary is missing or not executable`

  The binary path points at nothing, a folder, or a file without execute
  permission.

- `config: parse <path>: ...`

  The config file is not valid JSON, or holds an unknown key.

- `config: <path>: ...`

  A value is wrong. The message names the tool and the key, for example
  `report_days is negative`, `secret "api_key" has no env_var` or
  `read_only_secrets names "x", which is not in secrets`.

- `config: no config path; set HOME or XDG_CONFIG_HOME to an absolute path`

  `ask` could not work out where the config file lives.

- `no state folder; set HOME or XDG_STATE_HOME to an absolute path`

  `ask` could not work out where reports and the log go.

- `secret "<name>": <reason>`

  A secret lookup failed. The reason is one of these:

  | Reason | Scheme |
  |---|---|
  | `environment variable is not set` | `env:` |
  | `file does not exist`, `file is not readable`, `file read failed` | `file:` |
  | `cannot find the home folder` | `file:` with `~/` |
  | `command exited with status N`, `command could not start` | `cmd:` |
  | `command left a process holding its output` | `cmd:` or `keychain:`, when a leftover child kept the output open and nothing was printed |
  | `keychain lookup exited with status N`, `keychain lookup could not start` | `keychain:` |
  | `keychain ref has no service` | `keychain:` |
  | `keychain: refs work on macOS only` | `keychain:` on Linux |
  | `lookup timed out` | `cmd:` or `keychain:`, after 30 seconds |
  | `lookup returned an empty value` | any |

- `the report was not saved`

  The run worked and the final message was printed, but the report file
  could not be written. The line before it says why, for example a
  symlinked reports folder.

### Tool failures (exit 1, 124 or 128+n)

- `<tool> exited with status N`

  The tool failed. Run again with `--verbose` to see the end of its stderr.

- `opencode exited with status N: opencode reported a <type> error (status <code>)`

  opencode writes its errors to stdout, not stderr. `ask` shows the error
  type and status code from it, such as `provider.auth` and 401 for a
  missing or wrong API key, but not opencode's message text. A
  `provider.auth` error usually means the config does not pass the key
  opencode needs; check its `secrets` entries.

- `no final message`

  The tool exited 0 but `ask` found no final message in its output.

- `no line of the final message matched --final "<regex>"`

  The run worked, but no line matched. The whole message is in the report.

- `claude stopped at its turn limit`, `claude failed during execution`, `claude reported an error`

  claude exited 0 but reported an error in its result. `ask` does not
  print claude's own error text, because it may hold prompt content.

- `agy did not report success`

  agy exited 0, but its JSON output did not have status `SUCCESS`.

- `agy wrote a plan instead of an answer: <path>`

  A read-only agy run exited 0, but it wrote a plan for approval in
  `~/.gemini/antigravity-cli/brain/<conversation>/implementation_plan.md`
  instead of answering. The run fails with exit 1 and is not retried. Read
  the plan at the path to see what agy meant to do.

- An output that does not parse

  The tool's output did not have the shape `ask` expects. A tool upgrade
  can cause this. The message starts with the tool name:

  | Tool | Message |
  |---|---|
  | claude | `claude output line N is not valid JSON: ...` |
  | codex | `codex: output line N is not valid JSON` |
  | agy | `agy: output is not a JSON object` |
  | opencode | `opencode: output is not JSON lines` |

- `<tool> timed out after <duration>` (exit 124)

  Raise `--timeout` or `timeout_seconds`.

- `interrupted` or `<tool> was killed by signal N` (exit 128+n)

  You pressed Ctrl-C, or something sent a signal to `ask` or the tool.

### Warnings

- `warning: log: ...`

  The log line could not be written. The run's result and exit code do
  not change.

- `warning: report: ...`

  Old reports could not be cleaned up. The run's result does not change.
