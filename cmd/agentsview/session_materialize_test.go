package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSessionMaterializeRejectsRemoteBackends(t *testing.T) {
	for _, flag := range []string{"--server=http://127.0.0.1:1", "--pg", "--server-token-file="} {
		t.Run(flag, func(t *testing.T) {
			newAgentDataDir(t)
			_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir(), flag)
			require.ErrorContains(t, err, "requires the local archive")
		})
	}
}

func TestClaudeResumeCommandOverrides(t *testing.T) {
	defaultCommand := claudeResumeCommand("/checkout", "id", "", "", false)
	assert.NotContains(t, defaultCommand, "CLAUDE_CONFIG_DIR")
	assert.NotContains(t, defaultCommand, "CLAUDE_CODE_PROJECT_DIR_NAME")
	posix := claudeResumeCommand("/checkout path", "id", "/config path/", "project", false)
	assert.Contains(t, posix, "CLAUDE_CONFIG_DIR='/config path/'")
	assert.Contains(t, posix, "CLAUDE_CODE_PROJECT_DIR_NAME=project")
	assert.Contains(t, claudeResumeCommand("/checkout", "id", "/config", "", false), "CLAUDE_CODE_PROJECT_DIR_NAME=''")
	windows := claudeResumeCommand(`C:\checkout path`, "id", `C:\config path`, "project", true)
	assert.Contains(t, windows, `Set-Location -LiteralPath 'C:\checkout path'`)
	assert.Contains(t, windows, `$env:CLAUDE_CONFIG_DIR = 'C:\config path'`)
	assert.Contains(t, windows, `$env:CLAUDE_CODE_PROJECT_DIR_NAME = 'project'`)
	assert.Contains(t, windows, "finally")
	assert.NotContains(t, claudeResumeCommand(`C:\checkout`, "id", "", "", true), "CLAUDE_CONFIG_DIR")
}

func TestSessionMaterializeUsageOnly(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_ARCHIVE_CONTENT", "usage")
	_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir())
	require.ErrorContains(t, err, "archive_content=usage")
}

func TestSessionMaterializeSnapshotDispatch(t *testing.T) {
	ctx := t.Context()
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	root, dataDir := t.TempDir(), newAgentDataDir(t)
	originalData := filepath.Join(root, "original-data")
	require.NoError(t, os.MkdirAll(originalData, 0o700))
	initial, err := db.OpenIsolatedContext(ctx, filepath.Join(originalData, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, initial.Close())
	dbtest.WriteTestFile(t, filepath.Join(originalData, "telemetry-install-id"), []byte(device))
	projects := filepath.Join(root, "claude", "projects")
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "fixture", id).String()))
	capture, seed := filepath.Join(root, "capture"), filepath.Join(root, "seed")
	settings := rawarchive.RecoverySettings{LocalMachineName: "fixture"}
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: originalData, Destination: capture, Settings: settings, Roots: []rawarchive.RootSpec{{Provider: "claude", Path: projects}}})
	require.NoError(t, err)
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	initial, err = db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, initial, seed, nil)
	require.NoError(t, err)

	repository := filepath.Join(root, "repository")
	report, err := archive.Backup(ctx, repository, settings, "fixture")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, initial.Close())
	require.NoError(t, os.RemoveAll(projects))
	receiver, err := db.OpenIsolatedContext(ctx, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	cfg := config.Config{DataDir: dataDir, ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	manager, err := archivesource.NewManager(ctx, cfg, receiver, engine, nil)
	require.NoError(t, err)
	_, err = manager.Drain(ctx, nil)
	require.NoError(t, err)
	manager.Close()
	engine.Close()
	require.NoError(t, receiver.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(fmt.Sprintf("[[archive_sources]]\nname = %q\nrepository = %q\nsnapshot = %q\n", "fixture", repository, report.SnapshotID)), 0o600))
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
	output, err := executeCommand(newRootCommand(), "session", "materialize", device+"~"+id, "--cwd", t.TempDir(), "--json")
	require.NoError(t, err, output)
	var result struct {
		Source  string `json:"source"`
		Command string `json:"command"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	assert.Equal(t, "snapshot:"+report.SnapshotID, result.Source)
	assert.Contains(t, result.Command, "claude --resume ")
}

func TestPiResumeCommandQuotesPaths(t *testing.T) {
	assert.Equal(t, "cd '/work dir' && pi --session '/data/a b.jsonl'", piResumeCommand("/work dir", "/data/a b.jsonl", false))
	assert.Equal(t, "Set-Location -LiteralPath 'C:\\work dir'; if ($?) { pi --session 'C:\\data\\a b.jsonl' }", piResumeCommand(`C:\work dir`, `C:\data\a b.jsonl`, true))
}

func TestPiMaterializeCommandReturnsNativeResume(t *testing.T) {
	dataDir := newAgentDataDir(t)
	root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_DIR", root)
	const machine = "0123456789abcdef0123456789abcdef"
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "telemetry-install-id"), []byte(machine), 0o600))
	name := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(name, []byte(`{"type":"session","id":"example","cwd":"/old"}`+"\n"), 0o600))
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "pi:example", Agent: "pi", Machine: machine, Project: "fixture", FilePath: &name}))
	require.NoError(t, database.Close())
	output, err := executeCommand(newRootCommand(), "session", "materialize", "pi:example", "--cwd", cwd, "--json")
	require.NoError(t, err, output)
	var result struct {
		Command string `json:"command"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	resolved, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Command, resolved)
	assert.Contains(t, result.Command, filepath.Join(dest, "example.jsonl"))
}

func TestCodexMaterializeCommand(t *testing.T) {
	dataDir := newAgentDataDir(t)
	root, home, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_DIR", "")
	const machine = "0123456789abcdef0123456789abcdef"
	const id = "11111111-1111-4111-8111-111111111111"
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "telemetry-install-id"), []byte(machine), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(fmt.Sprintf("[agents.codex]\ndirs = [%q]\n", root)), 0o600))
	name := filepath.Join(root, "2026", "01", "01", "rollout-2026-01-01T01-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
	require.NoError(t, os.WriteFile(name, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`+"\n", id)), 0o600))
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "codex:" + id, Agent: "codex", Machine: machine, Project: "fixture", FilePath: &name}))
	require.NoError(t, database.Close())
	output, err := executeCommand(newRootCommand(), "session", "materialize", "codex:"+id, "--cwd", cwd, "--json")
	require.NoError(t, err, output)
	var result struct {
		Command       string `json:"command"`
		RequestedID   string `json:"requested_id"`
		InstalledPage string `json:"installed_page"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	assert.Equal(t, "codex:"+id, result.RequestedID)
	assert.Equal(t, filepath.Base(name), result.InstalledPage)
	want := "codex resume " + id + " -C " + cwd
	if runtime.GOOS == "windows" {
		want = "codex resume '" + id + "' -C '" + cwd + "'"
	}
	assert.Equal(t, want, result.Command)
	human, err := executeCommand(newRootCommand(), "session", "materialize", "codex:"+id, "--cwd", cwd)
	require.NoError(t, err, human)
	assert.Contains(t, human, "Requested: codex:"+id)
	assert.Contains(t, human, "Installed page: "+filepath.Base(name))
}
