#!/usr/bin/env bash
# check-build-tags.sh — fail if any muninn binary build is missing -tags localassets.
#
# The bundled local ONNX embedder requires this build tag to activate. Without
# it, LocalAvailable() always returns false and the embedder silently falls
# through to noop, breaking out-of-the-box semantic search.
#
# Files scanned: Dockerfile, Makefile, .github/workflows/*.yml,
#                cmd/muninn/integration_test.go

set -euo pipefail

FAILED=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Patterns that indicate a muninn binary build command.
# We match on lines that contain a go build invocation targeting the muninn
# binary (by output name or by package path) but NOT containing localassets.
check_file() {
    local file="$1"
    local relfile="${file#"${ROOT}/"}"

    # Extract lines that look like go build commands for the muninn binary.
    # A line qualifies if it contains "go build" and one of:
    #   - targets ./cmd/muninn
    #   - produces muninndb-server, muninn.exe, or muninn-<os>-<arch>
    while IFS= read -r line; do
        # Skip blank lines and comment lines.
        [[ -z "$line" || "$line" =~ ^[[:space:]]*# ]] && continue

        # Must mention go build.
        echo "$line" | grep -q 'go build' || continue

        # Must be targeting the muninn binary (by package or output name).
        echo "$line" | grep -qE 'cmd/muninn|muninndb-server|muninn\.exe|muninn-[a-z]' || continue

        # If it already has localassets, it is fine.
        echo "$line" | grep -q 'localassets' && continue

        echo "ERROR: go build without -tags localassets in ${relfile}:"
        echo "  ${line}"
        FAILED=1
    done < "$file"
}

FILES=(
    "${ROOT}/Dockerfile"
    "${ROOT}/Makefile"
    "${ROOT}/cmd/muninn/integration_test.go"
)

# Add all workflow YAML files.
while IFS= read -r f; do
    FILES+=("$f")
done < <(find "${ROOT}/.github/workflows" -name '*.yml' 2>/dev/null)

for f in "${FILES[@]}"; do
    [[ -f "$f" ]] && check_file "$f"
done

if [[ "$FAILED" -ne 0 ]]; then
    echo ""
    echo "Fix: add '-tags localassets' to the flagged go build command(s)."
    echo "See local_assets_noembed.go — without this tag LocalAvailable() always returns false."
    exit 1
fi

echo "OK: all muninn binary build commands include -tags localassets."
