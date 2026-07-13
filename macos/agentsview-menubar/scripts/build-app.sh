#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

swift build -c release --product AgentsViewMenuBar
BIN_DIR="$(swift build -c release --show-bin-path)"

APP_DIR="$ROOT_DIR/dist/AgentsViewMenuBar.app"
rm -rf "$APP_DIR"
mkdir -p "$APP_DIR/Contents/MacOS"
cp "$BIN_DIR/AgentsViewMenuBar" "$APP_DIR/Contents/MacOS/AgentsViewMenuBar"
cp "$ROOT_DIR/Resources/Info.plist" "$APP_DIR/Contents/Info.plist"

# Re-sign after assembling the bundle so the copied Info.plist is covered.
codesign --force --sign - "$APP_DIR" >/dev/null

echo "Built $APP_DIR"
