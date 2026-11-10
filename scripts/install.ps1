# agentsview installer for Windows
# Usage: powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/kenn-io/agentsview/main/scripts/install.ps1 | iex"

$ErrorActionPreference = 'Stop'

$repo = 'kenn-io/agentsview'
$binaryName = 'agentsview.exe'

function Write-Info($msg) { Write-Host $msg -ForegroundColor Green }
function Write-Warn($msg) { Write-Host $msg -ForegroundColor Yellow }
function Write-Err($msg) { Write-Host $msg -ForegroundColor Red }

function Test-EnvBool($name) {
    $val = [Environment]::GetEnvironmentVariable($name)
    return ($val -match '^(1|true|yes)$')
}

function Get-Architecture {
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
        return 'arm64'
    }

    try {
        $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
        switch ($arch.ToString()) {
            'X64' { return 'amd64' }
            'X86' { return '386' }
            'Arm64' { return 'arm64' }
            default { return 'amd64' }
        }
    } catch {
        if ([System.Environment]::Is64BitOperatingSystem) {
            return 'amd64'
        } else {
            return '386'
        }
    }
}

function Invoke-WebRequestCompat {
    param([string]$Uri, [string]$OutFile)

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{ Uri = $Uri }
    if ($OutFile) { $params.OutFile = $OutFile }

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    if ($OutFile) {
        Invoke-WebRequest @params
    } else {
        Invoke-RestMethod @params
    }
}

function Get-FinalUrl {
    # Returns the URL that ultimately responded to a request, after any
    # redirects were followed. The property differs by PowerShell edition:
    # Windows PowerShell 5.x exposes HttpWebResponse.ResponseUri, while
    # PowerShell 7+ exposes HttpResponseMessage.RequestMessage.RequestUri.
    param($Response)
    try {
        if ($Response.BaseResponse.ResponseUri) {
            return $Response.BaseResponse.ResponseUri.AbsoluteUri
        }
    } catch {}
    try {
        if ($Response.BaseResponse.RequestMessage.RequestUri) {
            return $Response.BaseResponse.RequestMessage.RequestUri.AbsoluteUri
        }
    } catch {}
    return $null
}

function Get-LatestVersion {
    # Resolve the latest release tag by following the /releases/latest
    # 302 redirect to /releases/tag/<version> and reading the final URL.
    # Using the HTML endpoint (not api.github.com) avoids the 60 req/hr
    # per-IP rate limit, so users behind shared NAT / VPN don't get 403.
    #
    # We let Invoke-WebRequest follow the redirect rather than inspecting
    # the Location header with MaximumRedirection=0: in Windows PowerShell
    # 5.x that throws a System.InvalidOperationException (not a WebException
    # with a usable .Response), which broke the install. A HEAD request
    # follows the redirect without downloading the release page body.
    $url = "https://github.com/$repo/releases/latest"

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{
        Uri = $url
        Method = 'Head'
        ErrorAction = 'Stop'
    }
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    $finalUrl = $null
    try {
        $response = Invoke-WebRequest @params
        $finalUrl = Get-FinalUrl $response
    } catch {
        throw "Failed to fetch latest version: $_"
    }

    if (-not $finalUrl) {
        throw "Failed to fetch latest version: could not resolve release URL from $url"
    }
    if ($finalUrl -notmatch '/releases/tag/([^/]+)/?$') {
        throw "Failed to fetch latest version: unexpected release URL $finalUrl"
    }
    return $Matches[1]
}

function Test-ReleaseAsset {
    # Returns $true when the given release asset URL exists (HTTP 2xx),
    # following the redirect to the storage backend. Used to detect
    # whether a native build is published for the detected architecture.
    param([string]$Url)

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{
        Uri = $Url
        Method = 'Head'
        ErrorAction = 'Stop'
    }
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    try {
        $response = Invoke-WebRequest @params
        return ($response.StatusCode -ge 200 -and $response.StatusCode -lt 300)
    } catch {
        return $false
    }
}

function Resolve-ReleaseArch {
    # Returns the release arch to install for the detected CPU arch.
    # Prefers a native build, but falls back to amd64 on arm64 because
    # Windows on ARM transparently runs x64 binaries under emulation and
    # no native windows/arm64 asset is published for every release.
    # Returns $null when no usable asset exists.
    param([string]$DetectedArch, [string]$Version)

    $candidates = @($DetectedArch)
    if ($DetectedArch -eq 'arm64') {
        $candidates += 'amd64'
    }

    $versionNum = $Version.TrimStart('v')
    foreach ($candidate in $candidates) {
        $name = "agentsview_${versionNum}_windows_${candidate}.zip"
        $url = "https://github.com/$repo/releases/download/$Version/$name"
        if (Test-ReleaseAsset $url) {
            return $candidate
        }
    }
    return $null
}

function Get-InstallDir {
    if ($env:AGENTSVIEW_INSTALL_DIR) {
        return $env:AGENTSVIEW_INSTALL_DIR
    }
    return Join-Path $env:USERPROFILE '.agentsview\bin'
}

