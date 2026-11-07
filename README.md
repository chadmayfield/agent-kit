# agent-kit

agent-kit is a set of small command-line tools for coding agents and the
people who run them. The first tool, `ask`, prints only the other agent's
final message, with no progress output around it.

| Tool | Status | What it does |
|---|---|---|
| `ask` | available | Sends one prompt to codex, claude, agy or opencode and prints the final message |
| `agent-kit install`, `update` and `doctor` | planned | Installs the kit's files, updates them, and reports what is out of date |
| Skills | planned | Generic skills for coding agents |

[AGENTS.md](AGENTS.md) tells an agent which command to use for what.

## ask

`ask` sends one prompt to a coding agent CLI (codex, claude, agy or
opencode) and prints the tool's final message. One command line, one exit
code, the same for every tool, so a script or another agent can call any of
them the same way.

Read-only is the default. Today only claude runs read-only, and only on
the version its read-only test passed on. All four tools run with
`--write`. The guide's
[What works per tool](docs/ask/README.md#what-works-per-tool) section has
the details.

### Install

Needs Go. From a clone of this repo:

```sh
go build -trimpath -o ~/.local/bin/ask ./cmd/ask
```

For the short names `ask-codex`, `ask-claude`, `ask-agy` and
`ask-opencode`, and for the man page, see
[Install](docs/ask/README.md#install) in the guide.

### Quick examples

Ask claude a question, read-only:

```sh
ask claude haiku "What does internal/runner/runner.go do?"
```

Review a diff sent on stdin, so it stays out of the process list:

```sh
git diff main | ask claude sonnet "Review this diff for bugs." -
```

Let codex edit files:

```sh
ask codex my-model --write "Add a unit test for the parse function."
```

### Docs

- [docs/ask/README.md](docs/ask/README.md): the full guide. Every flag,
  the config file, reports and log, exit codes and troubleshooting.
- [docs/man/ask.1](docs/man/ask.1): the man page.
- [docs/read-only-tests.md](docs/read-only-tests.md): how each tool's
  read-only setup was tested, and the results.
- [AGENTS.md](AGENTS.md): which command an agent should use. In Claude
  Code, add an `@` import line for it to your `CLAUDE.md`. For other
  agents, paste it or link it by hand.

## Hooks for contributors

The repo runs two checks before each commit and push, through
[pre-commit](https://pre-commit.com): gitleaks on staged changes, and
`scripts/denylist-check.sh`, which blocks strings from a private list. Both
hooks are local; pre-commit downloads nothing. You need `pre-commit` and
`gitleaks` on your PATH.

```sh
pre-commit install --hook-type pre-commit --hook-type pre-push
git config agentkit.denylist /path/to/your/denylist.txt
```

The list has one entry per line. Lines starting with `#` are skipped, lines
starting with `re:` are extended regular expressions, and every other line
is a fixed string. Both match in any case.

If you have no list, turn the denylist check off for your clone. The hook
then passes with a warning:

```sh
git config agentkit.denylist none
```

With `agentkit.denylist` unset, the hook fails and asks you to set it.

## License

MIT. See [LICENSE](LICENSE).
