#!/bin/bash
# Tests for nilaway-changed-packages.sh package selection.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

BIN_DIR="$TMP_DIR/bin"
mkdir -p "$BIN_DIR"

cat > "$BIN_DIR/go" <<'GOEOF'
#!/bin/bash
set -euo pipefail
if [ "$1" != "list" ]; then
    echo "unexpected go command: $*" >&2
    exit 1
fi
shift
if [ "$1" != "-f" ]; then
    echo "unexpected go list flags: $*" >&2
    exit 1
fi
shift 2
for pkg in "$@"; do
    case "$pkg" in
        ./internal/parser|./internal/db|./cmd/agentsview|.)
            printf '%s\n' "$pkg"
            ;;
        *)
            echo "unexpected package: $pkg" >&2
            exit 1
            ;;
    esac
done
GOEOF
chmod +x "$BIN_DIR/go"

cat > "$BIN_DIR/nilaway" <<'NILEOF'
#!/bin/bash
printf '%s\n' "$*" > "$NILAWAY_ARGS_FILE"
NILEOF
chmod +x "$BIN_DIR/nilaway"

export PATH="$BIN_DIR:$PATH"
export NILAWAY_ARGS_FILE="$TMP_DIR/nilaway-args"

WORK_DIR="$TMP_DIR/work"
mkdir -p "$WORK_DIR/internal/parser" "$WORK_DIR/internal/db"
touch "$WORK_DIR/internal/parser/parser.go"
touch "$WORK_DIR/internal/parser/parser_test.go"
touch "$WORK_DIR/internal/db/db.go"
touch "$WORK_DIR/README.md"

cd "$WORK_DIR"

"$SCRIPT_DIR/nilaway-changed-packages.sh" \
    internal/parser/parser.go \
    internal/parser/parser_test.go \
    internal/db/db.go \
    README.md \
    deleted/file.go

expected='-test=false -include-pkgs=github.com/wesm/agentsview ./internal/db ./internal/parser'
actual="$(cat "$NILAWAY_ARGS_FILE")"
if [ "$actual" != "$expected" ]; then
    echo "unexpected nilaway args" >&2
    echo "expected: $expected" >&2
    echo "actual:   $actual" >&2
    exit 1
fi

rm -f "$NILAWAY_ARGS_FILE"
"$SCRIPT_DIR/nilaway-changed-packages.sh" README.md
if [ -e "$NILAWAY_ARGS_FILE" ]; then
    echo "nilaway should not run without changed Go files" >&2
    exit 1
fi
