package agentsview_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDevBackendBuildRestoresPricingSnapshotBeforeBuild(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "scripts/dev-backend-build.sh", 0o755)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(t, root, stubs.env(), "bash", "scripts/dev-backend-build.sh")
	require.NoError(t, err, "%s", out)

	events := stubs.events(t)
	assertEventOrder(t, events,
		"go run ./internal/pricing/cmd/litellm-snapshot -restore",
		"go build -tags fts5",
	)
	require.FileExists(t, filepath.Join(root, "tmp", "agentsview"))
}

func TestDevBackendBuildStopsWhenPricingSnapshotRestoreFails(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "scripts/dev-backend-build.sh", 0o755)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("RESTORE_FAIL=1"),
		"bash",
		"scripts/dev-backend-build.sh",
	)
	require.Error(t, err, "script should fail when snapshot restore fails: %s", out)

	events := stubs.events(t)
	assertEventContains(t, events,
		"go run ./internal/pricing/cmd/litellm-snapshot -restore")
	assertNoEventContains(t, events, "go build")
}

func TestE2EServerRestoresPricingSnapshotBeforeServerBuild(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "scripts/e2e-server.sh", 0o755)
	writeE2EWorkspace(t, root)
	fixture := writeFixtureBinary(t, root)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("E2E_PREBUILT_FIXTURE="+fixture),
		"bash",
		"scripts/e2e-server.sh",
	)
	require.NoError(t, err, "%s", out)

	assertEventOrder(t, stubs.events(t),
		"fixture -out",
		"go run ./internal/pricing/cmd/litellm-snapshot -restore",
		"npm run build",
		"go build -tags fts5,kit_posthog_disabled",
		"built-binary serve --port 8090 --no-browser",
	)
}

func TestE2EServerRestoresPricingSnapshotBeforeFixtureBuild(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "scripts/e2e-server.sh", 0o755)
	writeE2EWorkspace(t, root)
	server := writeServerBinary(t, root)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("E2E_PREBUILT_SERVER="+server),
		"bash",
		"scripts/e2e-server.sh",
	)
	require.NoError(t, err, "%s", out)

	assertEventOrder(t, stubs.events(t),
		"go run ./internal/pricing/cmd/litellm-snapshot -restore",
		"go build -tags fts5,kit_posthog_disabled",
		"built-binary -out",
		"server serve --port 8090 --no-browser",
	)
}

func TestE2EServerStopsWhenPricingSnapshotRestoreFails(t *testing.T) {
	requireUnixShell(t)

	root := t.TempDir()
	installRepoFile(t, root, "scripts/e2e-server.sh", 0o755)
	writeE2EWorkspace(t, root)
	fixture := writeFixtureBinary(t, root)
	stubs := installUnixBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("E2E_PREBUILT_FIXTURE="+fixture, "RESTORE_FAIL=1"),
		"bash",
		"scripts/e2e-server.sh",
	)
	require.Error(t, err, "script should fail when snapshot restore fails: %s", out)

	events := stubs.events(t)
	assertEventContains(t, events,
		"go run ./internal/pricing/cmd/litellm-snapshot -restore")
	assertNoEventContains(t, events, "go build")
}

func TestDesktopDevPowerShellStopsWhenPricingSnapshotRestoreFails(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell native command failure handling is Windows-specific")
	}
	requireCommand(t, "pwsh")

	root := t.TempDir()
	installRepoFile(t, root, "scripts/desktop-dev.ps1", 0o755)
	writeDesktopDevWorkspace(t, root)
	stubs := installWindowsBuildStubs(t, root)

	out, err := runInWorkspace(
		t,
		root,
		stubs.env("RESTORE_FAIL=1"),
		"pwsh",
		"-NoProfile",
		"-ExecutionPolicy",
		"Bypass",
		"-File",
		filepath.Join(root, "scripts", "desktop-dev.ps1"),
	)
	require.Error(t, err, "script should fail when snapshot restore fails: %s", out)

	events := stubs.events(t)
	assertEventContains(t, events,
		"go run ./internal/pricing/cmd/litellm-snapshot -restore")
	assertNoEventContains(t, events, "go build")
}

type buildStubs struct {
	binDir  string
	logPath string
}

