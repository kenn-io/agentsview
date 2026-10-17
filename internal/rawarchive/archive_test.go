package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSeededReparseRetainsMisfiledCodexSession(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	home := t.TempDir()
	path := filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+b+".jsonl")
	dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
		AddCodexMeta("2026-01-01T00:00:00Z", b, "/workspace/project-a", "codex_cli_rs").
		AddCodexMeta("2026-01-01T00:00:01Z", a, "/workspace/project-a", "codex_cli_rs").
		AddCodexMessage("2026-01-01T00:00:02Z", "user", "Retained question").String()))
	opts := newImportCapture(t, owner, "source", RootSpec{Provider: "codex", Path: home})
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, source.UpsertSession(ctx, db.Session{
		ID: "codex:" + a, Agent: "codex", Project: "project-a", Machine: owner, FilePath: &path,
	}))
	require.NoError(t, source.InsertMessages(ctx, []db.Message{{SessionID: "codex:" + a, Role: "user", Content: "Retained question"}}))
	messages, err := source.GetAllMessages(ctx, "codex:"+a)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	_, err = source.PinMessage(ctx, "codex:"+a, messages[0].ID, new("Saved note"))
	require.NoError(t, err)
	require.NoError(t, source.RenameSession(ctx, "codex:"+a, new("Saved name")))
	_, err = source.StarSession(ctx, "codex:"+a)
	require.NoError(t, err)
	window, err := source.BuildRecallEvidenceWindow(ctx, "codex:"+a, 0, 0)
	require.NoError(t, err)
	selection, err := window.BindSelection(db.RecallEvidenceSelection{MessageStartOrdinal: 0, MessageEndOrdinal: 0})
	require.NoError(t, err)
	_, err = source.InsertRecallEntry(ctx, db.RecallEntry{
		ID: "saved-fact", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Saved fact", Body: "Saved evidence", SourceSessionID: "codex:" + a, Agent: "codex",
		ProvenanceOK: true, Evidence: []db.RecallEvidence{{SessionID: "codex:" + a, ContentDigest: selection.ContentDigest}},
	})
	require.NoError(t, err)
	before, err := source.ExportConversationChanges(ctx, db.ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, before.Changes, 1)
	require.NoError(t, source.Close())
	opts.Destination = filepath.Join(t.TempDir(), "capture")
	_, err = Capture(ctx, opts)
	require.NoError(t, err)
	seedPath := filepath.Join(t.TempDir(), "seed")
	_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), seedPath, nil)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(home))
	seeded, err := db.OpenIsolatedContext(ctx, filepath.Join(seedPath, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, seeded.Close()) })
	archive, err := Open(ctx, seeded, seedPath, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	for range 2 {
		report, err := archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
		require.NoError(t, err)
		assert.Equal(t, 1, report.Parsed)
		after, err := seeded.GetAllMessages(ctx, "codex:"+a)
		require.NoError(t, err)
		assert.Equal(t, messages, after)
		curated, err := seeded.GetSessionFull(ctx, "codex:"+a)
		require.NoError(t, err)
		require.NotNil(t, curated)
		assert.Equal(t, new("Saved name"), curated.DisplayName)
		stars, err := seeded.ListStarredSessionIDs(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"codex:" + a}, stars)
		pins, err := seeded.ListPinnedMessages(ctx, "codex:"+a, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Equal(t, new("Saved note"), pins[0].Note)
		recall, err := seeded.GetRecallEntry(ctx, "saved-fact")
		require.NoError(t, err)
		require.NotNil(t, recall)
		assert.Equal(t, "codex:"+a, recall.SourceSessionID)
		require.Len(t, recall.Evidence, 1)
		assert.Equal(t, "codex:"+a, recall.Evidence[0].SessionID)
		parsed, err := seeded.GetAllMessages(ctx, "codex:"+b)
		require.NoError(t, err)
		require.Len(t, parsed, 1)
		assert.Equal(t, "Retained question", parsed[0].Content)
		exported, err := seeded.ExportConversationChanges(ctx, db.ConversationExportOptions{})
		require.NoError(t, err)
		var retained db.ConversationChange
		for _, change := range exported.Changes {
			if change.MessageID == before.Changes[0].MessageID {
				retained = change
			}
		}
		assert.Equal(t, "codex:"+a, retained.SessionID)
		assert.False(t, retained.Deleted)
	}
}

