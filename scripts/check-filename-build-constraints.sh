#!/usr/bin/env bash
#
# check-filename-build-constraints.sh — fail if a source file's NAME silently
# excludes it from the build.
#
# The Go toolchain applies an implicit build constraint to any file whose name,
# after stripping the extension and a trailing "_test", ends in "_GOOS",
# "_GOARCH" or "_GOOS_GOARCH". This is filename matching, not content: no
# //go:build line is needed and none is reported. A file so named simply is not
# compiled on other platforms, and the toolchain says nothing.
#
# #814: a scratch file added as `zz_base_arm_test.go` was excluded on every
# machine in the project (nobody builds GOARCH=arm). `go test -run ...` printed
#
#     testing: warning: no tests to run
#     ok      github.com/scrypster/muninndb/internal/engine   0.310s
#
# and exited 0. A RED-first check that runs zero tests is indistinguishable from
# a passing baseline, so the "fails without the fix" evidence was fabricated by
# the toolchain rather than observed. The non-test case is worse: a production
# file dropped this way changes behaviour, not just coverage.
#
# The check is an allowlist, not a pattern ban, because platform-specific files
# are legitimate and this repo has five of them. A new implicitly-constrained
# file must be named here on purpose.
#
# Runs in CI's shellcheck job and via `make check-filenames`. It is deliberately
# NOT a Go test: a Go test guarding filename-induced build exclusion can itself
# be excluded by filename-induced build exclusion, and would then pass silently
# forever. The guard must not be enforced by the mechanism it guards against.
#
# WHAT THIS DOES NOT CATCH: an explicit `//go:build` line that excludes a file
# everywhere (`//go:build ignore`, a tag nothing sets, a negated tag). Those are
# visible in the file's own first lines; this class is invisible there. It also
# does not check files outside `git ls-files` (untracked scratch is exactly the
# #814 case, but an untracked file is not yet anyone else's problem — the guard
# fires when it is staged).

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Tokens that trigger implicit filename constraints, transcribed from the Go
# toolchain's own tables: internal/syslist/syslist.go, KnownOS and KnownArch
# ("Do not remove from this list, as it is used for filename matching").
# Cross-checked against the installed toolchain below, so this is regenerable
# rather than remembered.
#
# NOTE "unix" is absent on purpose. syslist.go's UnixOS carries the comment
# "This is not used for filename matching" — `process_unix.go` in this repo is
# constrained by an explicit //go:build line, not by its name. A token list that
# included "unix" would flag files the toolchain does not constrain, and a guard
# whose stated rule is wider than the mechanism is the defect class this repo
# spent a day removing from its own documentation.
KNOWN_OS="aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd openbsd plan9 solaris wasip1 windows zos"
KNOWN_ARCH="386 amd64 amd64p32 arm armbe arm64 arm64be loong64 mips mipsle mips64 mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm"
TOKENS="$KNOWN_OS $KNOWN_ARCH"

# Files whose implicit platform constraint is intentional. Adding a line here is
# the acknowledgement; there is no way to add such a file without one.
ALLOWED="
internal/plugin/embed/local_assets_darwin_amd64.go
internal/plugin/embed/local_assets_darwin_arm64.go
internal/plugin/embed/local_assets_linux_amd64.go
internal/plugin/embed/local_assets_linux_arm64.go
internal/plugin/embed/local_assets_windows_amd64.go
cmd/muninn/process_windows.go
cmd/muninn/process_windows_test.go
"

fail=0

is_allowed() {
	local f="$1" a
	for a in $ALLOWED; do
		[ "$a" = "$f" ] && return 0
	done
	return 1
}

# --- 1. every implicitly-constrained source file must be allowlisted ----------

constrained=""
while IFS= read -r f; do
	base="${f##*/}"
	stem="${base%.*}"
	stem="${stem%_test}"
	for tok in $TOKENS; do
		case "$stem" in
		*"_$tok")
			constrained="$constrained$f"$'\n'
			if ! is_allowed "$f"; then
				if [ "$fail" -eq 0 ]; then
					echo "ERROR: source file(s) silently excluded from the build by filename:"
					echo
				fi
				kind="production"
				case "$base" in *_test.go) kind="test" ;; esac
				echo "  $f"
				echo "      the trailing \"_$tok\" is a GOOS/GOARCH filename constraint, so this"
				echo "      $kind file compiles only on that platform. No //go:build line is"
				echo "      needed for that and none is reported; go build/test exit 0 either way."
				echo
				fail=1
			fi
			break
			;;
		esac
	done
done < <(git ls-files '*.go' '*.s')

if [ "$fail" -ne 0 ]; then
	echo "  If the constraint is intentional, add the path to ALLOWED in"
	echo "  scripts/check-filename-build-constraints.sh. If it is not, rename the file"
	echo "  (e.g. foo_arm_test.go -> foo_armscale_test.go). See #814 and"
	echo "  docs/internals/claim-discipline.md."
	echo
fi

# --- 2. no stale allowlist entries -------------------------------------------

for a in $ALLOWED; do
	if ! printf '%s' "$constrained" | grep -qx "$a"; then
		echo "ERROR: allowlist entry no longer applies: $a"
		echo "       (file removed, renamed, or no longer platform-suffixed) — drop the line."
		fail=1
	fi
done

# --- 3. the token list is checked against the toolchain, not remembered -------

syslist=""
if command -v go >/dev/null 2>&1; then
	goroot="$(go env GOROOT 2>/dev/null || true)"
	for cand in "$goroot/src/internal/syslist/syslist.go" "$goroot/src/go/build/syslist.go"; do
		if [ -n "$goroot" ] && [ -f "$cand" ]; then
			syslist="$cand"
			break
		fi
	done
fi

if [ -n "$syslist" ]; then
	# Every quoted key in the KnownOS/KnownArch maps, in source order.
	toolchain_tokens="$(
		sed -n '/^var Known/,/^}/p' "$syslist" |
			sed -n 's/^[[:space:]]*"\([^"]*\)":.*/\1/p' | sort -u
	)"
	if [ -z "$toolchain_tokens" ]; then
		echo "WARNING: found $syslist but parsed no tokens from it — token list NOT cross-checked."
	else
		missing="$(comm -23 <(printf '%s\n' "$toolchain_tokens") <(printf '%s\n' "$TOKENS" | tr ' ' '\n' | sort -u))"
		if [ -n "$missing" ]; then
			echo "ERROR: this toolchain knows GOOS/GOARCH tokens the guard does not:"
			printf '  %s\n' "$missing"
			echo "       Add them to KNOWN_OS/KNOWN_ARCH above."
			fail=1
		else
			echo "token list cross-checked against $syslist ($(printf '%s\n' "$toolchain_tokens" | wc -l | tr -d ' ') tokens)"
		fi
	fi
else
	echo "WARNING: Go toolchain source not found — token list NOT cross-checked this run."
fi

if [ "$fail" -ne 0 ]; then
	exit 1
fi

echo "filename build constraints: ok ($(printf '%s' "$constrained" | grep -c . || true) intentional platform files, all allowlisted)"
