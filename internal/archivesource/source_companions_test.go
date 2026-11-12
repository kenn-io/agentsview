package archivesource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func backupSourceRoots(t *testing.T, inputs string, roots []rawarchive.RootSpec) config.ArchiveSource {
	t.Helper()
	ctx := t.Context()
	data := filepath.Join(inputs, "application")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(sourceDevice))
	capture := filepath.Join(inputs, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Roots: roots, Settings: settings})
	require.NoError(t, err)
	seed := filepath.Join(inputs, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	repo := filepath.Join(t.TempDir(), "repository")
	report, err := archive.Backup(ctx, repo, settings, "test")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	return config.ArchiveSource{Name: "history", Repository: repo, Snapshot: report.SnapshotID}
}

func TestAttachedSourceRetainsContinuationsAndForks(t *testing.T) {
	inputs := t.TempDir()
	claude := filepath.Join(inputs, "claude")
	first := filepath.Join(claude, "project", "parent-a", "subagents", "agent-reviewer.jsonl")
	second := filepath.Join(claude, "project", "parent-b", "subagents", "agent-reviewer.jsonl")
	dbtest.WriteTestFile(t, first, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "earlier question").AddClaudeAssistant("2026-01-01T00:00:01Z", "earlier answer").String()))
	continuation := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2026-01-01T00:01:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2026-01-01T00:01:01Z", "reply", "b", "a").
		AddClaudeUserWithUUID("2026-01-01T00:01:02Z", "one", "c", "b").
		AddClaudeAssistantWithUUID("2026-01-01T00:01:03Z", "one reply", "d", "c").
		AddClaudeUserWithUUID("2026-01-01T00:01:04Z", "two", "e", "d").
		AddClaudeAssistantWithUUID("2026-01-01T00:01:05Z", "two reply", "f", "e").
		AddClaudeUserWithUUID("2026-01-01T00:01:06Z", "three", "g", "f").
		AddClaudeAssistantWithUUID("2026-01-01T00:01:07Z", "three reply", "h", "g").
		AddClaudeUserWithUUID("2026-01-01T00:01:08Z", "four", "k", "h").
		AddClaudeAssistantWithUUID("2026-01-01T00:01:09Z", "four reply", "l", "k").
		AddClaudeUserWithUUID("2026-01-01T00:02:00Z", "fork question", "i", "b").
		AddClaudeAssistantWithUUID("2026-01-01T00:02:01Z", "fork answer", "j", "i").String()
	dbtest.WriteTestFile(t, second, []byte(continuation))
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	parentRoot, childRoot := filepath.Join(inputs, "parent"), filepath.Join(inputs, "child")
	dbtest.WriteTestFile(t, filepath.Join(parentRoot, "sessions", "rollout-2026-01-01T00-00-00-"+parent+".jsonl"), []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/workspace/project-a", "codex_cli_rs", "2026-01-01T00:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", "2026-01-01T00:00:01Z"),
		testjsonl.CodexMsgJSON("user", "parent task", "2026-01-01T00:00:02Z"))))
	dbtest.WriteTestFile(t, filepath.Join(childRoot, "sessions", "rollout-2026-01-01T01-00-00-"+child+".jsonl"), []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSubagentSessionMetaJSON(child, parent, "/workspace/project-a", "codex_cli_rs", "2026-01-01T01:00:00Z"),
		testjsonl.CodexSessionMetaJSON(parent, "/workspace/project-a", "codex_cli_rs", "2026-01-01T01:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", "2026-01-01T01:00:01Z"),
		testjsonl.CodexMsgJSON("user", "replayed parent task", "2026-01-01T01:00:02Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "child-turn", "2026-01-01T01:01:00Z"),
		testjsonl.CodexMsgJSON("user", "child task", "2026-01-01T01:01:01Z"),
		testjsonl.CodexMsgJSON("assistant", "child answer", "2026-01-01T01:01:02Z"))))
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: claude}, {Provider: "codex", Path: childRoot}, {Provider: "codex", Path: parentRoot}})
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	receiver := dbtest.OpenTestDB(t)
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 3)
	key := db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: index.Info.SnapshotID}
	require.NoError(t, receiver.RegisterArchiveAttachment(t.Context(), key, index.ArchiveID))
	targets := make([]db.ArchiveSourceTarget, 0, len(sources))
	for _, item := range sources {
		targets = append(targets, db.ArchiveSourceTarget{Provenance: index.provenance(item), Target: "target"})
	}
	require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), key, "target", targets))
	for _, item := range sources {
		_, err := index.syncSource(t.Context(), source.Repository, item, config.Config{DataDir: filepath.Dir(receiver.Path())}, engine, t.TempDir(), "target", nil)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		native, parent string
		contents       []string
	}{
		{"agent-reviewer-i", "agent-reviewer", []string{"fork question", "fork answer"}},
		{"codex:" + child, "codex:" + parent, []string{"child task", "child answer"}},
	} {
		session, err := receiver.GetSessionFull(t.Context(), sourceDevice+"~"+tc.native)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, new(sourceDevice+"~"+tc.parent), session.ParentSessionID)
		require.NotNil(t, session.FilePath)
		assert.Contains(t, *session.FilePath, "archive://")
		messages, err := receiver.GetAllMessages(t.Context(), session.ID)
		require.NoError(t, err)
		contents := make([]string, 0, len(messages))
		for _, message := range messages {
			contents = append(contents, message.Content)
		}
		assert.Equal(t, tc.contents, contents)
	}
}
