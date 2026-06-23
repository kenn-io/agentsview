<#
.SYNOPSIS
    Build and launch the AgentsView Tauri desktop app in dev mode on Windows.

.DESCRIPTION
    Builds the frontend SPA, compiles the Go sidecar binary, copies it into
    the Tauri binaries directory, and runs `cargo tauri dev`.

.PARAMETER SkipBuild
    Skip the frontend and Go sidecar build steps. Use this when iterating
    on Rust/Tauri code only and the sidecar binary is already up to date.
#>
param(
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"

function ConvertTo-Semver {
    param([string]$Raw)
    # Strip leading 'v'
    $Raw = $Raw -replace '^v', ''
    # git-describe: 0.10.0-3-gabcdef -> 0.10.0-dev.3
    if ($Raw -match '^(\d+\.\d+\.\d+)-(\d+)-g[0-9a-f]+(-dirty)?$') {
        return "$($Matches[1])-dev.$($Matches[2])"
    }
    # Already semver, with optional prerelease (strip -dirty)
    if ($Raw -match '^\d+\.\d+\.\d+(-[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)*)?(-dirty)?$') {
        return ($Raw -replace '-dirty$', '')
    }
    # Looks like a version but isn't valid semver
    if ($Raw -match '^\d+\.') {
        Write-Error "Malformed version tag: $Raw"
        exit 1
    }
    # Non-tag fallback (bare hash, "dev", etc.)
    return ""
}

function Update-TauriVersion {
    param([string]$Version, [string]$ConfPath)
    $semver = ConvertTo-Semver $Version
    if (-not $semver) {
        Write-Host "Skipping tauri.conf.json version patch (non-tag build: $Version)"
        return
    }
    $origPath = "$ConfPath.orig"
    if (-not (Test-Path $origPath)) {
        Copy-Item $ConfPath $origPath
    }
    $content = Get-Content $ConfPath -Raw
    $content = $content -replace '"version":\s*"[^"]*"', "`"version`": `"$semver`""
    Set-Content -Path $ConfPath -Value $content -NoNewline
    Write-Host "Patched tauri.conf.json version to $semver" -ForegroundColor Green
}

function Restore-TauriVersion {
    param([string]$ConfPath)
    $origPath = "$ConfPath.orig"
    if (Test-Path $origPath) {
        Move-Item -Force $origPath $ConfPath
    }
}

# Ensure fnm-managed Node.js is on PATH. fnm requires an `fnm env`
# eval in each new PowerShell session; without it, node/npm are not
# found even though fnm itself is on PATH via WinGet links.
if ((Get-Command fnm -ErrorAction SilentlyContinue) -and -not (Get-Command node -ErrorAction SilentlyContinue)) {
    Write-Host "Activating fnm environment..." -ForegroundColor Yellow
    fnm env --use-on-cd --shell powershell | Out-String | Invoke-Expression
}

$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if (-not (Test-Path "$RepoRoot\go.mod")) {
    # Script may live at repo/scripts/ directly
    $RepoRoot = Split-Path -Parent $PSScriptRoot
}
if (-not (Test-Path "$RepoRoot\go.mod")) {
    Write-Error "Could not locate repository root (no go.mod found)"
    exit 1
}

$DesktopDir = Join-Path $RepoRoot "desktop"
$FrontendDir = Join-Path $RepoRoot "frontend"
$EmbedDir = Join-Path $RepoRoot "internal\web\dist"
$BinDir = Join-Path $DesktopDir "src-tauri\binaries"
# Detect Rust host triple for the sidecar binary name
$Triple = (rustc -vV | Select-String "^host:").ToString().Split(" ", 2)[1].Trim()
if (-not $Triple) {
    Write-Error "Could not detect Rust host triple. Is rustc installed?"
    exit 1
}
$SidecarBin = Join-Path $BinDir "agentsview-$Triple.exe"

if (-not $SkipBuild) {
    # --- Build frontend ---
    Write-Host "Building frontend..." -ForegroundColor Cyan
    Push-Location $FrontendDir
    try {
        npm ci
        npm run build
    } finally {
        Pop-Location
    }

    # Copy frontend dist into Go embed directory
    if (Test-Path $EmbedDir) {
        Remove-Item -Recurse -Force $EmbedDir
    }
    Copy-Item -Recurse (Join-Path $FrontendDir "dist") $EmbedDir

    # --- Build Go sidecar ---
    Write-Host "Building Go sidecar..." -ForegroundColor Cyan

    $version = git -C $RepoRoot describe --tags --always --dirty 2>$null
    if (-not $version) { $version = "dev" }
    $commit = git -C $RepoRoot rev-parse --short HEAD 2>$null
    if (-not $commit) { $commit = "unknown" }
    $buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    $ldflags = "-X main.version=$version -X main.commit=$commit -X main.buildDate=$buildDate"

    $env:CGO_ENABLED = "1"
    Push-Location $RepoRoot
    try {
        go run ./internal/pricing/cmd/litellm-snapshot -restore
        if ($LASTEXITCODE -ne 0) {
            throw "pricing snapshot restore failed with exit code $LASTEXITCODE"
        }
        go build -tags fts5 -ldflags $ldflags -o agentsview.exe ./cmd/agentsview
    } finally {
        Pop-Location
    }

    # Copy sidecar binary
    if (-not (Test-Path $BinDir)) {
        New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
    }
    Copy-Item (Join-Path $RepoRoot "agentsview.exe") $SidecarBin -Force

    Write-Host "Sidecar ready: $SidecarBin" -ForegroundColor Green

    # Patch tauri.conf.json version to match the git-derived version
    $TauriConf = Join-Path $DesktopDir "src-tauri\tauri.conf.json"
    Update-TauriVersion -Version $version -ConfPath $TauriConf
} else {
    if (-not (Test-Path $SidecarBin)) {
        Write-Error "Sidecar binary not found at $SidecarBin. Run without -SkipBuild first."
        exit 1
    }
    Write-Host "Skipping build (using existing sidecar binary)" -ForegroundColor Yellow
}

# --- Launch Tauri dev ---
# Use `cargo run` directly instead of `npx tauri dev` to avoid Tauri's
# built-in dev server opening a browser tab (port 1430) that shows a
# stale "Preparing your workspace" page. The app manages its own
# webview navigation from the splash screen to the Go backend.
$TauriConf = Join-Path $DesktopDir "src-tauri\tauri.conf.json"
Write-Host "Launching Tauri dev..." -ForegroundColor Cyan
Push-Location (Join-Path $DesktopDir "src-tauri")
try {
    cargo run
} finally {
    Pop-Location
    Restore-TauriVersion -ConfPath $TauriConf
}
