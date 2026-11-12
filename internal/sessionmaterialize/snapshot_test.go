package sessionmaterialize

import (
	"context"
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

// Capture and attach real synthetic sources so publication is exercised through
// the same independent parent and subagent manifests as a saved archive.
func TestMaterializeSnapshotInstallsCapturedCompanions(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	projects := filepath.Join(root, "projects")
	primary := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "parent fixture", id).String()
	child := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "child fixture", id).String()
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id+".jsonl"), []byte(primary))
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "tool-results", "tool.txt"), []byte("tool output"))
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "subagents", "agent-child.jsonl"), []byte(child))
	capture := filepath.Join(root, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "fixture"}
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Settings: settings, Roots: []rawarchive.RootSpec{{Provider: "claude", Path: projects}}})
	require.NoError(t, err)
	seed := filepath.Join(root, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	repository := filepath.Join(root, "repository")
	report, err := archive.Backup(ctx, repository, settings, "fixture")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	require.NoError(t, os.RemoveAll(projects))

	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := archivesource.NewManager(ctx, cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Drain(ctx, nil)
	require.NoError(t, err)
	session, err := receiver.GetSessionFull(ctx, device+"~"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	home, scratch, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
	t.Setenv("TMPDIR", scratch)
	result, err := Materialize(ctx, cfg, receiver, session, cwd)
	require.NoError(t, err)
	assert.Equal(t, "snapshot:"+report.SnapshotID, result.Source)
	assert.Equal(t, "installed", result.Status)
	dest := filepath.Join(home, "projects", "fixture")
	got, err := os.ReadFile(filepath.Join(dest, id+".jsonl"))
	require.NoError(t, err)
	assert.Equal(t, primary, string(got))
	got, err = os.ReadFile(filepath.Join(dest, id, "tool-results", "tool.txt"))
	require.NoError(t, err)
	assert.Equal(t, "tool output", string(got))
	got, err = os.ReadFile(filepath.Join(dest, id, "subagents", "agent-child.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, child, string(got))
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, entries)
	bad := *session
	bad.Machine = "wrong-device"
	_, err = Materialize(ctx, cfg, receiver, &bad, cwd)
	require.ErrorContains(t, err, "archive session identity differs")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = Materialize(cancelled, cfg, receiver, session, cwd)
	require.ErrorIs(t, err, context.Canceled)
	entries, err = os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
