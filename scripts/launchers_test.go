//go:build !windows

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProductionLauncherOpensAuthenticatedRuntimeURL(t *testing.T) {
	projectDir, recordDir, env := launcherFixture(t, "start-agentsview.command")
	writeExecutable(t, filepath.Join(projectDir, "agentsview"), `#!/bin/sh
printf '%s\n' "$*" >> "$RECORD_DIR/agentsview-args"
case "$1" in
serve)
	printf '%s\n' "agentsview running at http://127.0.0.1:43123 (pid 42)"
	printf '%s\n' "Logs: private/serve.log"
	;;
daemon)
	exit 0
	;;
esac
`)

	output, err := runLauncher(t, projectDir, "start-agentsview.command", env, "")
	require.NoError(t, err, output)
	assert.Equal(t, "http://127.0.0.1:43123\n",
		readRecorded(t, recordDir, "open"))
	assert.Contains(t, readRecorded(t, recordDir, "agentsview-args"),
		"serve --background --host 127.0.0.1 --port 0 --no-browser")
	assert.Contains(t, readRecorded(t, recordDir, "curl"),
		"--fail --silent http://127.0.0.1:43123/api/ping")
}

func TestProductionLauncherRejectsNonLoopbackStartupURL(t *testing.T) {
	projectDir, recordDir, env := launcherFixture(t, "start-agentsview.command")
	writeExecutable(t, filepath.Join(projectDir, "agentsview"), `#!/bin/sh
printf '%s\n' "agentsview running at http://attacker.example:43123 (pid 42)"
`)

	output, err := runLauncher(t, projectDir, "start-agentsview.command", env, "")
	require.Error(t, err, output)
	_, statErr := os.Stat(filepath.Join(recordDir, "open"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestDevelopmentLauncherUsesPrivateLogsAndActualChildURLs(t *testing.T) {
	projectDir, recordDir, env := launcherFixture(t, "start-agentsview-dev.command")
	require.NoError(t, os.MkdirAll(
		filepath.Join(projectDir, "frontend", "node_modules"), 0o755))
	writeExecutable(t, filepath.Join(projectDir, "agentsview"), `#!/bin/sh
printf '%s\n' "$*" >> "$RECORD_DIR/agentsview-args"
case "$1" in
serve)
	printf '%s\n' "agentsview running at http://127.0.0.1:44123 (pid 43)"
	;;
daemon)
	exit 0
	;;
esac
`)
	writeExecutable(t, filepath.Join(filepath.Dir(projectDir), "bin", "npm"), `#!/bin/sh
printf '%s|%s\n' "$VITE_API_TARGET" "$*" > "$RECORD_DIR/npm"
printf '%s\n' "  Local:   http://127.0.0.1:49222/"
while :; do
	/bin/sleep 1
done
`)

	output, err := runLauncher(
		t, projectDir, "start-agentsview-dev.command", env, "\n")
	require.NoError(t, err, output)
	assert.Equal(t, "http://127.0.0.1:49222\n",
		readRecorded(t, recordDir, "open"))
	assert.Contains(t, readRecorded(t, recordDir, "npm"),
		"http://127.0.0.1:44123|run dev -- --host 127.0.0.1 --port 0 --strictPort")
	curlCalls := readRecorded(t, recordDir, "curl")
	assert.Contains(t, curlCalls,
		"--fail --silent http://127.0.0.1:44123/api/ping")
	assert.Contains(t, curlCalls,
		"--fail --silent http://127.0.0.1:49222/")
	assert.Equal(t, "700\n", readRecorded(t, recordDir, "launch-mode"))
	launchDir := strings.TrimSpace(readRecorded(t, recordDir, "launch-dir"))
	assert.True(t, strings.HasPrefix(launchDir, filepath.Join(
		filepath.Dir(projectDir), "tmp", "agentsview-launch.")))
}

func launcherFixture(t *testing.T, launcher string) (string, string, []string) {
	t.Helper()
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	recordDir := filepath.Join(root, "records")
	binDir := filepath.Join(root, "bin")
	tempDir := filepath.Join(root, "tmp")
	for _, dir := range []string{projectDir, recordDir, binDir, tempDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	source, err := os.ReadFile(filepath.Join("..", launcher))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, launcher), source, 0o755))
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/bin/sh
printf '%s\n' "$*" >> "$RECORD_DIR/curl"
`)
	writeExecutable(t, filepath.Join(binDir, "open"), `#!/bin/sh
printf '%s\n' "$1" > "$RECORD_DIR/open"
for dir in "$TMPDIR"/agentsview-launch.*; do
	[ -d "$dir" ] || continue
	printf '%s\n' "$dir" > "$RECORD_DIR/launch-dir"
case "$(uname -s)" in
Darwin)
	mode=$(stat -f '%Lp' "$dir")
	;;
Linux)
	mode=$(stat -c '%a' "$dir")
	;;
*)
	printf 'unsupported platform: %s\n' "$(uname -s)" >&2
	exit 1
	;;
esac
	printf '%s\n' "$mode" > "$RECORD_DIR/launch-mode"
done
`)
	writeExecutable(t, filepath.Join(binDir, "osascript"), "#!/bin/sh\nexit 0\n")

	env := append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"RECORD_DIR="+recordDir,
		"TMPDIR="+tempDir,
	)
	return projectDir, recordDir, env
}

func runLauncher(
	t *testing.T, projectDir, launcher string, env []string, input string,
) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(projectDir, launcher))
	cmd.Dir = projectDir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o755))
}

func readRecorded(t *testing.T, dir, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(contents)
}
