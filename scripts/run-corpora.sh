#!/usr/bin/env bash
#
# run-corpora.sh — run the project's standing measurement corpora and write a
# normalised, diffable artifact.
#
# "Re-run the standing corpora to show your change did not move them" was an
# instruction with no referent: nothing in the repo said which runs they were.
# They are the four measurement harnesses in internal/engine/activation. They
# already run inside a full `go test ./...`, so this target is not extra
# coverage — it is the artifact. What a reviewer needs is a diff, and raw
# `go test -v` output is not diffable: it interleaves timestamped INFO logs and
# carries source line numbers that move whenever the harness file is edited.
#
# The artifact keeps the measured numbers and the test names, and drops
# timestamps, elapsed times and line numbers. Two runs of an unchanged tree
# produce byte-identical files; a change that moves a corpus shows up as a
# numeric diff and nothing else.
#
# Usage: scripts/run-corpora.sh [output-path]   (default .artifacts/corpora.txt)

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

OUT="${1:-.artifacts/corpora.txt}"
PKG="./internal/engine/activation/..."

# The standing corpora, by name. Adding a measurement harness here is what makes
# it standing; there is no pattern that discovers them, on purpose — "every test
# named TestMeasure*" would silently change meaning under a rename.
HARNESSES=(
	TestMeasureAbstentionGate
	TestMeasureRecallQuerySet
	TestMeasureRelevanceBandHint
	TestMeasureShadowPrecision_GraftedChain
)

pattern="$(
	IFS='|'
	echo "^(${HARNESSES[*]})\$"
)"

mkdir -p "$(dirname "$OUT")"
raw="$(mktemp -t muninn-corpora)"
trap 'rm -f "$raw"' EXIT

echo "==> running ${#HARNESSES[@]} standing corpora in $PKG"
status=0
go test -tags localassets -count=1 -run "$pattern" -v "$PKG" >"$raw" 2>&1 || status=$?

# Normalise: keep RUN/result lines and the harnesses' own t.Log output; drop
# wall-clock timestamps, elapsed times and source line numbers.
{
	echo "# standing corpora — $PKG"
	printf '# %s\n' "${HARNESSES[@]}"
	echo "# normalised by scripts/run-corpora.sh: no timestamps, no elapsed times, no line numbers."
	echo
	sed -n \
		-e 's/^=== RUN[[:space:]]*\(.*\)$/=== RUN \1/p' \
		-e 's/^--- \(PASS\|FAIL\|SKIP\):[[:space:]]*\([^ ]*\).*$/--- \1: \2/p' \
		-e 's/^[[:space:]]*[a-z_0-9]*_test\.go:[0-9]*:[[:space:]]\{0,1\}\(.*\)$/    \1/p' \
		"$raw"
} >"$OUT"

# A run that measured nothing is not a passing run. This is the #814 lesson
# applied to this script: `go test -run` prints "no tests to run" and exits 0,
# so an empty result set has to be checked for explicitly.
missing=()
for h in "${HARNESSES[@]}"; do
	grep -qx "=== RUN $h" "$OUT" || missing+=("$h")
done
if [ "${#missing[@]}" -ne 0 ]; then
	echo "ERROR: these standing corpora did not run at all:" >&2
	printf '  %s\n' "${missing[@]}" >&2
	echo "  (renamed, build-excluded, or filtered away — an unrun corpus is not a passing one)" >&2
	exit 1
fi

if [ "$status" -ne 0 ]; then
	echo "ERROR: corpora run failed (go test exit $status); artifact written to $OUT" >&2
	exit "$status"
fi

echo "==> $OUT ($(wc -l <"$OUT" | tr -d ' ') lines)"
echo "    diff it against the same file from before your change."
