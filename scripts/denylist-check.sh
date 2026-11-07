#!/usr/bin/env bash
# Checks git changes against a denylist of strings that must never be
# published. Prints where a hit is, never the matched text.
#
# usage:
#   denylist-check.sh --list <file> --staged
#   denylist-check.sh --list <file> --push <local-sha> <remote-sha>
#   denylist-check.sh --list <file> --all
#   denylist-check.sh --hook pre-commit
#   denylist-check.sh --hook pre-push [<remote-name> [<remote-url>]]
#
# List format: one entry per line. Blank lines and lines starting with #
# are skipped. A line starting with re: is an extended regular expression;
# any other line is a fixed string. Both match in any case.
#
# It checks added lines and changed file paths, and for commits also the
# message and the author and committer names and emails. --all also checks
# annotated tag objects.
# Exit codes: 0 clean, 1 a hit or a missing or empty list, 2 bad usage.
set -euo pipefail

# Match byte-wise on every platform.
export LC_ALL=C

usage() {
	cat >&2 <<'EOF'
usage: denylist-check.sh --list <file> (--staged | --push <local-sha> <remote-sha> | --all)
       denylist-check.sh --hook pre-commit
       denylist-check.sh --hook pre-push [<remote-name> [<remote-url>]]
EOF
	exit 2
}

die() {
	echo "denylist-check: $*" >&2
	exit 1
}

