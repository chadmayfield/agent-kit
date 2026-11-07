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
   test on the installed version. If not, it refuses with exit 2.
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
go install github.com/chadmayfield/agent-kit/ask/cmd/ask@latest
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
ask claude haiku "What does internal/runner/runner.go do?"
```

The same run through the short name:

```sh
ask-claude haiku "What does internal/runner/runner.go do?"
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

Let codex edit files. `--write` is required for codex today:

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
| claude | Runs on version `2.1.319 (Claude Code)` only. | Runs. |
| codex | Refused. Its read-only test was inconclusive. | Runs. |
| agy | Refused. Its read-only setup is not tested. | Runs. |
| opencode | Refused. It has no read-only setup. | Runs. |

claude read-only is verified for the version it was tested on, and only
that one. `ask` runs `claude --version` before each read-only run and
compares the trimmed output with `2.1.319 (Claude Code)`. After a claude
upgrade the strings differ, and `ask` refuses read-only runs until the new
version passes the same test. `--write` runs do not check the version.

[docs/read-only-tests.md](docs/read-only-tests.md) describes the test and
what it found for each tool.

## Read-only and --write

Read-only is the default. `--write` turns it off.

| Tool | Read-only run | `--write` run |
|---|---|---|
| claude | Only the Read, Grep and Glob tools. No hooks, no MCP servers, no browser tools, no saved session. Your user settings and the repo's `CLAUDE.md` are not loaded. | `--permission-mode acceptEdits`: claude may edit files in the working folder. claude runs without `--safe-mode`, so your user settings load, and `ask` does not turn off hooks or MCP servers. |
| codex | Refused today. The setup `ask` would use is the `read-only` sandbox, with your user config and rules files skipped. | The `workspace-write` sandbox. Your user config and rules load. |
| agy | Refused. The setup is not tested. | Runs with `--mode accept-edits`. |
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
- Without `--write`, stderr also gets `read-only: verified for "<version>"`
  or `read-only: not verified`. A dry run does not run the version check,
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

- `<tool> version "<found>" is not the tested "<tested>"; read-only is refused until it is tested again`

  The installed claude is not the version its read-only setup was tested
  on, usually after an upgrade. Use `--write`, or go back to the tested
  version.

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

- `no final message`

  The tool exited 0 but `ask` found no final message in its output.

- `no line of the final message matched --final "<regex>"`

  The run worked, but no line matched. The whole message is in the report.

- `claude stopped at its turn limit`, `claude failed during execution`, `claude reported an error`

  claude exited 0 but reported an error in its result. `ask` does not
  print claude's own error text, because it may hold prompt content.

- `agy did not report success`

  agy exited 0, but its JSON output did not have status `SUCCESS`.

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
