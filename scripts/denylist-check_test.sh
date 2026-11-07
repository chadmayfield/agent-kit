#!/usr/bin/env bash
# Tests for denylist-check.sh. Each case builds a throwaway git repo and
# checks the script against a placeholder list. Run: scripts/denylist-check_test.sh
set -u

here=$(cd "$(dirname "$0")" && pwd)
script="$here/denylist-check.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Keep the user's git config (signing, hooks, templates) out of the tests.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
unset PRE_COMMIT PRE_COMMIT_FROM_REF PRE_COMMIT_TO_REF PRE_COMMIT_REMOTE_NAME PRE_COMMIT_REMOTE_URL

list="$tmp/list.txt"
cat > "$list" <<'EOF'
# placeholder list for tests

placeholder.invalid
/Users/example
re:\b192\.0\.2\.[0-9]+\b
EOF

zero=0000000000000000000000000000000000000000
passed=0
failed=0
out=""
code=0
n=0

pass() { passed=$((passed + 1)); }
fail() {
	failed=$((failed + 1))
	echo "FAIL: $1"
	echo "  exit: $code"
	echo "  output: $out"
}

# new_repo makes a repo with one clean commit and cds into it.
new_repo() {
	n=$((n + 1))
	repo="$tmp/repo$n"
	git init -q -b main "$repo"
	cd "$repo" || exit 1
	printf 'one\ntwo\n' > a.txt
	git add a.txt
	git commit -q -m "first commit"
}

# check runs the script and stores its exit code and output.
check() {
	code=0
	out=$("$script" "$@" 2>&1) || code=$?
	# The output must never carry the matched text.
	if printf '%s' "$out" | grep -qiE 'placeholder\.invalid|/users/example|192\.0\.2'; then
		code=99
		fail "output leaked matched text (args: $*)"
	fi
}

