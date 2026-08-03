#!/usr/bin/env bash
# check-leak-tells.sh — catch decidable, near-zero-false-positive shapes of
# leaked infrastructure/host identifiers in ADDED lines only.
#
# MuninnDB is public. A vault, client, or host name must never be NAMED in
# committed content (see CLAUDE.md). The maintainer runs an additional
# denylist-based guard (.claude/maintainer/, gitignored — a list of real
# names cannot itself live in a public repo). This script is the part of
# that protection every contributor gets: it has no denylist, so it can only
# catch identifiers with a STRUCTURAL tell — an .internal hostname, an AWS
# instance id, an "our/on the <ProperNoun> ..." phrase. A bare
# project-internal codename with no structural marker — the shape every
# leak in this repo's history has actually taken — passes through
# undetected. This is a net, not a proof.
#
# Rules considered and rejected (measured against this repo's own tree and
# history — see the design note for the numbers):
#   - email addresses: dependency lockfiles (composer.lock, pyproject.toml)
#     carry dozens of legitimate third-party maintainer emails; noise with
#     no history of being the leak vector.
#   - RFC1918/link-local IP ranges (10.x, 192.168.x, 172.16-31.x, 169.254.x):
#     used throughout as safe placeholder addresses in test fixtures and
#     docs/cluster-operations.md, exactly like example.com — same failure
#     shape as generic hostname TLDs, below.
#   - hostnames with real TLDs, and the .local / .lan suffixes: real TLDs
#     match ~2400 lines of legitimate open-source references (github.com,
#     golang.org, npmjs.org, ...); .local and .lan are this repo's own
#     placeholder convention in tests (flag.local, ui.example.lan, ...).
#   - absolute paths under /Users/<name>/ or /home/<name>/: the maintainer's
#     own design docs already carry ~28 legitimate "code read at
#     /Users/<maintainer>/..." breadcrumbs; the only way to keep this rule
#     from firing on them is a name-specific exemption, which would put a
#     real name in tracked content to suppress itself. Rejected.
#   - "Production <ProperNoun>" phrase: this codebase's own prose uses
#     "Production" constantly as a plain adjective before a capitalized
#     technical term or heading noun — "Production Hardening",
#     "Production Readiness Verdict", "production OnWeightUpdate",
#     "production GetEngrams", "production ContentMatch", "Production
#     Checklist" all hit when scanned whole-tree. Ten real false positives;
#     dropped in favor of the two narrower phrase tells below, which stayed
#     at zero.
#
# Rules kept (zero hits across the tracked tree and the last ~200 commits'
# added lines at the time this was written):
#   - .internal hostname suffix (unlike .local/.lan, not used as a
#     placeholder convention anywhere in this codebase).
#   - "our <ProperNoun> server" / "on the <ProperNoun> box", restricted to a
#     following Capitalized token to avoid "our own server" / "our mock
#     server" generic-English hits.
#   - AWS/GCP-style instance identifiers (EC2 instance ids, ARNs).
#
# Usage:
#   scripts/check-leak-tells.sh                  # staged changes (local hook)
#   scripts/check-leak-tells.sh --range A..B     # explicit diff range (CI)
#   scripts/check-leak-tells.sh --install-hook   # opt-in local pre-commit hook
#   scripts/check-leak-tells.sh --selftest       # exercise the rules on
#                                                 # inline, invented fixtures
#
# Bypass (deliberate, logged): ALLOW_LEAK_TELLS=1 git commit …

set -uo pipefail

SELF_RELPATH="scripts/check-leak-tells.sh"

# Patterns run against ADDED lines only (diff `+` lines, stripped of the
# leading marker). One pattern per array entry; kept as extended regexes.
PATTERNS=(
  # .internal hostname suffix
  '[A-Za-z0-9][A-Za-z0-9._-]*\.internal([^A-Za-z0-9]|$)'
  # phrase-shaped tells — require a following Capitalized (proper-noun-shaped)
  # token so ordinary English ("our own server", "our mock server") doesn't
  # hit. "Production <ProperNoun>" was tried and dropped: this repo's own
  # prose uses "Production"/"production" as a plain adjective before a
  # capitalized technical term constantly (see the header note above).
  '\<our[[:space:]]+[A-Z][A-Za-z0-9_-]{2,}[[:space:]]+server'
  '\<on the[[:space:]]+[A-Z][A-Za-z0-9_-]{2,}[[:space:]]+box'
  # AWS-style EC2 instance id / ARN
  '\<i-[0-9a-f]{8,17}\>'
  'arn:aws:'
)

usage() {
  echo "usage: $(basename "$0") [--range A..B] [--install-hook] [--selftest]" >&2
}

