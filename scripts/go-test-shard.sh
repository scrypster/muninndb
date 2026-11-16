#!/usr/bin/env bash
# go-test-shard.sh — split one Go package's tests into stable shards.
#
# Usage:
#   scripts/go-test-shard.sh INDEX COUNT PACKAGE [go test flags...]
#       prints the -run regex selecting shard INDEX (0-based) of COUNT
#   scripts/go-test-shard.sh --check COUNT PACKAGE [go test flags...]
#       verifies the COUNT shards partition the package's tests exactly
#       (every name in exactly one shard, no shard empty); exits non-zero if not
#
# The go test flags (e.g. -tags localassets -race) are passed to
# `go test -list`, so the listing sees exactly the files the run will compile.
#
# The rule: a top-level Test/Example/Fuzz name goes to shard
# hash(name) mod COUNT, where hash is a fixed polynomial over the name's
# characters. A test's shard depends on its own name only, so adding or
# removing a test never moves any other test between shards — a shard's
# timing changes only by what was added to it. Benchmarks are not listed:
# `go test` without -bench does not run them.
#
# Why this exists: internal/engine is the one package whose -race run is
# near the per-package `-timeout 300s` cliff in CI (see
# docs/internals/drift-and-obligations.md, "CI map and budget"). Sharding
# gives each shard its own test binary, its own timeout, and its own runner.
set -euo pipefail

usage() {
	echo "usage: $0 INDEX COUNT PACKAGE [go test flags...]" >&2
	echo "       $0 --check COUNT PACKAGE [go test flags...]" >&2
	exit 2
}

[ $# -ge 3 ] || usage
mode=select
if [ "$1" = "--check" ]; then
	mode=check
	index=0
else
	index=$1
fi
count=$2
pkg=$3
shift 3

case "$count" in '' | *[!0-9]*) usage ;; esac
case "$index" in '' | *[!0-9]*) usage ;; esac
[ "$count" -ge 1 ] || usage
[ "$index" -lt "$count" ] || usage

# `go test -list` prints matching names, then an "ok <pkg>" line. A build
# failure must fail this script, not yield an empty shard.
listing=$(go test "$@" -list '.*' "$pkg")
names=$(printf '%s\n' "$listing" | grep -E '^(Test|Example|Fuzz)[A-Za-z0-9_]*$' || true)
if [ -z "$names" ]; then
	echo "go-test-shard: no tests listed for $pkg" >&2
	exit 1
fi

# assign prints "<shard> <name>" for every name.
assign() {
	printf '%s\n' "$names" | awk -v n="$count" '
		BEGIN {
			alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_"
			for (i = 1; i <= length(alphabet); i++) ord[substr(alphabet, i, 1)] = i
		}
		{
			h = 0
			for (i = 1; i <= length($0); i++) h = (h * 31 + ord[substr($0, i, 1)]) % 1000003
			print (h % n), $0
		}'
}

if [ "$mode" = "check" ]; then
	total=$(printf '%s\n' "$names" | wc -l | tr -d ' ')
	assigned=$(assign | awk '{print $2}' | sort -u | wc -l | tr -d ' ')
	if [ "$assigned" != "$total" ]; then
		echo "go-test-shard: $assigned of $total names assigned" >&2
		exit 1
	fi
	status=0
	for ((k = 0; k < count; k++)); do
		c=$(assign | awk -v k="$k" '$1 == k' | wc -l | tr -d ' ')
		echo "shard $k/$count: $c tests"
		if [ "$c" = "0" ]; then
			echo "go-test-shard: shard $k is empty — lower COUNT" >&2
			status=1
		fi
	done
	echo "total: $total tests"
	exit "$status"
fi

selected=$(assign | awk -v k="$index" '$1 == k {print $2}')
if [ -z "$selected" ]; then
	echo "go-test-shard: shard $index of $count is empty — lower COUNT" >&2
	exit 1
fi
printf '^(%s)$\n' "$(printf '%s\n' "$selected" | paste -sd '|' -)"
