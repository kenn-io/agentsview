package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	if out, err := exec.CommandContext(t.Context(), bwrap, "--ro-bind", "/", "/", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot run: %s", out)
	}
	hostHome, err := os.UserHomeDir()
	require.NoError(t, err)
	// The sandbox replaces /tmp, so keep the test home outside it.
	t.Setenv("TMPDIR", hostHome)
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	base := t.TempDir()
	require.NoError(t, os.Chmod(base, 0o700))
	home := filepath.Join(base, "home")
	clientDir := filepath.Join(home, "fixture")
	project := filepath.Join(clientDir, "project")
	dataDir := filepath.Join(clientDir, "agentsview-data")
	runDir := filepath.Join(clientDir, "attempt")
	binDir := filepath.Join(clientDir, "bin")
	fixtureRoot := filepath.Join(clientDir, "fixture-sessions")
	for _, dir := range []string{project, dataDir, runDir, binDir, filepath.Join(home, ".codex"), fixtureRoot} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(fixtureRoot, "fixture.jsonl"), []byte("fixture transcript"), 0o600))
	private := filepath.Join(home, "private.txt")
	require.NoError(t, os.WriteFile(private, []byte("private"), 0o600))
	prior := filepath.Join(clientDir, "prior-answer.txt")
	require.NoError(t, os.WriteFile(prior, []byte("prior"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "probe"), []byte("fixture"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nif [ -r \"$PRIVATE_PATH\" ] || [ -r \"$PRIOR_PATH\" ] || [ -r \"/proc/$HOST_PID/root$PRIVATE_PATH\" ] || [ -n \"${ANTHROPIC_API_KEY:-}\" ] || [ -n \"${OPENAI_API_KEY:-}\" ]; then echo leaked; else echo hidden; fi\ncat \"$AGENTSVIEW_DATA_DIR/probe\"\ncat \"$HOME/.agentsview/probe\"\ncat \"$HOME/.codex/sessions/fixture.jsonl\"\nprintf 'new transcript' > \"$HOME/.codex/sessions/fresh.jsonl\"\n"), 0o700))
	require.NoError(t, os.Symlink(bwrap, filepath.Join(binDir, "bwrap")))
	bin := filepath.Join(binDir, "agentsview")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("PATH", binDir)
	t.Setenv("ANTHROPIC_API_KEY", "host-anthropic-key")
	t.Setenv("OPENAI_API_KEY", "host-openai-key")
	require.NoError(t, copyLookupTranscriptsForClient(t.Context(), clientCodex, fixtureRoot, filepath.Join(runDir, "home"), project))
	isolation, err := prepareLookupIsolation(t.Context(), clientCodex, clientDir, project, project, bin, runDir)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), isolation.command)
	cmd.Env = append(cmd.Env, isolation.env...)
	cmd.Env = append(cmd.Env, "PRIVATE_PATH="+private, "PRIOR_PATH="+prior, "HOST_PID="+strconv.Itoa(os.Getpid()))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, "hidden\nfixturefixturefixture transcript", string(out))
	data, err := os.ReadFile(filepath.Join(runDir, "home", ".codex", "sessions", "fresh.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, "new transcript", string(data))
	isolation.removeCredentials()
	_, err = os.Stat(filepath.Join(runDir, "home", ".codex", "auth.json"))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.Remove(filepath.Join(home, ".codex", "auth.json")))
	_, err = prepareLookupIsolation(t.Context(), clientCodex, clientDir, project, project, bin, filepath.Join(clientDir, "missing-auth"))
	require.ErrorContains(t, err, "missing codex credentials")
}

func TestLookupIsolationRequiresNodeOnlyForNodeScripts(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         bool
	}{
		{"node mentioned in comment", "#!/bin/sh\n# node is not the interpreter\nexit 0\n", false},
		{"node interpreter", "#!/usr/bin/env node\n", true},
		{"shell shim falling back to node", "#!/bin/sh\nbasedir=$(dirname \"$0\")\nif [ -x \"$basedir/node\" ]; then\n  exec \"$basedir/node\" \"$basedir/client.js\" \"$@\"\nelse\n  exec node \"$basedir/client.js\" \"$@\"\nfi\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client")
			require.NoError(t, os.WriteFile(path, []byte(tc.script), 0o700))
			needsNode, err := lookupExecutableNeedsNode(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, needsNode)
		})
	}
}

