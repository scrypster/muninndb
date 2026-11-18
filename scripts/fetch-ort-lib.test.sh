#!/usr/bin/env bash
#
# fetch-ort-lib.test.sh: the refusal paths of scripts/fetch-ort-lib.sh.
#
# Hermetic. The mirror is a file:// directory and upstream is skipped, so no
# network is touched. Run: bash scripts/fetch-ort-lib.test.sh
#
# Cases 1 to 6 and 8 are ones where the script must install NOTHING, and each
# also checks the line the script printed, so a script that simply never
# downloads cannot pass them. Case 7 is the positive control: the same script
# must install a file when the hash matches. The real library bytes are not in
# the repository and an override for the pinned hash would be a hole in the
# thing under test, so cases 5 to 8 reach the install step by replacing the
# hash tool with a shim.
#
# DOES NOT CATCH: a regression in the upstream archive extraction, or a wrong
# pinned hash. Those need a real download.

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/fetch-ort-lib.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fails=0
target="linux-amd64"
dest="libonnxruntime_linux_amd64.so"
pinned="ffc84d48e845cf0b562ba4ea5ca32aaafc0d4069019fef4f63095b307d0270ad"

# expect <name> <wanted exit> <assets dir> <"empty"|"installed"|"keep"|"untouched:<text>"> -- cmd...
# When want_log is set, the output must also contain that text.
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
    installed)
      [[ -f "$assets/$dest" ]] || ok=0
      if [[ "$(find "$assets" -type f | wc -l)" -ne 1 ]]; then ok=0; fi
      ;;
    untouched:*)
      [[ "$(cat "$assets/$dest" 2>/dev/null)" == "${state#untouched:}" ]] || ok=0
      if [[ "$(find "$assets" -type f | wc -l)" -ne 1 ]]; then ok=0; fi
      ;;
  esac
  if [[ -n "${want_log:-}" ]] && ! grep -q -- "$want_log" "$work/out.log"; then ok=0; fi
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
want_log="usage:" expect "unknown target is refused" 2 "$a" empty -- \
  env ASSETS_DIR="$a" "$script" solaris-sparc

# 2. No mirror configured and upstream skipped.
a="$work/a2"; mkdir -p "$a"
want_log="no mirror configured" expect "no source available installs nothing" 1 "$a" empty -- \
  env ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="" "$script" "$target"

# 3. The mirror serves a file with the wrong bytes.
a="$work/a3"; mkdir -p "$a" "$work/m3"
printf 'not the library\n' > "$work/m3/$dest"
want_log="mirror gave a file with sha256" expect "wrong bytes from the mirror are refused" 1 "$a" empty -- \
  env ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

# 4. A wrong file already at the destination is not replaced by wrong bytes
#    and is not removed either.
a="$work/a4"; mkdir -p "$a"
printf 'stale' > "$a/$dest"
want_log="mirror gave a file with sha256" expect "a stale file is left as it was" 1 "$a" "untouched:stale" -- \
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
want_log="could not copy the library" expect "a failed copy installs nothing" 1 "$a" empty -- \
  env PATH="$work/shim:$PATH" ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

# 6. The copy is cut short but reports success. Hashing is real here, except
#    that the first call (on the download) is answered with the pinned value
#    so the install step is reached. The second call hashes the copy for real.
a="$work/a6"; mkdir -p "$a" "$work/shim6"
real_hash="$(command -v sha256sum || command -v shasum)"
cat > "$work/shim6/sha256sum" <<EOF
#!/usr/bin/env bash
if [[ ! -e "$work/shim6/called" ]]; then
  : > "$work/shim6/called"
  echo "$pinned  \$1"
  exit 0
fi
case "$real_hash" in
  *shasum) exec "$real_hash" -a 256 "\$@" ;;
  *) exec "$real_hash" "\$@" ;;
esac
EOF
cat > "$work/shim6/cp" <<'EOF'
#!/usr/bin/env bash
for last; do :; done
printf 'trunc' > "$last"
exit 0
EOF
chmod +x "$work/shim6/sha256sum" "$work/shim6/cp"
want_log="the copy in .* has sha256" expect "a copy cut short is not installed" 1 "$a" empty -- \
  env PATH="$work/shim6:$PATH" ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

# 7. Positive control. With the hash shim from case 5 and the real cp, the
#    file must be installed. A script that never installs fails here.
a="$work/a7"; mkdir -p "$a" "$work/shim7"
cp "$work/shim/sha256sum" "$work/shim7/sha256sum"
want_log="from mirror, sha256 verified" expect "a matching file is installed" 0 "$a" installed -- \
  env PATH="$work/shim7:$PATH" ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

# 8. A directory at the destination is an error, not a silent success.
a="$work/a8"; mkdir -p "$a/$dest"
want_log="is a directory" expect "a directory at the destination is refused" 1 "$a" keep -- \
  env PATH="$work/shim7:$PATH" ASSETS_DIR="$a" ORT_SKIP_UPSTREAM=1 ORT_MIRROR_BASE="file://$work/m3" "$script" "$target"

if [[ "$fails" -ne 0 ]]; then
  echo "fetch-ort-lib.test: FAILED"
  exit 1
fi
echo "fetch-ort-lib.test: all cases hold"