function Add-ToPath($dir) {
    $currentPath = [Environment]::GetEnvironmentVariable('Path', 'User')

    $normalizedDir = $dir.TrimEnd('\', '/')
    $alreadyInPath = $currentPath -split ';' | Where-Object {
        $_.TrimEnd('\', '/') -ieq $normalizedDir
    }
    if ($alreadyInPath) {
        Write-Info "Directory already in PATH"
        return $false
    }

    $newPath = "$currentPath;$dir"
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$dir"

    return $true
}

function Install-Agentsview {
    Write-Info "Installing agentsview..."
    Write-Host ""

    $arch = Get-Architecture
    Write-Info "Platform: windows/$arch"

    if ($arch -eq '386') {
        Write-Err "Error: 32-bit Windows is not supported."
        Write-Err "agentsview requires 64-bit Windows (amd64 or arm64)."
        exit 1
    }

    $version = Get-LatestVersion
    Write-Info "Latest version: $version"

    $resolvedArch = Resolve-ReleaseArch -DetectedArch $arch -Version $version
    if (-not $resolvedArch) {
        Write-Err "Error: No Windows release asset found for $version (detected windows/$arch)."
        Write-Err "See https://github.com/$repo for build-from-source instructions."
        exit 1
    }
    if ($resolvedArch -ne $arch) {
        Write-Warn "No native windows/$arch build for $version; installing windows/$resolvedArch (runs under emulation)."
        $arch = $resolvedArch
    }

    $versionNum = $version.TrimStart('v')
    $archiveName = "agentsview_${versionNum}_windows_${arch}.zip"
    $downloadUrl = "https://github.com/$repo/releases/download/$version/$archiveName"

    $installDir = Get-InstallDir
    Write-Info "Install directory: $installDir"
    Write-Host ""

    if (-not (Test-Path $installDir)) {
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    }

    $tmpDir = Join-Path $env:TEMP "agentsview-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $archivePath = Join-Path $tmpDir $archiveName

        Write-Info "Downloading $archiveName..."
        Invoke-WebRequestCompat -Uri $downloadUrl -OutFile $archivePath

        $checksumUrl = "https://github.com/$repo/releases/download/$version/SHA256SUMS"
        $checksumFile = Join-Path $tmpDir "SHA256SUMS"

        if (Test-EnvBool 'AGENTSVIEW_SKIP_CHECKSUM') {
            Write-Warn "Warning: Skipping checksum verification (AGENTSVIEW_SKIP_CHECKSUM is set)"
        } else {
            Write-Info "Verifying checksum..."
            try {
                Invoke-WebRequestCompat -Uri $checksumUrl -OutFile $checksumFile
            } catch {
                Write-Err "Error: Could not download checksums file: $_"
                Write-Err "Set AGENTSVIEW_SKIP_CHECKSUM=1 to bypass verification (not recommended)"
                exit 1
            }

            $matchingLines = @()
            foreach ($line in Get-Content $checksumFile) {
                if ($line -match '^\s*$') { continue }
                $parts = $line -split '\s+', 2
                if ($parts.Count -lt 2) { continue }
                $hash = $parts[0]
                $filename = $parts[1]
                $filename = $filename -replace '^[\*]', ''
                $filename = $filename -replace '^\.\/', ''
                $filename = $filename -replace '^\.\\', ''
                if ($filename -eq $archiveName) {
                    $matchingLines += $hash
                }
            }

            if ($matchingLines.Count -eq 0) {
                Write-Err "Error: Could not find checksum for $archiveName in SHA256SUMS"
                Write-Err "Set AGENTSVIEW_SKIP_CHECKSUM=1 to bypass verification (not recommended)"
                exit 1
            }

            if ($matchingLines.Count -gt 1) {
                Write-Err "Error: Multiple checksum entries found for $archiveName"
                exit 1
            }

            $expectedHash = $matchingLines[0]
            $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()

            if ($actualHash -ne $expectedHash.ToLower()) {
                Write-Err "Error: Checksum verification failed!"
                Write-Err "Expected: $expectedHash"
                Write-Err "Got:      $actualHash"
                exit 1
            }
            Write-Info "Checksum verified."
        }

        Write-Info "Extracting..."
        if ($PSVersionTable.PSVersion.Major -lt 5) {
            Write-Err "Error: PowerShell 5.0 or later is required for Expand-Archive."
            Write-Err "Please upgrade PowerShell or download the release manually from GitHub."
            exit 1
        }
        try {
            Expand-Archive -Path $archivePath -DestinationPath $tmpDir -Force
        } catch {
            Write-Err "Error: Failed to extract archive: $_"
            exit 1
        }

        $binaryFile = Get-ChildItem -Path $tmpDir -Recurse -Filter $binaryName | Select-Object -First 1
        if (-not $binaryFile) {
            Write-Err "Error: Could not find $binaryName in extracted archive"
            exit 1
        }

        $destPath = Join-Path $installDir $binaryName

        if (Test-Path $destPath) {
            Remove-Item $destPath -Force
        }

        Move-Item $binaryFile.FullName $destPath -Force

        Write-Host ""
        Write-Info "Installation complete!"
        Write-Host ""

        if (-not (Test-EnvBool 'AGENTSVIEW_NO_MODIFY_PATH')) {
            $pathUpdated = Add-ToPath $installDir
            if ($pathUpdated) {
                Write-Info "Added $installDir to PATH"
                Write-Warn "Restart your terminal for PATH changes to take effect."
                Write-Host ""
            }
        }

        Write-Host "Get started:"
        Write-Host "  agentsview serve    # Start the server and open browser"
        Write-Host "  agentsview update   # Check for and install updates"
        Write-Host "  To enable search by meaning, choose Settings -> Embeddings -> Turn on."

    } finally {
        if (Test-Path $tmpDir) {
            Remove-Item $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

Install-Agentsview
