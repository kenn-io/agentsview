package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSyncWorkerProfileHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_PROFILE_TEST_HELPER") != "1" {
		return
	}
	command := newSyncWorkerCommand()
	command.SetArgs([]string{"--mode", "startup"})
	require.NoError(t, command.Execute())
	os.Exit(0)
}

func TestSyncProfileTraceExcludesHeapSnapshotGC(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "runtime.trace")
	stop := startSyncProfile(SyncConfig{
		Trace: tracePath, MemProfile: filepath.Join(dir, "memory.pprof"),
	})
	stop()

	// Inspect the artifact: the forced heap-snapshot GC is profiling cleanup,
	// not work performed by the operation being measured.
	command := exec.CommandContext(t.Context(), "go", "tool", "trace", "-d=parsed", tracePath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.NotContains(t, string(output), "\truntime.GC @")
}

func TestSyncWorkerProfilesActualPass(t *testing.T) {
	for _, tc := range []struct {
		name, trace            string
		disabled, badDirectory bool
		wantFiles              []string
	}{
		{name: "default disabled", disabled: true},
		{name: "profiles", wantFiles: []string{"cpu.pprof", "memory.pprof"}},
		{name: "trace", trace: "true", wantFiles: []string{"cpu.pprof", "memory.pprof", "runtime.trace"}},
		{name: "invalid trace", trace: "invalid"},
		{name: "invalid directory", badDirectory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(fmt.Sprintf(
				"disable_update_check = true\n[[session_sources]]\nagent = \"claude\"\ndir = %q\n", cfg.AgentDirs[parser.AgentClaude][0],
			)), 0o600))
			root := filepath.Join(t.TempDir(), "profiles")
			if tc.badDirectory {
				require.NoError(t, os.WriteFile(root, []byte("occupied"), 0o600))
			}
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSyncWorkerProfileHelperProcess$")
			home, temp := t.TempDir(), t.TempDir()
			command.Env = []string{
				"HOME=" + home, "USERPROFILE=" + home,
				"TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp,
				"AGENTSVIEW_DATA_DIR=" + cfg.DataDir, "AGENTSVIEW_NO_DAEMON=1",
				"AGENTSVIEW_PROFILE_TEST_HELPER=1",
				"AGENTSVIEW_SYNC_PROFILE_TRACE=" + tc.trace,
			}
			if !tc.disabled {
				command.Env = append(command.Env, "AGENTSVIEW_SYNC_PROFILE_DIR="+root)
			}
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			require.NoError(t, command.Run(), stderr.String())
			result := decodeSingleResult(t, &stdout)
			require.Equal(t, "ok", result.Status)
			assert.Equal(t, 3, result.Synced)
			if len(tc.wantFiles) == 0 {
				// Bad profile options must leave the worker pass functional.
				return
			}
			dirs, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Len(t, dirs, 1)
			dir := filepath.Join(root, dirs[0].Name())
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
				require.NoError(t, err)
				profile, err := io.ReadAll(compressed)
				require.NoError(t, err)
				require.NoError(t, compressed.Close())
				assert.NotEmpty(t, profile, "profile must survive worker shutdown")
			}
			if tc.trace == "true" {
				trace, err := os.ReadFile(filepath.Join(dir, "runtime.trace"))
				require.NoError(t, err)
				assert.NotEmpty(t, trace)
			}
		})
	}
}
