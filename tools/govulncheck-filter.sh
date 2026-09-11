#!/bin/sh
# govulncheck-filter — wrapper around govulncheck that permits documented
# suppressions for pre-existing, unfixable false positives.
#
# Usage: govulncheck-filter.sh [-ignore FILE] [-govulncheck BIN] [govulncheck args...]
#
# Reads GO- advisory ids (one per line, # comments allowed) from the ignore file.
# If govulncheck exits 3 (vulnerabilities found) and every affected "Vulnerability"
# GO- id is listed in the ignore file, the script prints the original output and
# exits 0. Any unignored finding, or any non-vulnerability govulncheck error,
# causes the script to exit non-zero.

set -eu

ignore_file="tools/govulncheck-ignore.txt"
govulncheck_bin="govulncheck"

while [ $# -gt 0 ]; do
	case "$1" in
		-ignore)
			ignore_file="$2"
			shift 2
			;;
		-govulncheck)
			govulncheck_bin="$2"
			shift 2
			;;
		*)
			break
			;;
	esac
done

if [ ! -r "$ignore_file" ]; then
	echo "govulncheck-filter: cannot read ignore file: $ignore_file" >&2
	exit 2
fi

tmpout=$(mktemp)
tmperr=$(mktemp)
tmpfound=$(mktemp)
tmpignored=$(mktemp)
tmpunignored=$(mktemp)
trap 'rm -f "$tmpout" "$tmperr" "$tmpfound" "$tmpignored" "$tmpunignored"' EXIT

rc=0
"$govulncheck_bin" "$@" >"$tmpout" 2>"$tmperr" || rc=$?

cat "$tmperr" >&2

if [ "$rc" -eq 0 ]; then
	cat "$tmpout"
	exit 0
fi

# Any exit code other than 3 is a govulncheck error, not a vulnerability finding.
if [ "$rc" -ne 3 ]; then
	cat "$tmpout"
	exit "$rc"
fi

# Extract GO- ids from affected vulnerabilities. govulncheck only numbers
# affected findings in the text output, so this naturally excludes
# "packages/modules you import but do not call".
grep -E '^Vulnerability #[0-9]+: GO-[0-9]+-[0-9]+$' "$tmpout" \
	| sed 's/^Vulnerability #[0-9]*: //' \
	| sort -u >"$tmpfound"

if [ ! -s "$tmpfound" ]; then
	cat "$tmpout"
	exit "$rc"
fi

grep -E '^GO-[0-9]+-[0-9]+$' "$ignore_file" | sort -u >"$tmpignored"

# comm -23 prints lines only in tmpfound (unignored findings).
comm -23 "$tmpfound" "$tmpignored" >"$tmpunignored"

if [ ! -s "$tmpunignored" ]; then
	cat "$tmpout"
	echo "" >&2
	echo "govulncheck-filter: all affected findings are in the documented ignore list ($ignore_file)." >&2
	exit 0
fi

cat "$tmpout"
exit "$rc"
