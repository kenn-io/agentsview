package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// TestSyncWorkerProfilesActualPass runs the real sync-worker command in a
// subprocess and checks both the sync outcome and the profiling artifacts the
// worker leaves behind for each environment setting.
func TestSyncWorkerProfilesActualPass(t *testing.T) {
	if testing.Short() {
		t.Skip("real-spawn worker test re-execs the binary; skipped in -short")
	}
	for _, tc := range []struct {
		name         string
		trace        string
		disabled     bool
		badDirectory bool
		wantFiles    []string
	}{
		{name: "default disabled", disabled: true},
		{name: "profiles", wantFiles: []string{"cpu.pprof", "memory.pprof"}},
		{name: "trace disabled explicitly", trace: "false", wantFiles: []string{"cpu.pprof", "memory.pprof"}},
		{name: "trace", trace: "true", wantFiles: []string{"cpu.pprof", "memory.pprof", "runtime.trace"}},
		{name: "invalid trace", trace: "invalid"},
		{name: "unwritable directory", badDirectory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			root := filepath.Join(t.TempDir(), "profiles")
			if tc.badDirectory {
				// A regular file where the profile root belongs makes the
				// directory impossible to create.
				require.NoError(t, os.WriteFile(root, []byte("occupied"), 0o600))
			}

			cmd := exec.CommandContext(t.Context(),
				os.Args[0],
				"-test.run=^TestSyncWorkerMainHelperProcess$",
				"--",
				"sync-worker", "--mode", "startup",
			)
			// Isolated home and temp directories; Windows reads USERPROFILE,
			// TMP, and TEMP where Unix reads HOME and TMPDIR.
			home, temp := t.TempDir(), t.TempDir()
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + home, "USERPROFILE=" + home,
				"TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp,
				"AGENTSVIEW_SYNC_WORKER_MAIN_HELPER=1",
				"AGENTSVIEW_DATA_DIR=" + cfg.DataDir,
				"CLAUDE_PROJECTS_DIR=" + cfg.AgentDirs[parser.AgentClaude][0],
				syncWorkerProfileTraceEnv + "=" + tc.trace,
			}
			if value := os.Getenv("SYSTEMROOT"); value != "" {
				cmd.Env = append(cmd.Env, "SYSTEMROOT="+value)
			}
			if !tc.disabled {
				cmd.Env = append(cmd.Env, syncWorkerProfileDirEnv+"="+root)
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			require.NoError(t, cmd.Run(),
				"profiling settings must not fail the pass; stderr:\n%s", stderr.String())

			// Profiling diagnostics must stay off stdout: every line still
			// decodes as a worker protocol record.
			result := decodeSingleResult(t, &stdout)
			assert.Equal(t, "ok", result.Status)
			assert.Equal(t, 3, result.Synced)

			if len(tc.wantFiles) == 0 {
				if !tc.badDirectory {
					assert.NoFileExists(t, root, "disabled capture must not create output")
					assert.NoDirExists(t, root, "disabled capture must not create output")
				}
				return
			}

			dirs, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Len(t, dirs, 1, "one private directory per worker")
			assert.True(t, strings.HasPrefix(dirs[0].Name(), "sync-worker-startup-"),
				"worker directory %q is named for its mode", dirs[0].Name())
			dir := filepath.Join(root, dirs[0].Name())
			if runtime.GOOS != "windows" {
				info, err := os.Stat(dir)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
			}

			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			names := make([]string, 0, len(files))
			for _, file := range files {
				names = append(names, file.Name())
			}
			assert.ElementsMatch(t, tc.wantFiles, names)

			for _, name := range []string{"cpu.pprof", "memory.pprof"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				compressed, err := gzip.NewReader(bytes.NewReader(data))
				require.NoError(t, err, "%s must be a complete gzip-encoded profile", name)
				profile, err := io.ReadAll(compressed)
				require.NoError(t, err, "%s must be flushed before the worker exits", name)
				assert.NotEmpty(t, profile, "%s must survive worker shutdown", name)
			}
			if tc.trace == "true" {
				trace, err := os.ReadFile(filepath.Join(dir, "runtime.trace"))
				require.NoError(t, err)
				assert.NotEmpty(t, trace)
			}
		})
	}
}

// TestSyncProfileTraceExcludesHeapSnapshotGC checks that the forced GC taken
// for the final heap profile happens after tracing stops, so profiling cleanup
// does not appear as work done by the measured operation.
func TestSyncProfileTraceExcludesHeapSnapshotGC(t *testing.T) {
	if testing.Short() {
		t.Skip("parses the trace with the Go toolchain; skipped in -short")
	}
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "runtime.trace")
	memPath := filepath.Join(dir, "memory.pprof")
	stop := startSyncProfile(SyncConfig{Trace: tracePath, MemProfile: memPath})
	stop()

	require.FileExists(t, memPath, "heap snapshot must still be written")
	cmd := exec.CommandContext(t.Context(), "go", "tool", "trace", "-d=parsed", tracePath)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	// The parsed dump prints each event's stack; a runtime.GC frame means the
	// forced collection ran while tracing was still active.
	assert.NotContains(t, string(output), "\truntime.GC @",
		"the trace must end before the heap snapshot's forced GC")
}
