# agent-kit

agent-kit is a set of small command-line tools for coding agents and the
people who run them. Each tool lives in its own folder with its own guide.

| Tool | Status | What it does |
|---|---|---|
| [`ask`](ask/README.md) | available | Sends one prompt to codex, claude, agy or opencode and prints the final message |
| Kit installer | planned | One command that installs every tool in the kit, the skills, shell completions and AGENTS.md, plus `update` and `doctor` for all of them |
| Skills | planned | Generic skills for coding agents |

- [ask/README.md](ask/README.md): install and use `ask`.
- [AGENTS.md](AGENTS.md): which command an agent should use for what. In
  Claude Code, add an `@` import line for it to your `CLAUDE.md`. For
  other agents, paste it or link it by hand.

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
