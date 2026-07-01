#!/bin/bash
# Tests for changelog.sh prompt behavior.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHANGELOG_SCRIPT="$SCRIPT_DIR/changelog.sh"

PASS=0
FAIL=0

assert_success() {
    local desc="$1"
    shift
    if "$@"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        FAIL=$((FAIL + 1))
    fi
}

test_prompt_skips_documentation_only_changes() {
    local tmp fakebin output output_file
    tmp="$(mktemp -d)"
    fakebin="$tmp/bin"
    output_file="$tmp/output"
    mkdir -p "$fakebin"

    cat >"$fakebin/claude" <<'EOF'
#!/bin/bash
set -euo pipefail
prompt="$(cat)"
case "$prompt" in
    *"Do NOT mention documentation-only changes"*)
        printf 'Bug fixes\n\n- Fix PostgreSQL push sync ownership conflict handling.\n'
        ;;
    *)
        printf 'missing documentation-only skip instruction\n' >&2
        exit 42
        ;;
esac
EOF
    chmod +x "$fakebin/claude"

    (
        cd "$tmp"
        git init -q
        git config user.name "Test User"
        git config user.email "test@example.com"
        printf 'initial\n' > README.md
        git add README.md
        git commit -q -m "chore: initial"
        git tag v0.1.0

        printf 'usage docs\n' > docs.md
        git add docs.md
        git commit -q -m "docs: update usage docs"

        printf 'fix\n' > postgres.txt
        git add postgres.txt
        git commit -q -m "fix(postgres): unblock alias backfill marker"

        PATH="$fakebin:$PATH" CHANGELOG_AGENT=claude "$CHANGELOG_SCRIPT" 0.1.1 v0.1.0
    ) >"$output_file" 2>&1 || {
        cat "$output_file"
        rm -rf "$tmp"
        return 1
    }

    output="$(cat "$output_file")"
    rm -rf "$tmp"

    case "$output" in
        *"Fix PostgreSQL push sync ownership conflict handling."*) return 0 ;;
        *)
            printf 'unexpected changelog output:\n%s\n' "$output"
            return 1
            ;;
    esac
}

echo "=== changelog generation ==="

assert_success \
    "prompt tells agents to skip documentation-only changes" \
    test_prompt_skips_documentation_only_changes

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
