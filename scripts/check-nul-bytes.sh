#!/usr/bin/env bash
#
# check-nul-bytes.sh — fail if a tracked source file contains a NUL byte.
#
# git decides binary-vs-text by sniffing the blob for a NUL byte. A source
# file that happens to contain one — deliberately, as a field delimiter, or
# by accident — is silently classified as binary: `git diff`, `git show
# --stat`, and every GitHub PR view render it as `Bin N -> M bytes` with
# "0 insertions(+), 0 deletions(-)", and its actual contents never appear in
# any diff-based review surface. #827: a 9,191-byte JavaScript file carrying
# two literal NULs (a deliberate, correct field delimiter in a hash — NUL
# cannot occur in the hashed fields, so it looked like the careful choice)
# reached `develop`, was reviewed, and merged with its contents structurally
# invisible the whole way through.
#
# This is the CI-side half of the fix. `.gitattributes`' `diff text` entries
# are the other half: they make git render a diff regardless of content, so
# an accidental NUL becomes visible instead of silencing the file — but nothing
# there stops a NUL from landing in the first place, or fails the build when
# it does. This script is that stop.
#
# The extension list is kept in sync with .gitattributes' `diff text` list by
# hand; both are re-derived from `git ls-files | sed -E 's/.*\.//' | sort -u`
# against the tree, not remembered — re-run that if this list goes stale.
#
# DOES NOT CATCH: a NUL in a tracked file whose extension is not in EXTENSIONS
# below (an image or other binary asset legitimately contains arbitrary bytes,
# so this is deliberately an allowlist of source extensions, not every tracked
# file) or in an UNTRACKED file (not yet anyone else's problem — this fires
# once the file is staged, the same boundary check-filename-build-constraints.sh
# uses for the same reason).

set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

EXTENSIONS="go js mjs ts py php sh yml yaml json proto kt kts swift css html toml md"

pathspecs=()
for ext in $EXTENSIONS; do
	pathspecs+=("*.${ext}")
done

fail=0
checked=0
while IFS= read -r -d '' f; do
	checked=$((checked + 1))
	# A NUL byte cannot be held in a shell variable or matched by a portable
	# grep pattern across BSD and GNU grep (BSD grep has no -P, and both
	# treat an embedded NUL specially in ways that vary by version) — so
	# detect it indirectly: stripping every NUL byte from the file changes
	# its length if and only if it had one. wc -c / tr -d are POSIX and
	# behave identically on macOS (BSD) and Linux (GNU), unlike grep -P.
	before=$(wc -c <"$f")
	after=$(LC_ALL=C tr -d '\000' <"$f" | wc -c)
	if [ "$before" -ne "$after" ]; then
		echo "NUL byte found in tracked source file: $f" >&2
		fail=1
	fi
done < <(git ls-files -z -- "${pathspecs[@]}")

if [ "$fail" -ne 0 ]; then
	echo "" >&2
	echo "A NUL byte makes git classify the file above as binary — invisible to" >&2
	echo "git diff, GitHub's PR view, and every other diff-based review surface" >&2
	echo "(#827). Remove the NUL (e.g. replace a byte delimiter with something" >&2
	echo "printable and unambiguous, such as JSON.stringify([...fields]))." >&2
	exit 1
fi

echo "check-nul-bytes: no NUL bytes found in $checked tracked source file(s)."