is_zero() { [[ $1 =~ ^0+$ ]]; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
found=0
# Commits on these remote-tracking refs count as already published. Manual
# --push mode trusts every remote; the pre-push hook narrows this to the
# remote being pushed to.
published=--remotes

# load_list splits the list into fixed-string and regex pattern files.
# Patterns stay in files and reach grep through -f, never through argv.
load_list() {
	local list=$1 line n=0
	[ -f "$1" ] && [ -r "$1" ] || die "denylist not found or not readable"
	: > "$work/fixed"
	: > "$work/re"
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		line=${line%$'\r'}
		case $line in
		'' | '#'*) continue ;;
		esac
		# Skip whitespace-only lines.
		[[ $line =~ ^[[:space:]]*$ ]] && continue
		case $line in
		re:*)
			# An empty regex would match every line.
			if [[ ${line#re:} =~ ^[[:space:]]*$ ]]; then
				die "empty pattern on line $n"
			fi
			printf '%s\n' "${line#re:}" >> "$work/re"
			;;
		*) printf '%s\n' "$line" >> "$work/fixed" ;;
		esac
	done < "$list"
	[ -s "$work/fixed" ] || [ -s "$work/re" ] || die "denylist is empty"
	# Reject a bad pattern before the scan starts.
	: > "$work/hits.raw"
	if [ -s "$work/fixed" ]; then run_grep -F -f "$work/fixed" /dev/null; fi
	if [ -s "$work/re" ]; then run_grep -E -f "$work/re" /dev/null; fi
}

run_grep() { # run_grep <grep args...>; appends matches to hits.raw
	local rc=0
	grep -a -n -i "$@" >> "$work/hits.raw" || rc=$?
	[ "$rc" -le 1 ] || die "grep failed; check the patterns in the list"
}

# hit_lines <file> prints the numbers of lines in <file> that hit.
hit_lines() {
	: > "$work/hits.raw"
	if [ -s "$work/fixed" ]; then run_grep -F -f "$work/fixed" "$1"; fi
	if [ -s "$work/re" ]; then run_grep -E -f "$work/re" "$1"; fi
	cut -d: -f1 "$work/hits.raw" | sort -n -u
}

# text_hits <text> succeeds when the text hits.
text_hits() {
	printf '%s\n' "$1" > "$work/text"
	[ -n "$(hit_lines "$work/text")" ]
}

# show_path prints a path, or a stand-in when the path itself hits.
show_path() {
	if text_hits "$1"; then
		printf '%s' "<path hidden>"
	else
		printf '%s' "$1"
	fi
}

# scan_diff <label> <git diff args...> checks added lines and changed paths.
# <label> is "" for the index, or a commit id.
scan_diff() {
	local label=$1 prefix where nums
	shift
	prefix=""
	where=staged
	if [ -n "$label" ]; then
		prefix="$label:"
		where=$label
	fi

	git -c core.quotePath=false diff --no-color --no-ext-diff --no-textconv \
		--text -U0 --no-prefix --diff-filter=ACMRT "$@" | tr '\000' '\001' > "$work/diff"

	# NUL bytes are \001 here. Write each added line to one file and its
	# path:line to the same line number in another.
	awk -v added="$work/added" -v where_file="$work/where" '
		BEGIN { printf "" > added; printf "" > where_file }
		/^diff --git / { hdr = 1; path = ""; next }
		hdr && /^\+\+\+ / { path = substr($0, 5); sub(/\t$/, "", path); next }
		/^@@ / {
			hdr = 0
			match($0, /\+[0-9]+/)
			ln = substr($0, RSTART + 1, RLENGTH - 1) + 0
			next
		}
		hdr { next }
		/^\+/ {
			print substr($0, 2) > added
			print path "\t" ln > where_file
			ln++
		}
	' "$work/diff"

	nums=$(hit_lines "$work/added")
	if [ -n "$nums" ]; then
		found=1
		printf '%s\n' "$nums" > "$work/nums"
		awk -F'\t' 'NR == FNR { want[$1] = 1; next } want[FNR] { print $1 "\t" $2 }' \
			"$work/nums" "$work/where" | while IFS=$'\t' read -r path ln; do
			echo "$prefix$(show_path "$path"):$ln: denylist hit"
		done
	fi

	git -c core.quotePath=false diff --name-only --diff-filter=ACMRT "$@" > "$work/paths"
	nums=$(hit_lines "$work/paths")
	if [ -n "$nums" ]; then
		found=1
		for _ in $nums; do
			echo "$where: denylist hit (path)"
		done
	fi
}

scan_staged() {
	scan_diff "" --cached
}

empty_tree() { git hash-object -t tree /dev/null; }

scan_commit() {
	local c=$1 short base
	short=$(git rev-parse --short=12 "$c")
	base=$(git rev-parse -q --verify "$c^1" 2>/dev/null) || base=$(empty_tree)
	scan_diff "$short" "$base" "$c"
	git log -1 --format=%B "$c" > "$work/msg"
	if [ -n "$(hit_lines "$work/msg")" ]; then
		found=1
		echo "$short: denylist hit (message)"
	fi
	git log -1 --format='%an%n%ae' "$c" > "$work/author"
	if [ -n "$(hit_lines "$work/author")" ]; then
		found=1
		echo "$short: denylist hit (author)"
	fi
	git log -1 --format='%cn%n%ce' "$c" > "$work/committer"
	if [ -n "$(hit_lines "$work/committer")" ]; then
		found=1
		echo "$short: denylist hit (committer)"
	fi
}

# scan_tags [<file of object ids to skip>] checks every annotated tag
# object: its name, tagger and message.
scan_tags() {
	local obj type skip=${1:-}
	git for-each-ref --format='%(objectname) %(objecttype)' refs/tags > "$work/tags"
	while read -r obj type; do
		if [ -n "$skip" ] && grep -qxF "$obj" "$skip"; then
			continue
		fi
		if [ "$type" = tag ]; then
			git cat-file tag "$obj" > "$work/tag"
			if [ -n "$(hit_lines "$work/tag")" ]; then
				found=1
				echo "$(git rev-parse --short=12 "$obj"): denylist hit (tag)"
			fi
		fi
	done < "$work/tags"
}

scan_commits() { # scan_commits <file of commit ids>
	local c
	sort -u "$1" > "$work/commits.sorted"
	while IFS= read -r c; do
		if [ -n "$c" ]; then scan_commit "$c"; fi
	done < "$work/commits.sorted"
}

# push_commits <local> <remote> appends the commits a push would send.
push_commits() {
	local lsha=$1 rsha=$2
	is_zero "$lsha" && return 0 # a branch delete sends nothing
	if is_zero "$rsha" || ! git cat-file -e "$rsha^{commit}" 2>/dev/null; then
		if [ -n "$published" ]; then
			git rev-list "$lsha" --not "$published" >> "$work/commits"
		else
			git rev-list "$lsha" >> "$work/commits"
		fi
	else
		git rev-list "$rsha..$lsha" >> "$work/commits"
	fi
}

run_hook() {
	local list
	list=$(git config --get agentkit.denylist || true)
	[ -n "$list" ] || die "set agentkit.denylist to your list, or to none"
	if [ "$list" = none ]; then
		echo "denylist-check: warning: agentkit.denylist is none, so the denylist check is skipped" >&2
		exit 0
	fi
	load_list "$list"
	case $1 in
	pre-commit) scan_staged ;;
	pre-push)
		# Only commits on the target remote count as published. git passes the
		# remote name as the hook's first argument; pre-commit sets
		# PRE_COMMIT_REMOTE_NAME.
		local remote=${2:-${PRE_COMMIT_REMOTE_NAME:-}}
		local url=${3:-${PRE_COMMIT_REMOTE_URL:-}}
		if [ -n "$remote" ]; then
			published="--remotes=$remote"
		else
			# Unknown target: treat nothing as published and scan it all.
			published=""
		fi
		: > "$work/commits"
		if [ -n "${PRE_COMMIT:-}" ]; then
			# pre-commit reads git's ref lines itself and passes only the
			# first ref through these variables.
			if [ -n "${PRE_COMMIT_TO_REF:-}" ]; then
				push_commits "$PRE_COMMIT_TO_REF" "${PRE_COMMIT_FROM_REF:-0}"
			fi
		else
			local lsha rsha
			while read -r _ lsha _ rsha; do
				if [ -n "${lsha:-}" ]; then push_commits "$lsha" "${rsha:-0}"; fi
			done
		fi
		# Also scan every local branch and tag commit the target remote lacks.
		if [ -n "$published" ]; then
			git rev-list --branches --tags --not "$published" >> "$work/commits"
		else
			git rev-list --branches --tags >> "$work/commits"
		fi
		scan_commits "$work/commits"
		# Scan annotated tags the push URL lacks, or all of them if it cannot
		# be asked. Never prompt for a password or key.
		: > "$work/remote-tags"
		if [ -z "$url" ] && [ -n "$remote" ]; then
			url=$(git remote get-url --push "$remote" 2>/dev/null || true)
		fi
		if [ -n "$url" ]; then
			local ssh
			ssh=${GIT_SSH_COMMAND:-$(git config --get core.sshCommand || echo ssh)}
			if GIT_TERMINAL_PROMPT=0 GIT_SSH_COMMAND="$ssh -o BatchMode=yes" \
				git ls-remote --tags "$url" > "$work/ls-remote" 2>/dev/null < /dev/null; then
				cut -f1 "$work/ls-remote" > "$work/remote-tags"
			fi
		fi
		scan_tags "$work/remote-tags"
		;;
	*) usage ;;
	esac
}

git rev-parse --git-dir > /dev/null 2>&1 || die "not inside a git repository"

[ $# -ge 1 ] || usage
if [ "$1" = --hook ]; then
	[ $# -ge 2 ] && [ $# -le 4 ] || usage
	if [ "$2" = pre-commit ] && [ $# -ne 2 ]; then usage; fi
	run_hook "$2" "${3:-}" "${4:-}"
	exit "$found"
fi

[ "$1" = --list ] && [ $# -ge 3 ] || usage
load_list "$2"
mode=$3
shift 3
case $mode in
--staged)
	[ $# -eq 0 ] || usage
	scan_staged
	;;
--push)
	[ $# -eq 2 ] || usage
	: > "$work/commits"
	push_commits "$1" "$2"
	scan_commits "$work/commits"
	;;
--all)
	[ $# -eq 0 ] || usage
	git rev-list --all > "$work/commits"
	scan_commits "$work/commits"
	scan_tags
	;;
*) usage ;;
esac
exit "$found"