expect() { # expect <name> <code> [output fragment]
	if [ "$code" -ne "$2" ]; then
		fail "$1: want exit $2"
		return
	fi
	if [ $# -ge 3 ] && ! printf '%s' "$out" | grep -qF -- "$3"; then
		fail "$1: output lacks '$3'"
		return
	fi
	pass
}

# Staged added line hits, with its line number.
new_repo
printf 'see https://placeholder.invalid/x\n' >> a.txt
git add a.txt
check --list "$list" --staged
expect "staged added line" 1 "a.txt:3: denylist hit"

# A change only in the working tree does not count for --staged.
new_repo
printf 'see placeholder.invalid\n' >> a.txt
check --list "$list" --staged
expect "working tree only" 0

# Fixed strings match in any case.
new_repo
printf 'PlaceHolder.INVALID\n' > b.txt
git add b.txt
check --list "$list" --staged
expect "mixed case" 1 "b.txt:1: denylist hit"

# Regex lines: an address in the range hits, a version number does not.
new_repo
printf 'host 192.0.2.7\n' > c.txt
git add c.txt
check --list "$list" --staged
expect "regex address" 1 "c.txt:1: denylist hit"

new_repo
printf 'cobra 1.10.2\n' > c.txt
git add c.txt
check --list "$list" --staged
expect "version number" 0

# A home path hits.
new_repo
printf 'cd /Users/example/code\n' > d.txt
git add d.txt
check --list "$list" --staged
expect "home path" 1 "d.txt:1: denylist hit"

# A staged file path hits, and the output hides the path.
new_repo
printf 'clean\n' > notes-placeholder.invalid.txt
git add notes-placeholder.invalid.txt
check --list "$list" --staged
expect "staged path" 1 "staged: denylist hit (path)"

# A binary file is checked as text.
new_repo
printf 'a\000b placeholder.invalid\000\n' > blob.bin
git add blob.bin
check --list "$list" --staged
expect "binary file" 1 "blob.bin:1: denylist hit"

# A removed line does not hit.
new_repo
printf 'old placeholder.invalid\n' >> a.txt
git add a.txt
git commit -q -m "add a line"
before=$(git rev-parse HEAD)
printf 'one\ntwo\n' > a.txt
git add a.txt
check --list "$list" --staged
expect "removed line, staged" 0
git commit -q -m "remove a line"
check --list "$list" --push "$(git rev-parse HEAD)" "$before"
expect "removed line, push" 0

# A commit message hit, reported by commit.
new_repo
printf 'clean\n' >> a.txt
git commit -q -am "docs at placeholder.invalid"
sha=$(git rev-parse --short=12 HEAD)
check --list "$list" --push "$(git rev-parse HEAD)" "$zero"
expect "commit message" 1 "$sha: denylist hit (message)"

# A committed file path hit.
new_repo
printf 'clean\n' > dir-placeholder.invalid.md
git add dir-placeholder.invalid.md
git commit -q -m "add notes"
sha=$(git rev-parse --short=12 HEAD)
check --list "$list" --push "$(git rev-parse HEAD)" "$zero"
expect "commit path" 1 "$sha: denylist hit (path)"

# A first push to a new branch scans every commit not on a remote.
new_repo
git init -q --bare "$tmp/remote$n.git"
git remote add origin "$tmp/remote$n.git"
git push -q origin main
check --list "$list" --push "$(git rev-parse HEAD)" "$zero"
expect "nothing new to push" 0
git checkout -q -b feature
printf 'host 192.0.2.44\n' > e.txt
git add e.txt
git commit -q -m "add e"
printf 'more\n' >> e.txt
git commit -q -am "more e"
check --list "$list" --push "$(git rev-parse HEAD)" "$zero"
expect "first push of a new branch" 1 "e.txt:1: denylist hit"
check --list "$list" --push "$(git rev-parse HEAD)" "$(git rev-parse main)"
expect "push with a known remote sha" 1 "e.txt:1: denylist hit"
check --list "$list" --push "$zero" "$(git rev-parse main)"
expect "branch delete" 0

# --all finds a hit that a later commit removed.
new_repo
printf 'x placeholder.invalid\n' > f.txt
git add f.txt
git commit -q -m "add f"
git rm -q f.txt
git commit -q -m "remove f"
check --list "$list" --all
expect "all history" 1 "f.txt:1: denylist hit"

# An empty list (comments and blanks only) or a missing list exits 1.
new_repo
empty="$tmp/empty.txt"
printf '# nothing\n\n   \n' > "$empty"
check --list "$empty" --staged
expect "empty list" 1
check --list "$tmp/missing.txt" --staged
expect "missing list" 1

# A re: line with an empty pattern is rejected.
new_repo
printf 'placeholder.invalid\nre:  \n' > "$tmp/badre.txt"
check --list "$tmp/badre.txt" --staged
expect "empty regex" 1 "empty pattern on line 2"

# Author and committer names and emails are checked.
new_repo
printf 'clean\n' >> a.txt
git add a.txt
GIT_AUTHOR_EMAIL=me@placeholder.invalid git commit -q -m "clean"
sha=$(git rev-parse --short=12 HEAD)
check --list "$list" --push "$(git rev-parse HEAD)" "$zero"
expect "author email" 1 "$sha: denylist hit (author)"

new_repo
printf 'clean\n' >> a.txt
git add a.txt
GIT_COMMITTER_NAME="/Users/example" git commit -q -m "clean"
sha=$(git rev-parse --short=12 HEAD)
check --list "$list" --push "$(git rev-parse HEAD)" "$zero"
expect "committer name" 1 "$sha: denylist hit (committer)"

# --all checks annotated tag messages.
new_repo
git tag -a v1 -m "release notes at placeholder.invalid"
check --list "$list" --all
expect "annotated tag" 1 "denylist hit (tag)"

# Hooks read the list path from git config.
new_repo
check --hook pre-commit
expect "hook with no config" 1 "set agentkit.denylist to your list, or to none"
git config agentkit.denylist none
check --hook pre-commit
expect "hook with none" 0 "warning"
git config agentkit.denylist "$list"
printf 'placeholder.invalid\n' > g.txt
git add g.txt
check --hook pre-commit
expect "pre-commit hook" 1 "g.txt:1: denylist hit"
git commit -q -m "add g"
check --hook pre-push < /dev/null
expect "pre-push hook, no remote" 1 "g.txt:1: denylist hit"

# Ref lines on stdin are read. The hit sits in a commit that no branch or
# tag points at, so only the stdin ref can find it.
new_repo
git config agentkit.denylist "$list"
git init -q --bare "$tmp/remote$n.git"
git remote add origin "$tmp/remote$n.git"
git push -q origin main
printf 'host 192.0.2.9\n' > h.txt
blob=$(git hash-object -w h.txt)
rm h.txt
tree=$(printf '100644 blob %s\th.txt\n' "$blob" | git mktree)
loose=$(printf 'add h\n' | git commit-tree "$tree" -p main)
check --hook pre-push origin "$tmp/remote$n.git" < /dev/null
expect "pre-push hook, unreferenced commit not pushed" 0
printf 'refs/heads/x %s refs/heads/x %s\n' "$loose" "$zero" > "$tmp/refs"
check --hook pre-push origin "$tmp/remote$n.git" < "$tmp/refs"
expect "pre-push hook, refs on stdin" 1 "h.txt:1: denylist hit"

# Under pre-commit the first ref arrives in environment variables.
# check_env <to-ref> <from-ref> <remote>; empty refs count as unset.
check_env() {
	code=0
	out=$(PRE_COMMIT=1 PRE_COMMIT_TO_REF=$1 PRE_COMMIT_FROM_REF=$2 \
		PRE_COMMIT_REMOTE_NAME=$3 "$script" --hook pre-push 2>&1 < /dev/null) || code=$?
	if printf '%s' "$out" | grep -qiE 'placeholder\.invalid|/users/example|192\.0\.2'; then
		code=99
		fail "output leaked matched text (pre-commit env)"
	fi
}
check_env "$loose" "$(git rev-parse main)" origin
expect "pre-push hook, pre-commit variables" 1 "h.txt:1: denylist hit"

# A commit already on another remote is scanned on its first push to the
# target remote.
new_repo
git config agentkit.denylist "$list"
git init -q --bare "$tmp/mirror$n.git"
git init -q --bare "$tmp/public$n.git"
git remote add mirror "$tmp/mirror$n.git"
git remote add public "$tmp/public$n.git"
printf 'see placeholder.invalid\n' > m.txt
git add m.txt
git commit -q -m "add m"
git push -q mirror main
printf 'refs/heads/main %s refs/heads/main %s\n' "$(git rev-parse HEAD)" "$zero" > "$tmp/refs"
check --hook pre-push public "$tmp/public$n.git" < "$tmp/refs"
expect "two remotes, git hook" 1 "m.txt:1: denylist hit"
check_env "" "" public
expect "two remotes, pre-commit first push" 1 "m.txt:1: denylist hit"
check --hook pre-push mirror "$tmp/mirror$n.git" < /dev/null
expect "two remotes, already on the target" 0

# Pre-push checks annotated tags the target remote lacks, and skips the
# ones it already has.
new_repo
git config agentkit.denylist "$list"
git init -q --bare "$tmp/remote$n.git"
git remote add origin "$tmp/remote$n.git"
git push -q origin main
git tag -a v1 -m "notes at placeholder.invalid"
check --hook pre-push origin "$tmp/remote$n.git" < /dev/null
expect "pre-push, new annotated tag" 1 "denylist hit (tag)"
check_env "" "" origin
expect "pre-push under pre-commit, new annotated tag" 1 "denylist hit (tag)"
git push -q origin v1
check --hook pre-push origin "$tmp/remote$n.git" < /dev/null
expect "pre-push, tag already on the remote" 0

# Tags are compared against the push URL, not the fetch URL. The tag is on
# the fetch side only, so pushing must still scan it.
new_repo
git config agentkit.denylist "$list"
git init -q --bare "$tmp/fetch$n.git"
git init -q --bare "$tmp/push$n.git"
git remote add split "$tmp/fetch$n.git"
git remote set-url --push split "$tmp/push$n.git"
git tag -a v1 -m "notes at placeholder.invalid"
git push -q "$tmp/fetch$n.git" main v1
git push -q split main
check --hook pre-push split "$tmp/push$n.git" < /dev/null
expect "split URLs, git hook" 1 "denylist hit (tag)"
check_env "" "" split
expect "split URLs, pre-commit with no URL variable" 1 "denylist hit (tag)"
code=0
out=$(PRE_COMMIT=1 PRE_COMMIT_REMOTE_NAME=split PRE_COMMIT_REMOTE_URL="$tmp/push$n.git" \
	"$script" --hook pre-push 2>&1 < /dev/null) || code=$?
if printf '%s' "$out" | grep -qiE 'placeholder\.invalid|/users/example|192\.0\.2'; then
	code=99
	fail "output leaked matched text (split URLs)"
fi
expect "split URLs, pre-commit URL variable" 1 "denylist hit (tag)"
git push -q split v1
check --hook pre-push split "$tmp/push$n.git" < /dev/null
expect "split URLs, tag at the push URL" 0

# If the push URL cannot be asked, every annotated tag is scanned.
check --hook pre-push split "$tmp/no-such-remote.git" < /dev/null
expect "unreachable push URL scans all tags" 1 "denylist hit (tag)"

# Bad usage exits 2.
check --list "$list"
expect "no mode" 2

echo "passed: $passed, failed: $failed"
[ "$failed" -eq 0 ]
