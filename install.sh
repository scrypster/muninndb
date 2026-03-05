#!/bin/sh
# muninn installer — https://muninndb.com
# Usage: curl -fsSL https://muninndb.com/install.sh | sh
set -e

REPO="scrypster/muninndb"
BIN_NAME="muninn"
INSTALL_DIR="/usr/local/bin"

# ── Detect platform ─────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "${ARCH}" in
  x86_64)          ARCH="amd64" ;;
  arm64|aarch64)   ARCH="arm64" ;;
  *)
    echo "muninn: unsupported architecture: ${ARCH}" >&2
    exit 1
    ;;
esac

case "${OS}" in
  darwin|linux) ;;
  *)
    echo "muninn: unsupported OS: ${OS}" >&2
    echo "  Download manually: https://github.com/${REPO}/releases/latest" >&2
    exit 1
    ;;
esac

PLATFORM="${OS}-${ARCH}"

# ── Resolve latest release tag ───────────────────────────────────────────────
echo "  Checking latest release..."
TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep '"tag_name"' | head -1 | cut -d'"' -f4)

if [ -z "${TAG}" ]; then
  echo "muninn: could not determine latest version (GitHub API rate limit?)" >&2
  echo "  Try again in a minute, or download from: https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi

# ── Download binary ──────────────────────────────────────────────────────────
URL="https://github.com/${REPO}/releases/download/${TAG}/${BIN_NAME}-${PLATFORM}"
TMP=$(mktemp)

echo "  Downloading muninn ${TAG} for ${OS}/${ARCH}..."
HTTP_CODE=$(curl -sSL --progress-bar -w "%{http_code}" -o "${TMP}" "${URL}")
if [ "${HTTP_CODE}" != "200" ]; then
  rm -f "${TMP}"
  echo "" >&2
  echo "muninn: download failed (HTTP ${HTTP_CODE})" >&2
  echo "  URL: ${URL}" >&2
  echo "" >&2
  echo "  This may mean the release asset for ${PLATFORM} is not yet available." >&2
  echo "  Download manually: https://github.com/${REPO}/releases/tag/${TAG}" >&2
  exit 1
fi
chmod +x "${TMP}"

# ── Install ──────────────────────────────────────────────────────────────────
# Try /usr/local/bin first; fall back to ~/.local/bin if we lack write permission.
if [ -w "${INSTALL_DIR}" ]; then
  mv "${TMP}" "${INSTALL_DIR}/${BIN_NAME}"
else
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "${INSTALL_DIR}"
  mv "${TMP}" "${INSTALL_DIR}/${BIN_NAME}"

  # Warn if this directory is not on PATH.
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
      echo ""
      echo "  ⚠  ${INSTALL_DIR} is not in your PATH."
      echo "     Add this to your shell profile (~/.zshrc or ~/.bashrc):"
      echo ""
      echo "       export PATH=\"\$HOME/.local/bin:\$PATH\""
      echo ""
      ;;
  esac
fi

# ── Done ─────────────────────────────────────────────────────────────────────
echo ""
echo "  muninn ${TAG} installed to ${INSTALL_DIR}/${BIN_NAME}"
echo ""
echo "  Get started:"
echo "    muninn init"
echo ""