func TestRejectedIdentityKeepsHistoryAndChangedImportKeepsSource(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	path := filepath.Join(root, "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	header := testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project-a", "codex_cli_rs").String()
	valid := header + testjsonl.NewSessionBuilder().AddCodexMessage("2026-01-01T00:00:01Z", "user", "retained question").String()
	dbtest.WriteTestFile(t, path, []byte(valid))
	database := dbtest.OpenTestDB(t)
	engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Ephemeral: true, Machine: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DisableFilesystemProjectDiscovery: true})
	require.NoError(t, engine.SyncPathsContext(ctx, []string{path}))
	engine.Close()
	before, err := database.GetAllMessages(ctx, "codex:"+id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	// The same native session ID belongs to a different original source path.
	// Reject that collision after parsing without publishing scratch writes.
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	capturedRoot := t.TempDir()
	capturedPath := filepath.Join(capturedRoot, filepath.Base(path))
	dbtest.WriteTestFile(t, capturedPath, []byte(valid))
	capture := newImportCapture(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source", RootSpec{Provider: "codex", Path: capturedRoot})
	spec := loadTestCapture(t, &capture)
	report, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	require.Equal(t, 1, report.Sources)
	sources, err := database.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.Error(t, err)
	assert.Zero(t, report.Parsed)
	after, err := database.GetAllMessages(ctx, "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	source, err := database.GetRawArchiveSource(ctx, sources[0].ManifestID)
	require.NoError(t, err)
	assert.NotEmpty(t, source.ParseError)
	report, err = archive.Verify(ctx)
	require.NoError(t, err)
	assert.Zero(t, report.Parsed)
	// A different capture of the same identity is retained but never silently
	// reparented or allowed to replace the accepted source.
	require.NoError(t, os.WriteFile(capturedPath, []byte(header), 0o600))
	spec = loadTestCapture(t, &capture)
	report, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Len(t, report.Gaps, 1)
	head, err := database.RawArchiveSourceByKey(ctx, source.RootID, source.SourceKey)
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.Equal(t, source.ManifestID, head.ManifestID)
	files, err := database.ListRawArchiveFiles(ctx, 0, 100)
	require.NoError(t, err)
	var versions int
	for _, file := range files {
		if file.RootID == source.RootID && file.Path == filepath.Base(path) {
			versions++
		}
	}
	assert.Equal(t, 2, versions)
	target := filepath.Join(t.TempDir(), "native")
	_, err = archive.Extract(ctx, target, "")
	require.Error(t, err)
	assert.NoDirExists(t, target)
}

// Sources absent from the seed must still claim distinct identities within one
// batch. Otherwise the second parse silently overwrites the first scratch row.
func TestReparseRejectsBatchIdentityCollision(t *testing.T) {
	for _, condition := range []string{"distinct roots", "same root, overlapping continuation paths"} {
		t.Run(condition, func(t *testing.T) {
			ctx := t.Context()
			database := dbtest.OpenTestDB(t)
			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
			require.NoError(t, database.EnableArchiveOnly(ctx))
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			defer archive.Close()
			id := "019eb791-cf7d-75c1-8439-9ed74c122e02"
			var roots []RootSpec
			if condition == "distinct roots" {
				for _, name := range []string{"one", "two"} {
					root := t.TempDir()
					dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", name, id).String()))
					roots = append(roots, RootSpec{Provider: "claude", Path: root})
				}
			} else {
				id = "agent-reviewer"
				root := t.TempDir()
				for _, parent := range []string{"parent-a", "parent-b"} {
					path := filepath.Join(root, "project", parent, "subagents", id+".jsonl")
					dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-08-05T03:40:00Z", parent+" question").AddClaudeAssistant("2026-08-05T03:41:00Z", parent+" answer").String()))
				}
				roots = []RootSpec{{Provider: "claude", Path: root}}
			}
			capture := newImportCapture(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source", roots...)
			spec := loadTestCapture(t, &capture)
			report, err := archive.Import(ctx, spec)
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			require.Equal(t, 2, report.Sources)
			for _, root := range roots {
				require.NoError(t, os.RemoveAll(root.Path))
			}
			report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.ErrorContains(t, err, "another selected source")
			assert.Zero(t, report.Parsed)
			session, err := database.GetSessionFull(ctx, id)
			require.NoError(t, err)
			assert.Nil(t, session)
			report, err = archive.Verify(ctx)
			require.NoError(t, err)
			assert.Equal(t, 2, report.Sources)
			assert.Zero(t, report.Parsed)
		})
	}
}