func TestLookupIsolationRejectsOutputOutsideHiddenRoots(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	t.Setenv("TMPDIR", "/dev/shm")
	outside := t.TempDir()
	home := filepath.Join(outside, "home")
	binDir := filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o700))
	require.NoError(t, os.Symlink(bwrap, filepath.Join(binDir, "bwrap")))
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	for _, tc := range []struct {
		name, clientDir, repository string
	}{
		{name: "output", clientDir: filepath.Join(outside, "output"), repository: filepath.Join(home, "repository")},
		{name: "repository", clientDir: filepath.Join(home, "output"), repository: filepath.Join(outside, "repository")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := filepath.Join(tc.clientDir, "project")
			runDir := filepath.Join(tc.clientDir, "attempt")
			for _, dir := range []string{tc.clientDir, tc.repository, project, runDir} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
			_, err := prepareLookupIsolation(t.Context(), clientCodex, tc.clientDir, tc.repository, project, filepath.Join(tc.clientDir, "agentsview"), runDir)
			require.ErrorContains(t, err, "fixture and output paths must be under the home directory or /tmp")
		})
	}
}

func TestLookupCaptureClassifiesSetupAndClientFailures(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	if out, err := exec.CommandContext(t.Context(), bwrap, "--ro-bind", "/", "/", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot run: %s", out)
	}
	hostHome, err := os.UserHomeDir()
	require.NoError(t, err)
	t.Setenv("TMPDIR", hostHome)
	base := t.TempDir()
	require.NoError(t, os.Chmod(base, 0o700))
	home := filepath.Join(base, "home")
	clientDir := filepath.Join(home, "fixture")
	project := filepath.Join(clientDir, "project")
	dataDir := filepath.Join(clientDir, "agentsview-data")
	fixtureRoot := filepath.Join(clientDir, "fixture-sessions")
	binDir := filepath.Join(clientDir, "bin")
	for _, dir := range []string{home, filepath.Join(home, ".codex"), filepath.Join(home, ".claude"), project, dataDir, fixtureRoot, binDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("{}"), 0o600))
	require.NoError(t, os.Symlink(bwrap, filepath.Join(binDir, "bwrap")))
	serverBin := filepath.Join(binDir, "agentsview")
	require.NoError(t, os.WriteFile(serverBin, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("PATH", binDir)

	cases := []struct {
		name, executable, script string
		client                   clientName
		want, wantNot            error
		wantText                 string
	}{
		{name: "missing client", client: clientClaude, want: errLookupSetup, wantText: "claude"},
		{
			name: "invalid model", executable: "claude", client: clientClaude,
			script: "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"result\":\"invalid model\"}'\nexit 1\n",
			want:   errLookupSetup,
		},
		{
			name: "task failure after answer", executable: "codex", client: clientCodex,
			script: "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"partial answer\"}}'\nexit 1\n",
			want:   errLookupClientFailed, wantNot: errLookupSetup,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runDir := filepath.Join(clientDir, "attempt-"+tc.name)
			if tc.name == "invalid model" {
				root, err := lookupTranscriptRoot(clientClaude, filepath.Join(runDir, "home"), project)
				require.NoError(t, err)
				tc.script = "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --session-id ]; then shift; sid=$1; fi; shift; done\n" +
					"mkdir -p " + strconv.Quote(root) + "\nprintf '%s\\n' '{}' > " + strconv.Quote(root) + "/\"$sid.jsonl\"\n" +
					"printf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"result\":\"invalid model\"}'\nexit 1\n"
			}
			if tc.executable != "" {
				require.NoError(t, os.WriteFile(filepath.Join(binDir, tc.executable), []byte(tc.script), 0o700))
			}
			_, _, err := captureLookup(t.Context(), config{timeout: 10 * time.Second, repository: project}, serverBin, tc.client, project, "model", "", "question", runDir)
			require.ErrorIs(t, err, tc.want)
			if tc.wantNot != nil {
				require.NotErrorIs(t, err, tc.wantNot)
			}
			if tc.wantText != "" {
				assert.ErrorContains(t, err, tc.wantText)
			}
		})
	}
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
