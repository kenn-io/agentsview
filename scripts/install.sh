#!/bin/bash
# agentsview installer
# Usage: curl -fsSL https://raw.githubusercontent.com/kenn-io/agentsview/main/scripts/install.sh | bash

set -euo pipefail

REPO="kenn-io/agentsview"
BINARY_NAME="agentsview"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info() { echo -e "${GREEN}$1${NC}"; }
warn() { echo -e "${YELLOW}$1${NC}"; }
error() { echo -e "${RED}$1${NC}" >&2; exit 1; }

detect_os() {
    case "$(uname -s)" in
        Darwin) echo "darwin" ;;
        Linux) echo "linux" ;;
        *) error "Unsupported OS: $(uname -s). agentsview supports macOS and Linux." ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac
}

find_install_dir() {
    if [ -w "/usr/local/bin" ]; then
        echo "/usr/local/bin"
    else
        mkdir -p "$HOME/.local/bin"
        echo "$HOME/.local/bin"
    fi
}

download() {
    local url="$1"
    local output="$2"
    if command -v curl &>/dev/null; then
        curl -fsSL "$url" -o "$output"
    elif command -v wget &>/dev/null; then
        wget -q "$url" -O "$output"
    else
        error "Neither curl nor wget found"
    fi
}

get_latest_version() {
    # Use the HTML /releases/latest endpoint, which 302-redirects to
    # /releases/tag/<version>. Unlike api.github.com it is not rate-limited
    # at 60 req/hr per IP, so users behind shared NAT / VPN don't get 403.
    local url="https://github.com/${REPO}/releases/latest"
    local final_url=""
    if command -v curl &>/dev/null; then
        final_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url") || return 1
    elif command -v wget &>/dev/null; then
        final_url=$(wget --spider -S "$url" 2>&1 \
            | awk 'tolower($1)=="location:" {print $2}' \
            | tail -1 \
            | tr -d '\r\n') || return 1
    else
        return 1
    fi
    case "$final_url" in
        */releases/tag/*) echo "${final_url##*/releases/tag/}" ;;
        *) return 1 ;;
    esac
}

verify_checksum() {
    local file="$1"
    local checksums_file="$2"
    local filename="$3"

    if [ "${AGENTSVIEW_SKIP_CHECKSUM:-0}" = "1" ]; then
        warn "Checksum verification skipped (AGENTSVIEW_SKIP_CHECKSUM=1)"
        return 0
    fi

    if [ ! -f "$checksums_file" ]; then
        error "Checksum file not available. Set AGENTSVIEW_SKIP_CHECKSUM=1 to bypass."
    fi

    local expected
    expected=$(awk -v f="$filename" '{gsub(/^\*/, "", $2); if ($2==f) {print $1; exit}}' "$checksums_file")
    if [ -z "$expected" ]; then
        error "No checksum found for $filename in SHA256SUMS"
    fi

    local actual
    if command -v sha256sum &>/dev/null; then
        actual=$(sha256sum "$file" | cut -d' ' -f1)
    elif command -v shasum &>/dev/null; then
        actual=$(shasum -a 256 "$file" | cut -d' ' -f1)
    else
        error "No sha256 tool available. Install coreutils or set AGENTSVIEW_SKIP_CHECKSUM=1 to bypass."
    fi

    if [ "$expected" != "$actual" ]; then
        error "Checksum verification failed!\n  Expected: $expected\n  Actual:   $actual"
    fi

    info "Checksum verified"
}

install_from_release() {
    local os="$1"
    local arch="$2"
    local install_dir="$3"

    info "Fetching latest release..."
    local version
    version=$(get_latest_version)

    if [ -z "$version" ]; then
        return 1
    fi

    info "Found version: $version"

    local platform="${os}_${arch}"
    local filename="${BINARY_NAME}_${version#v}_${platform}.tar.gz"
    local base_url="https://github.com/${REPO}/releases/download/${version}"

    local tmpdir
    tmpdir=$(mktemp -d)
    trap "rm -rf $tmpdir" EXIT

    info "Downloading ${filename}..."
    if ! download "${base_url}/${filename}" "$tmpdir/release.tar.gz"; then
        return 1
    fi

    if [ "${AGENTSVIEW_SKIP_CHECKSUM:-0}" != "1" ]; then
        if ! download "${base_url}/SHA256SUMS" "$tmpdir/SHA256SUMS"; then
            error "Failed to download SHA256SUMS. Cannot verify binary integrity."
        fi
        verify_checksum "$tmpdir/release.tar.gz" "$tmpdir/SHA256SUMS" "$filename"
    else
        warn "Checksum verification skipped (AGENTSVIEW_SKIP_CHECKSUM=1)"
    fi

    info "Extracting..."
    tar -xzf "$tmpdir/release.tar.gz" -C "$tmpdir"

    if [ -f "$tmpdir/${BINARY_NAME}" ]; then
        if [ -w "$install_dir" ]; then
            mv "$tmpdir/${BINARY_NAME}" "$install_dir/"
        else
            sudo mv "$tmpdir/${BINARY_NAME}" "$install_dir/"
        fi
        chmod +x "$install_dir/${BINARY_NAME}"
    else
        error "Binary not found in archive"
    fi

    if [ "$os" = "darwin" ] && [ -f "$install_dir/${BINARY_NAME}" ]; then
        codesign -s - "$install_dir/${BINARY_NAME}" 2>/dev/null || true
    fi

    return 0
}

main() {
    info "Installing agentsview..."
    echo

    local os
    os=$(detect_os)
    local arch
    arch=$(detect_arch)
    local install_dir
    install_dir=$(find_install_dir)

    info "Platform: ${os}/${arch}"
    info "Install directory: ${install_dir}"
    echo

    if install_from_release "$os" "$arch" "$install_dir"; then
        info "Installed from GitHub release"
    else
        error "Installation failed. Please check https://github.com/${REPO}/releases for available builds."
    fi

    echo
    info "Installation complete!"
    echo

    if ! echo "$PATH" | grep -q "$install_dir"; then
        warn "Add this to your shell profile:"
        echo "  export PATH=\"\$PATH:$install_dir\""
        echo
    fi

    echo "Get started:"
    echo "  agentsview serve    # Start the server and open browser"
    echo "  agentsview update   # Check for and install updates"
    echo "  To enable search by meaning, choose Settings -> Embeddings -> Turn on."
}

# Guard: only run main when executed directly, not when sourced.
# ${BASH_SOURCE[0]-} defaults to empty when piped via stdin
# (curl ... | bash), which we treat as direct execution.
if [[ "${BASH_SOURCE[0]-}" == "${0}" || -z "${BASH_SOURCE[0]-}" ]]; then
    main "$@"
fi