install_hook() {
  local common_dir hook line
  # The hooks directory is shared across worktrees (through --git-common-dir);
  # a linked worktree's own .git is a file, not a directory, so writing to
  # "$(show-toplevel)/.git/hooks" fails there. But this script itself is
  # TRACKED content, checked out fresh into every worktree — unlike the
  # maintainer's private, gitignored denylist guard, it must be invoked via
  # --show-toplevel (the current worktree) at run time, not the shared
  # common-dir's parent (the main checkout, which may not have this file
  # checked out at the branch a given worktree is on).
  common_dir="$(git rev-parse --path-format=absolute --git-common-dir)"
  hook="$common_dir/hooks/pre-commit"
  # shellcheck disable=SC2016 # deliberately single-quoted: expands at hook-run time, not here.
  line='bash "$(git rev-parse --show-toplevel)/'"${SELF_RELPATH}"'" || exit 1'
  if [[ -f "$hook" ]] && grep -qF "$SELF_RELPATH" "$hook"; then
    echo "already installed -> $hook"
    return 0
  fi
  if [[ -f "$hook" ]]; then
    printf '\n%s\n' "$line" >> "$hook"
  else
    printf '#!/usr/bin/env bash\n%s\n' "$line" > "$hook"
  fi
  chmod +x "$hook"
  echo "installed -> $hook"
}

# Extract ADDED lines (without the leading '+') from a diff.
added_lines_from_diff() {
  grep -E '^\+' | grep -vE '^\+\+\+' | sed -E 's/^\+//'
}

get_added_lines_staged() {
  git diff --cached --unified=0 -- . ":(exclude)${SELF_RELPATH}" | added_lines_from_diff
}

get_added_lines_range() {
  local range="$1"
  git diff --unified=0 "$range" -- . ":(exclude)${SELF_RELPATH}" | added_lines_from_diff
}

scan() {
  local content="$1"
  local hit=0 pat found
  [[ -z "$content" ]] && return 0
  for pat in "${PATTERNS[@]}"; do
    found="$(printf '%s\n' "$content" | grep -nE "$pat" || true)"
    if [[ -n "$found" ]]; then
      hit=1
      echo "  pattern: $pat"
      printf '%s\n' "$found" | sed 's/^/    added: /'
    fi
  done
  return $hit
}

selftest() {
  local fail=0

  # Positive fixtures: each shape below MUST be flagged. Names are invented.
  local -a positive=(
    "the archived config still lives on db1.internal, so the old default has to survive"
    "our Coyotefleet server never saw the migration"
    "on the Ranchhand box the timeout was still 30s"
    "instance i-0a1b2c3d4e5f67890 never rejoined the cluster"
    "arn:aws:iam::000000000000:role/example-role was over-scoped"
  )
  # Negative fixtures: ordinary content that must NOT be flagged.
  local -a negative=(
    "our own server handles this fine"
    "our mock server returns a canned response"
    "see https://github.com/scrypster/muninndb for the source"
    "default listen host is 10.0.0.1 in the fixture"
    "override.lan is used as the test placeholder domain"
    "code read at /Users/maintainer/github.com/scrypster/muninndb"
    "Production Hardening checklist, iteration ten"
    "in production OnWeightUpdate feeds the engine"
  )

  local line
  for line in "${positive[@]}"; do
    if scan "$line" >/dev/null; then
      echo "SELFTEST FAIL (expected hit, got none): $line"
      fail=1
    fi
  done
  for line in "${negative[@]}"; do
    if ! scan "$line" >/dev/null; then
      echo "SELFTEST FAIL (expected clean, got hit): $line"
      fail=1
    fi
  done

  if [[ "$fail" -eq 0 ]]; then
    echo "selftest OK: ${#positive[@]} positive, ${#negative[@]} negative fixtures behaved as expected."
  fi
  return $fail
}

MODE="staged"
RANGE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --range)
      MODE="range"; RANGE="${2:-}"; shift 2 ;;
    --install-hook)
      install_hook; exit 0 ;;
    --selftest)
      selftest; exit $? ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ "${ALLOW_LEAK_TELLS:-}" == "1" ]]; then
  echo "check-leak-tells: BYPASSED via ALLOW_LEAK_TELLS=1" >&2
  exit 0
fi

case "$MODE" in
  staged)
    ADDED="$(get_added_lines_staged)"
    ;;
  range)
    if [[ -z "$RANGE" ]]; then
      echo "check-leak-tells: --range requires A..B" >&2
      exit 2
    fi
    ADDED="$(get_added_lines_range "$RANGE")"
    ;;
esac

echo "check-leak-tells: scanning added lines ($MODE)…"
if scan "$ADDED"; then
  exit 0
fi

cat >&2 <<'MSG'

check-leak-tells: BLOCKED — a structurally-shaped infrastructure/host
identifier was added.

MuninnDB is public. Real hostnames, instance ids, and "our/on the <Name> ..."
references to a real host must never be committed — rewrite the
reference generically (e.g. "a production vault", "the affected host") and
keep any measurement or number, which is the point.

This check only catches identifiers with a structural tell. It is not a
substitute for reading your own diff.

Deliberate exception: ALLOW_LEAK_TELLS=1 git commit …
MSG
exit 1
