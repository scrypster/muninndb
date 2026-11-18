#!/usr/bin/env bash
#
# fetch-ort-lib.test.sh: the refusal paths of scripts/fetch-ort-lib.sh.
#
# Hermetic. The mirror is a file:// directory and upstream is skipped, so no
# network is touched. Run: bash scripts/fetch-ort-lib.test.sh
#
# Every case here is one where the script must install NOTHING. The success
# path needs the real library bytes, which are not in the repository, and an
# override for the pinned hash would be a hole in the thing under test, so
# there is none. Case 5 reaches the install step by replacing the hash tool
# with a shim that always prints the pinned value.
#
# DOES NOT CATCH: a regression in the success path (right bytes, not installed)
# or in the upstream archive extraction. Those need a real download.

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/fetch-ort-lib.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fails=0
target="linux-amd64"
dest="libonnxruntime_linux_amd64.so"
pinned="ffc84d48e845cf0b562ba4ea5ca32aaafc0d4069019fef4f63095b307d0270ad"

# expect <name> <wanted exit> <assets dir> <"empty"|"untouched:<text>"> -- cmd...
expect() {
  local name="$1" want_rc="$2" assets="$3" state="$4" rc
  shift 5
  "$@" >"$work/out.log" 2>&1
  rc=$?
  local ok=1
  [[ "$rc" -eq "$want_rc" ]] || ok=0
  case "$state" in
    empty)
      if [[ -n "$(ls -A "$assets" 2>/dev/null)" ]]; then ok=0; fi
      ;;
    untouched:*)
      [[ "$(cat "$assets/$dest" 2>/dev/null)" == "${state#untouched:}" ]] || ok=0
      if [[ "$(find "$assets" -type f | wc -l)" -ne 1 ]]; then ok=0; fi
      ;;
  esac
  if [[ "$ok" -eq 1 ]]; then
    echo "ok   $name"
  else
    echo "FAIL $name (exit $rc, wanted $want_rc, assets: $(find "$assets" -mindepth 1 -maxdepth 1 -exec basename {} \; 2>/dev/null | tr '\n' ' '))"
    sed 's/^/       /' "$work/out.log"
    fails=1
  fi
}

# 1. An unknown target is a usage error.
a="$work/a1"; mkdir -p "$a"
expect "unknown target is refused" 2 "$a" empty -- \
  env ASSETS_DIR="$a" "$script" solaris-sparc

# 2. No mirror configured and upstream skipped.
a="$work/a2"; mkdir -p "$a"
expect "no source available installs nothing" 1 "$a" empty -- \
  env ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="" "$script" "$target"

# 3. The mirror serves a file with the wrong bytes.
a="$work/a3"; mkdir -p "$a" "$work/m3"
printf 'not the library\n' > "$work/m3/$dest"
expect "wrong bytes from the mirror are refused" 1 "$a" empty -- \
  env ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

# 4. A wrong file already at the destination is not replaced by wrong bytes
#    and is not removed either.
a="$work/a4"; mkdir -p "$a"
printf 'stale' > "$a/$dest"
expect "a stale file is left as it was" 1 "$a" "untouched:stale" -- \
  env ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

# 5. The copy into the assets directory fails part way (a full disk). The
#    hash shim makes the candidate look pinned so the install step is reached.
#    The script must exit non-zero and leave no library behind.
a="$work/a5"; mkdir -p "$a" "$work/shim"
cat > "$work/shim/sha256sum" <<EOF
#!/usr/bin/env bash
echo "$pinned  \$1"
EOF
cat > "$work/shim/cp" <<'EOF'
#!/usr/bin/env bash
# Write a truncated file, then fail like ENOSPC.
for last; do :; done
printf 'trunc' > "$last"
echo "cp: $last: No space left on device" >&2
exit 1
EOF
chmod +x "$work/shim/sha256sum" "$work/shim/cp"
expect "a failed copy installs nothing" 1 "$a" empty -- \
  env PATH="$work/shim:$PATH" ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

if [[ "$fails" -ne 0 ]]; then
  echo "fetch-ort-lib.test: FAILED"
  exit 1
fi
echo "fetch-ort-lib.test: all refusal paths hold"
