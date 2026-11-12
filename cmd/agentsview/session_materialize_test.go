package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	projects := filepath.Join(root, "projects")
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
		Status  string `json:"status"`
		Command string `json:"command"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	assert.Equal(t, "snapshot:"+report.SnapshotID, result.Source)
	assert.Equal(t, "installed", result.Status)
	assert.Contains(t, result.Command, "claude --resume ")
}
