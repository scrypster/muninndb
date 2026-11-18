#!/usr/bin/env bash
#
# fetch-ort-lib.sh: install one ONNX Runtime native library into the embed
# assets directory, and refuse any file whose sha256 is not the pinned one.
#
# Usage: scripts/fetch-ort-lib.sh <darwin-arm64|darwin-amd64|linux-amd64|linux-arm64|windows-amd64>
#
# Why this exists. The build used to download each library straight from the
# upstream release page with no integrity check and no second source. When
# that page stopped answering, a fresh clone, the Docker image build and any
# CI run with a cold asset cache could not build at all, and nothing would
# have noticed a changed binary on the day it came back.
#
# What it does, in order:
#   1. If the destination file already has the pinned sha256, do nothing.
#   2. Try the upstream release archive, extract the library, check the sha256.
#   3. If that fails for any reason, say so on stderr and try the mirror, which
#      serves the extracted library under its destination file name.
#   4. If no source yields the pinned bytes, exit 1. Nothing is installed.
#
# The pinned hashes are the libraries every MuninnDB release up to v0.12.1
# embedded. A different file is never installed, whichever source it came
# from. To move to a new ONNX Runtime version, change the version and the
# hash together, and publish the new files to the mirror first.
#
# Environment:
#   ASSETS_DIR         destination directory (default internal/plugin/embed/assets)
#   ORT_UPSTREAM_BASE  upstream release download base
#   ORT_MIRROR_BASE    mirror base, set to an empty string to disable the mirror
#   ORT_SKIP_UPSTREAM  set to 1 to go straight to the mirror
#
# DOES NOT CATCH: a mirror and an upstream that are both unreachable (the build
# fails loudly, it cannot be made to succeed), or a pinned hash that was wrong
# when it was written. The hashes below were read from files that were in use,
# not from an upstream manifest.

set -euo pipefail

ASSETS_DIR="${ASSETS_DIR:-internal/plugin/embed/assets}"
ORT_UPSTREAM_BASE="${ORT_UPSTREAM_BASE:-https://github.com/microsoft/onnxruntime/releases/download}"
ORT_MIRROR_BASE="${ORT_MIRROR_BASE-https://github.com/scrypster/muninndb/releases/download/ort-libs-1}"
ORT_SKIP_UPSTREAM="${ORT_SKIP_UPSTREAM:-0}"

target="${1:-}"

# target -> version, upstream archive, name pattern inside it, destination, sha256
case "$target" in
  darwin-arm64)
    version="1.24.2"
    archive="onnxruntime-osx-arm64-${version}.tgz"
    member="libonnxruntime*.dylib"
    dest="libonnxruntime_darwin_arm64.dylib"
    want="87df6f94dd559ea958748adc80fd4c46d91c52bc025771f513291d155539590a"
    ;;
  darwin-amd64)
    # Upstream dropped Intel Mac builds after 1.23.2. This pin is intentional.
    version="1.23.2"
    archive="onnxruntime-osx-x86_64-${version}.tgz"
    member="libonnxruntime*.dylib"
    dest="libonnxruntime_darwin_amd64.dylib"
    want="8c9c78de65ea3786f987c0d980e9c1b13a3a5fbc6b3e2965ba05b450e6e4c054"
    ;;
  linux-amd64)
    version="1.24.2"
    archive="onnxruntime-linux-x64-${version}.tgz"
    member="libonnxruntime.so*"
    dest="libonnxruntime_linux_amd64.so"
    want="ffc84d48e845cf0b562ba4ea5ca32aaafc0d4069019fef4f63095b307d0270ad"
    ;;
  linux-arm64)
    version="1.24.2"
    archive="onnxruntime-linux-aarch64-${version}.tgz"
    member="libonnxruntime.so*"
    dest="libonnxruntime_linux_arm64.so"
    want="e18fe095919d8613ead3a31ff78212bde4fad929418a9b48f49d61c829ed5c82"
    ;;
  windows-amd64)
    version="1.24.2"
    archive="onnxruntime-win-x64-${version}.zip"
    member="onnxruntime.dll"
    dest="onnxruntime_windows_amd64.dll"
    want="114947d633e6844ce3c4b51ef6678f776628571d08a5763859c61642c8dcca9c"
    ;;
  *)
    echo "usage: $0 <darwin-arm64|darwin-amd64|linux-amd64|linux-arm64|windows-amd64>" >&2
    exit 2
    ;;
esac

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    echo "fetch-ort-lib: need sha256sum or shasum to verify downloads" >&2
    exit 1
  fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$ASSETS_DIR"
out="$ASSETS_DIR/$dest"

# install_if_pinned <candidate> <source label>: install and return 0 only when
# the candidate has the pinned hash. A mismatch is reported and returns 1.
install_if_pinned() {
  local candidate="$1" label="$2" got
  got="$(sha256_of "$candidate")"
  if [[ "$got" != "$want" ]]; then
    echo "fetch-ort-lib: WARN $label gave a file with sha256 $got, pinned is $want. Not installing it." >&2
    return 1
  fi
  cp "$candidate" "$out.tmp"
  mv "$out.tmp" "$out"
  echo "    $target: ONNX Runtime $version from $label, sha256 verified ($(du -sh "$out" | cut -f1))"
}

if [[ -f "$out" ]] && [[ "$(sha256_of "$out")" == "$want" ]]; then
  echo "    $target: already present, sha256 verified"
  exit 0
fi

try_upstream() {
  local url="$ORT_UPSTREAM_BASE/v$version/$archive" found
  if ! curl -fsSL --retry 2 "$url" -o "$work/$archive"; then
    echo "fetch-ort-lib: WARN upstream download failed: $url" >&2
    return 1
  fi
  mkdir -p "$work/x"
  case "$archive" in
    *.zip) unzip -q "$work/$archive" -d "$work/x" ;;
    *)     tar -xzf "$work/$archive" -C "$work/x" ;;
  esac
  # -type f skips the unversioned symlink that sits beside the real library.
  found="$(find "$work/x" -type f -name "$member" | sort | head -1)"
  if [[ -z "$found" ]]; then
    echo "fetch-ort-lib: WARN upstream archive holds no file named $member" >&2
    return 1
  fi
  install_if_pinned "$found" "upstream"
}

try_mirror() {
  if [[ -z "$ORT_MIRROR_BASE" ]]; then
    echo "fetch-ort-lib: WARN no mirror configured (ORT_MIRROR_BASE is empty)" >&2
    return 1
  fi
  local url="$ORT_MIRROR_BASE/$dest"
  if ! curl -fsSL --retry 2 "$url" -o "$work/$dest"; then
    echo "fetch-ort-lib: WARN mirror download failed: $url" >&2
    return 1
  fi
  install_if_pinned "$work/$dest" "mirror"
}

if [[ "$ORT_SKIP_UPSTREAM" != "1" ]] && try_upstream; then
  exit 0
fi
if try_mirror; then
  exit 0
fi

echo "fetch-ort-lib: ERROR no source produced $dest with the pinned sha256. Nothing was installed." >&2
exit 1