func installUnixBuildStubs(t *testing.T, root string) buildStubs {
	t.Helper()

	stubs := newBuildStubs(t, root)
	writeExecutable(t, filepath.Join(stubs.binDir, "go"), `#!/bin/sh
set -eu
printf 'go %s\n' "$*" >> "$CALL_LOG"
if [ "${1:-}" = "run" ] && [ "${2:-}" = "./internal/pricing/cmd/litellm-snapshot" ]; then
  if [ "${RESTORE_FAIL:-}" = "1" ]; then exit 42; fi
  exit 0
fi
if [ "${1:-}" = "build" ]; then
  out=""
  prev=""
  for arg in "$@"; do
    if [ "$prev" = "-o" ]; then
      out="$arg"
      break
    fi
    prev="$arg"
  done
  if [ -n "$out" ]; then
    mkdir -p "$(dirname "$out")"
    cat > "$out" <<'EOS'
#!/bin/sh
printf 'built-binary %s\n' "$*" >> "$CALL_LOG"
exit 0
EOS
    chmod +x "$out"
  fi
  exit 0
fi
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "npm"), `#!/bin/sh
set -eu
printf 'npm %s\n' "$*" >> "$CALL_LOG"
if [ "${1:-}" = "run" ] && [ "${2:-}" = "build" ]; then
  mkdir -p dist
  printf 'ok\n' > dist/index.html
fi
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "git"), `#!/bin/sh
set -eu
printf 'git %s\n' "$*" >> "$CALL_LOG"
case " $* " in
  *" describe "*) printf 'v1.2.3-4-gabcdef\n' ;;
  *" rev-parse "*) printf 'abcdef1\n' ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "rustc"), `#!/bin/sh
set -eu
if [ "${1:-}" = "-vV" ]; then
  printf 'host: x86_64-unknown-linux-gnu\n'
fi
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "cargo"), `#!/bin/sh
set -eu
printf 'cargo %s\n' "$*" >> "$CALL_LOG"
exit 0
`)
	return stubs
}

func installWindowsBuildStubs(t *testing.T, root string) buildStubs {
	t.Helper()

	stubs := newBuildStubs(t, root)
	writeExecutable(t, filepath.Join(stubs.binDir, "go.ps1"), `Add-Content -Path $env:CALL_LOG -Value ("go " + ($args -join " "))
if ($args.Length -gt 0 -and $args[0] -eq "run") {
  if ($env:RESTORE_FAIL -eq "1") { exit 42 }
  exit 0
}
if ($args.Length -gt 0 -and $args[0] -eq "build") {
  $out = $null
  for ($i = 0; $i -lt $args.Length - 1; $i++) {
    if ($args[$i] -eq "-o") {
      $out = $args[$i+1]
      break
    }
  }
  if ($out) {
    $dir = [System.IO.Path]::GetDirectoryName($out)
    if ($dir) {
      New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
    Set-Content -Path $out -Value @'
@echo off
echo built-binary %*>> "%CALL_LOG%"
exit /b 0
'@ -NoNewline
  }
  exit 0
}
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "go.cmd"), `@echo off
echo go %*>> "%CALL_LOG%"
if "%1"=="run" (
  if "%RESTORE_FAIL%"=="1" exit /b 42
  exit /b 0
)
if "%1"=="build" (
  set "out="
  set "next="
:go_loop
  if "%~1"=="" goto go_after
  if defined next (
    set "out=%~1"
    set "next="
  ) else if "%~1"=="-o" (
    set "next=1"
  )
  shift
  goto go_loop
:go_after
  if not "%out%"=="" (
    echo @echo off>"%out%"
    echo echo built-binary %%*^>^> "%%CALL_LOG%%">>"%out%"
    echo exit /b 0>>"%out%"
  )
  exit /b 0
)
exit /b 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "npm.ps1"), `Add-Content -Path $env:CALL_LOG -Value ("npm " + ($args -join " "))
if ($args.Length -ge 2 -and $args[0] -eq "run" -and $args[1] -eq "build") {
  New-Item -ItemType Directory -Path dist -Force | Out-Null
  Set-Content -Path "dist/index.html" -Value "ok" -NoNewline
}
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "npm.cmd"), `@echo off
echo npm %*>> "%CALL_LOG%"
if "%1"=="run" if "%2"=="build" (
  if not exist dist mkdir dist
  echo ok>dist\index.html
)
exit /b 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "git.ps1"), `$joined = $args -join " "
Add-Content -Path $env:CALL_LOG -Value ("git " + $joined)
if ($joined -like "*describe*") {
  Write-Output "v1.2.3-4-gabcdef"
  exit 0
}
if ($joined -like "*rev-parse*") {
  Write-Output "abcdef1"
  exit 0
}
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "git.cmd"), `@echo off
echo git %*>> "%CALL_LOG%"
echo %* | findstr /C:"describe" >nul && (
  echo v1.2.3-4-gabcdef
  exit /b 0
)
echo %* | findstr /C:"rev-parse" >nul && (
  echo abcdef1
  exit /b 0
)
exit /b 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "rustc.ps1"), `if ($args.Length -gt 0 -and $args[0] -eq "-vV") {
  Write-Output "host: x86_64-pc-windows-msvc"
}
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "rustc.cmd"), `@echo off
if "%1"=="-vV" echo host: x86_64-pc-windows-msvc
exit /b 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "cargo.ps1"), `Add-Content -Path $env:CALL_LOG -Value ("cargo " + ($args -join " "))
exit 0
`)
	writeExecutable(t, filepath.Join(stubs.binDir, "cargo.cmd"), `@echo off
echo cargo %*>> "%CALL_LOG%"
exit /b 0
`)
	return stubs
}

