package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupIsolationReadsOnlyFixtureHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	hostHome, err := os.UserHomeDir()
	require.NoError(t, err)
	tempRoot, err := os.MkdirTemp(hostHome, ".agentsview-lookup-isolation-")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(tempRoot)) })
	// The sandbox replaces /tmp, so keep the test home outside it.
	t.Setenv("TMPDIR", tempRoot)
	base := t.TempDir()
	require.NoError(t, os.Chmod(base, 0o700))
	home := filepath.Join(base, "home")
	clientDir := filepath.Join(home, "fixture")
	project := filepath.Join(clientDir, "project")
	dataDir := filepath.Join(clientDir, "agentsview-data")
	runDir := filepath.Join(clientDir, "attempt")
	binDir := filepath.Join(clientDir, "bin")
	for _, dir := range []string{project, dataDir, runDir, binDir, filepath.Join(home, ".codex")} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	private := filepath.Join(home, "private.txt")
	require.NoError(t, os.WriteFile(private, []byte("private"), 0o600))
	prior := filepath.Join(clientDir, "prior-answer.txt")
	require.NoError(t, os.WriteFile(prior, []byte("prior"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "probe"), []byte("fixture"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nif [ -r \"$PRIVATE_PATH\" ] || [ -r \"$PRIOR_PATH\" ] || [ -n \"${ANTHROPIC_API_KEY:-}\" ] || [ -n \"${OPENAI_API_KEY:-}\" ]; then echo leaked; else echo hidden; fi\ncat \"$AGENTSVIEW_DATA_DIR/probe\"\ncat \"$HOME/.agentsview/probe\"\n"), 0o700))
	require.NoError(t, os.Symlink(bwrap, filepath.Join(binDir, "bwrap")))
	bin := filepath.Join(binDir, "agentsview")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("PATH", binDir)
	t.Setenv("ANTHROPIC_API_KEY", "host-anthropic-key")
	t.Setenv("OPENAI_API_KEY", "host-openai-key")
	isolation, err := prepareLookupIsolation(t.Context(), clientCodex, clientDir, project, bin, runDir)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), isolation.command)
	cmd.Env = append(isolation.env, "PRIVATE_PATH="+private, "PRIOR_PATH="+prior)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, "hidden\nfixturefixture", string(out))
	isolation.removeCredentials()
	_, err = os.Stat(filepath.Join(runDir, "home", ".codex", "auth.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.Remove(filepath.Join(home, ".codex", "auth.json")))
	_, err = prepareLookupIsolation(t.Context(), clientCodex, clientDir, project, bin, filepath.Join(clientDir, "missing-auth"))
	require.ErrorContains(t, err, "missing codex credentials")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"turn.failed\",\"error\":{\"message\":\"invalid model\"}}'\nexit 1\n"), 0o700))
	_, _, err = captureLookup(t.Context(), config{timeout: 10 * time.Second}, bin, clientCodex, project, "invalid-model", "", "question", filepath.Join(clientDir, "bad-model"))
	require.ErrorIs(t, err, errLookupSetup)
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call\",\"name\":\"mcp__agentsview__search_content\",\"call_id\":\"x\",\"arguments\":\"{}\"}}' '{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call_output\",\"call_id\":\"x\",\"output\":\"no result\"}}'\nexit 1\n"), 0o700))
	_, _, err = captureLookup(t.Context(), config{timeout: 10 * time.Second}, bin, clientCodex, project, "model", "", "question", filepath.Join(clientDir, "task-failure"))
	require.ErrorIs(t, err, errLookupClientFailed)
	assert.NotErrorIs(t, err, errLookupSetup)
}

func TestLookupMissingClientSetupFails(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, _, err := captureLookup(t.Context(), config{}, "", clientClaude, t.TempDir(), "", "", "", t.TempDir())
	require.ErrorIs(t, err, errLookupSetup)
}

func TestLookupRefusesPopulatedOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing")
	require.NoError(t, os.WriteFile(path, []byte("preserve"), 0o600))
	err := run(t.Context(), config{suite: "lookups", outputDir: dir})
	require.ErrorContains(t, err, "output directory")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "preserve", string(data))
}

func TestLookupUnknownCaseLeavesOutputUncreated(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	output := filepath.Join(t.TempDir(), "output")
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	err = run(t.Context(), config{suite: "lookups", repository: repository, caseName: "missing", outputDir: output})
	require.ErrorContains(t, err, "unknown --case")
	_, err = os.Stat(output)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
