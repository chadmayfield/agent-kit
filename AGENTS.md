# agent-kit commands for agents

## Second opinion from another agent

Use `ask` for a second opinion or a review from another coding agent CLI
(codex, claude, agy or opencode):

```sh
ask <tool> <model> [prompt...]
ask-<tool> <model> [prompt...]
```

- Read-only is the default. `ask` runs a tool read-only only if its
  read-only setup passed its test on the installed version. Otherwise it
  refuses with exit 2.
- Add `--write` only when the other agent must edit files.
- By default `ask` prints only the tool's final message.
- `--final <regex>` prints only the last matching line, plus the report
  path.
- `--verbose` adds run details on stderr.
- Put prompt text in a file with `-f <file>`, or pipe it in and pass a
  lone `-`. Prompt words given as arguments show in the process list.

Exit codes: 0 ok, 1 tool failed or no match, 2 bad arguments or refused,
3 setup failed, 124 timeout, 128+n killed by signal n.

[docs/ask/README.md](docs/ask/README.md) explains every flag, refusal and
error message.

## Other services

These have their own CLIs. agent-kit does not wrap them.

- Fizzy: use `fz`.
- GitHub: use `gh`.
- Gitea: use `tea`.