func newBuildStubs(t *testing.T, root string) buildStubs {
	t.Helper()

	binDir := filepath.Join(root, "stub-bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	logPath := filepath.Join(root, "calls.log")
	require.NoError(t, os.WriteFile(logPath, nil, 0o644))
	return buildStubs{binDir: binDir, logPath: logPath}
}

func (s buildStubs) env(extra ...string) []string {
	env := envWithout("PATH", "CALL_LOG")
	env = append(env, "PATH="+s.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, "CALL_LOG="+s.logPath)
	return append(env, extra...)
}

func (s buildStubs) events(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(s.logPath)
	require.NoError(t, err)
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.FieldsFunc(normalized, func(r rune) bool { return r == '\n' })
}

func writeE2EWorkspace(t *testing.T, root string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(root, "frontend"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "parser"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "web"), 0o755))

	var registry strings.Builder
	registry.WriteString("package parser\n\nvar agentDirs = []struct{ EnvVar string }{\n")
	for i := range 12 {
		fmt.Fprintf(&registry, "\t{EnvVar: \"AGENT_%02d_DIR\"},\n", i)
	}
	registry.WriteString("}\n")
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "internal", "parser", "types.go"),
		[]byte(registry.String()),
		0o644,
	))
}

func writeDesktopDevWorkspace(t *testing.T, root string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "frontend"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "web"), 0o755))
	tauriDir := filepath.Join(root, "desktop", "src-tauri")
	require.NoError(t, os.MkdirAll(tauriDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tauriDir, "tauri.conf.json"),
		[]byte(`{"version": "0.0.0"}`),
		0o644,
	))
}

func writeFixtureBinary(t *testing.T, root string) string {
	t.Helper()

	fixture := filepath.Join(root, "fixture")
	writeExecutable(t, fixture, `#!/bin/sh
set -eu
printf 'fixture %s\n' "$*" >> "$CALL_LOG"
while [ "$#" -gt 0 ]; do
  case "$1" in
    -out|-duckdb-out)
      shift
      mkdir -p "$(dirname "$1")"
      printf 'fixture\n' > "$1"
      ;;
  esac
  shift || true
done
exit 0
`)
	return fixture
}

func writeServerBinary(t *testing.T, root string) string {
	t.Helper()

	server := filepath.Join(root, "server")
	writeExecutable(t, server, `#!/bin/sh
set -eu
printf 'server %s\n' "$*" >> "$CALL_LOG"
exit 0
`)
	return server
}

func installRepoFile(t *testing.T, root, rel string, mode os.FileMode) {
	t.Helper()

	data, err := os.ReadFile(rel)
	require.NoError(t, err)
	dst := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, data, mode))
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o755))
}

func runInWorkspace(
	t *testing.T,
	root string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = env
	return cmd.CombinedOutput()
}

func requireUnixShell(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("bash script behavior tests run on Unix")
	}
	requireCommand(t, "bash")
}

func requireCommand(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found on PATH", name)
	}
	return path
}

func assertEventOrder(t *testing.T, events []string, wants ...string) {
	t.Helper()

	start := 0
	for _, want := range wants {
		found := -1
		for i := start; i < len(events); i++ {
			if strings.Contains(events[i], want) {
				found = i
				break
			}
		}
		require.NotEqual(t, -1, found,
			"event containing %q not found after index %d in %v", want, start, events)
		start = found + 1
	}
}

func assertEventContains(t *testing.T, events []string, want string) {
	t.Helper()

	for _, event := range events {
		if strings.Contains(event, want) {
			return
		}
	}
	assert.Failf(t, "missing event", "event containing %q not found in %v", want, events)
}

func assertNoEventContains(t *testing.T, events []string, unwanted string) {
	t.Helper()

	for _, event := range events {
		assert.NotContains(t, event, unwanted)
	}
}

func envWithout(keys ...string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		drop[key] = struct{}{}
	}

	var env []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if _, found := drop[key]; found {
			continue
		}
		env = append(env, entry)
	}
	return env
}
