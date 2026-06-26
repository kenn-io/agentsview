#!/usr/bin/env bash
set -euo pipefail

# Build and run the dockerized screenshot pipeline.
#
# Usage:
#   ./screenshots/run.sh
#
# Environment:
#   AGENTSVIEW_SRC   Path to agentsview source (default: ~/code/agentsview)
#   SOURCE_DB        Path to real sessions database (default: ~/.agentsview/sessions.db)

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGENTSVIEW_SRC="${AGENTSVIEW_SRC:-$HOME/code/agentsview}"
SOURCE_DB="${SOURCE_DB:-$HOME/.agentsview/sessions.db}"
OUTPUT_DIR="$ROOT/assets/generated/screenshots"
IMAGE_NAME="agentsview-screenshots"

# Verify prerequisites
if ! command -v docker &> /dev/null; then
  echo "Error: docker is required"
  exit 1
fi

if ! command -v sqlite3 &> /dev/null; then
  echo "Error: sqlite3 is required to prepare the screenshot database"
  exit 1
fi

if [ ! -d "$AGENTSVIEW_SRC" ]; then
  echo "Error: agentsview source not found at $AGENTSVIEW_SRC"
  echo "Set AGENTSVIEW_SRC to the correct path"
  exit 1
fi

if [ ! -f "$SOURCE_DB" ]; then
  echo "Error: sessions database not found at $SOURCE_DB"
  echo "Set SOURCE_DB to the correct path"
  exit 1
fi

echo "=== agentsview screenshot pipeline ==="
echo "Source code: $AGENTSVIEW_SRC"
echo "Source DB:   $SOURCE_DB"
echo "Output:      $OUTPUT_DIR"
echo ""

# Assemble Docker build context in temp directory
CONTEXT=$(mktemp -d)
trap 'rm -rf "$CONTEXT"' EXIT

echo "Assembling build context..."

# Resolve version info from git before copying (we exclude .git).
# Sanitize to alphanumeric + ._+- to prevent Make injection.
AV_VERSION=$(cd "$AGENTSVIEW_SRC" && git describe --tags --always --dirty 2>/dev/null || echo "dev")
AV_VERSION=$(printf '%s' "$AV_VERSION" | tr -cd 'A-Za-z0-9._+-')
AV_COMMIT=$(cd "$AGENTSVIEW_SRC" && git rev-parse --short HEAD 2>/dev/null || echo "unknown")
AV_COMMIT=$(printf '%s' "$AV_COMMIT" | tr -cd 'A-Za-z0-9._+-')

# Copy agentsview source (exclude heavy/unnecessary dirs)
rsync -a \
  --exclude='node_modules' \
  --exclude='.cache' \
  --exclude='.git' \
  --exclude='/agentsview' \
  --exclude='desktop/src-tauri/target' \
  "$AGENTSVIEW_SRC/" "$CONTEXT/agentsview/"

# Extract the screenshot-safe database before Docker sees the
# build context. The production session DB is large; copying it
# into the context would force Docker to transfer gigabytes before
# the image build can shrink it.
bash "$ROOT/screenshots/extract-db.sh" \
  "$SOURCE_DB" "$CONTEXT/test-sessions.db"

# Copy screenshot pipeline files
cp -r "$ROOT/screenshots/" "$CONTEXT/screenshots/"
cp "$ROOT/screenshots/Dockerfile" "$CONTEXT/Dockerfile"

echo "Build context: $(du -sh "$CONTEXT" | cut -f1)"
echo ""

# Build Docker image
echo "Building Docker image (this may take a few minutes on first run)..."
docker build \
  --build-arg AV_VERSION="$AV_VERSION" \
  --build-arg AV_COMMIT="$AV_COMMIT" \
  -t "$IMAGE_NAME" "$CONTEXT"

echo ""

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Run screenshots (forward extra args to Playwright, e.g. --grep "test name")
echo "Running screenshot capture..."
docker run --rm \
  -v "$OUTPUT_DIR:/output" \
  "$IMAGE_NAME" "$@"

echo ""
echo "=== Done ==="
echo "Screenshots saved to $OUTPUT_DIR/"
ls -la "$OUTPUT_DIR/"*.png 2>/dev/null || echo "(no screenshots found)"
