# agent-kit commands for agents

## Second opinion from another agent

Use `ask` for a second opinion or a review from another coding agent CLI
(codex, claude, agy or opencode):

```sh
ask <tool> <model> [prompt...]
ask-<tool> <model> [prompt...]
```

- Read-only is the default. `ask` runs a tool read-only only if its
  read-only setup passed its test on the installed major version.
  Otherwise it refuses with exit 2. Today claude and codex run
  read-only. For claude a guard checks each run: if claude loads a hook,
  an MCP server or a tool other than Read, Grep and Glob, `ask` stops it
  and exits 2. codex runs in its `read-only` sandbox.
- Add `--write` only when the other agent must edit files.
- By default `ask` prints only the tool's final message.
- `--final <regex>` prints only the last matching line, plus the report
  path.
- `--verbose` adds run details on stderr.
- Put prompt text in a file with `-f <file>`, or pipe it in and pass a
  lone `-`. Prompt words given as arguments show in the process list.

Exit codes: 0 ok, 1 tool failed or no match, 2 bad arguments, refused or
stopped by the read-only guard, 3 setup failed, 124 timeout, 128+n killed
by signal n.

For a review from several models at once, use `ask review`. It sends one
prompt to claude, codex and agy and prints the answers together:

```sh
ask review --claude <model> --codex <model> --agy <model> [--as adversarial|code|docs|security] -f <file>
```

It never runs a tool with `--write`; tools whose read-only setup is not
verified review a throwaway copy, and a review that changed the copy
fails. Treat each answer as one opinion to verify against the code.

To pick a reviewer by remaining quota, run `ask usage` first. It shows
each tool's 5-hour and weekly windows, how much is used and when each
resets; `ask usage --json` gives the same data for scripts. `ask models
<tool>` lists the model names a tool accepts.

[ask/README.md](ask/README.md) explains every flag, refusal and
error message.

## Other services

These have their own CLIs. agent-kit does not wrap them.

- Fizzy: use `fz`.
- GitHub: use `gh`.
- Gitea: use `tea`.
