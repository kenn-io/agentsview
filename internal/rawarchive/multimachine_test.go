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

func TestMultipleMachinesKeepDistinctSessionIdentities(t *testing.T) {
	for _, machine := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "known-old-label", "unowned-label"} {
		t.Run(machine, func(t *testing.T) {
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			path := filepath.Join(root, "project", id+".jsonl")
			dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "seed history", id).String()))
			if machine == "known-old-label" {
				require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "prior-session", Agent: "claude", Project: "project-a", Machine: machine}))
				require.NoError(t, database.AdoptMachineIdentity(ctx, owner, []string{machine}))
			}
			engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: machine, Ephemeral: true, DisableFilesystemProjectDiscovery: true})
			require.NoError(t, engine.SyncPathsContext(ctx, []string{path}))
			engine.Close()
			before, err := database.GetAllMessages(ctx, id)
			require.NoError(t, err)
			require.Len(t, before, 1)
			beforeExport, err := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, beforeExport.Changes, 1)
			require.NoError(t, database.EnableArchiveOnly(ctx))
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			defer archive.Close()
			capture := newImportCapture(t, owner, "same-label", RootSpec{Provider: "claude", Path: root})
			_, err = archive.Import(ctx, loadTestCapture(t, &capture))
			require.NoError(t, err)
			dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "foreign history", id).String()))
			capture = newImportCapture(t, foreign, "same-label", RootSpec{Provider: "claude", Path: root})
			report, err := archive.Import(ctx, loadTestCapture(t, &capture))
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			capture.Settings.LocalMachineName = "renamed-label"
			report, err = archive.Import(ctx, loadTestCapture(t, &capture))
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			labels, err := database.GetMachineLabels(ctx)
			require.NoError(t, err)
			assert.Equal(t, "renamed-label", labels[foreign])
			require.NoError(t, os.RemoveAll(root))
			report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			if machine == "unowned-label" {
				require.ErrorContains(t, err, "identity conflicts")
				unchanged, err := database.GetAllMessages(ctx, id)
				require.NoError(t, err)
				assert.Equal(t, before, unchanged)
				foreignSession, err := database.GetSessionFull(ctx, foreign+"~"+id)
				require.NoError(t, err)
				assert.Nil(t, foreignSession, "a rejected batch never publishes its foreign rows")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 2, report.Parsed)
			after, err := database.GetAllMessages(ctx, id)
			require.NoError(t, err)
			require.Len(t, after, 1)
			assert.Equal(t, before[0].Content, after[0].Content)
			afterExport, err := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			var seedMessageIDs []string
			for _, c := range afterExport.Changes {
				if c.SessionID == id && c.Type == "message" {
					seedMessageIDs = append(seedMessageIDs, c.MessageID)
				}
			}
			assert.Equal(t, []string{beforeExport.Changes[0].MessageID}, seedMessageIDs, "seed archive message identities survive")
			messages, err := database.GetAllMessages(ctx, foreign+"~"+id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "foreign history", messages[0].Content)
			session, err := database.GetSessionFull(ctx, foreign+"~"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, foreign, session.Machine)
			// A second parse must resolve to the same durable observations.
			_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err)
			repeated, err := database.GetAllMessages(ctx, foreign+"~"+id)
			require.NoError(t, err)
			require.Len(t, repeated, 1)
			assert.Equal(t, messages[0].SessionID, repeated[0].SessionID)
			assert.Equal(t, messages[0].Content, repeated[0].Content)
			// Full resync copies source identity and suppression state into a fresh DB.
			fresh := dbtest.OpenTestDB(t)
			require.NoError(t, fresh.CopySyncStateFrom(database.Path()))
			var mappings int
			require.NoError(t, fresh.Reader().QueryRow(ctx, "SELECT count(*) FROM raw_archive_sessions").Scan(&mappings))
			assert.Equal(t, 2, mappings)
		})
	}
}

func TestForeignCaptureHonorsSourceDeletions(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const trashed = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const deleted = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	opts := captureFixture(t)
	dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"), []byte(foreign))
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, source.UpsertSession(ctx, db.Session{ID: trashed, Agent: "claude", Project: "project-a", Machine: foreign, FilePath: new(filepath.Join(opts.Roots[0].Path, "project-a", trashed+".jsonl"))}))
	require.NoError(t, source.SoftDeleteSession(ctx, trashed))
	require.NoError(t, source.UpsertSession(ctx, db.Session{ID: deleted, Agent: "claude", Project: "project-a", Machine: foreign}))
	require.NoError(t, source.DeleteSession(ctx, deleted))
	require.NoError(t, source.Close())
	for _, id := range []string{trashed, deleted} {
		dbtest.WriteTestFile(t, filepath.Join(opts.Roots[0].Path, "project-a", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "excluded history", id).String()))
	}
	require.NoError(t, os.Remove(filepath.Join(opts.Roots[0].Path, "project-a", "saved.jsonl")))
	_, err = Capture(ctx, opts)
	require.NoError(t, err)
	spec, err := LoadImportSpec(ctx, filepath.Join(opts.Destination, "capture.json"))
	require.NoError(t, err)
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	report, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, 2, report.Parsed)
	assert.Equal(t, 2, report.Suppressed)
	copied := dbtest.OpenTestDB(t)
	require.NoError(t, copied.CopySyncStateFrom(database.Path()))
	deletions, err := copied.RawArchiveSuppressions(ctx, foreign)
	require.NoError(t, err)
	assert.Equal(t, []db.RawArchiveSuppression{
		{ParserID: trashed, Provider: "claude", Kind: "trashed"},
		{ParserID: deleted, Kind: "deleted"},
	}, deletions)
	for _, id := range []string{trashed, deleted} {
		session, err := database.GetSessionFull(ctx, foreign+"~"+id)
		require.NoError(t, err)
		assert.Nil(t, session)
	}
	// The same deletion evidence survives seed recovery and explicit reparse.
	seedPath := filepath.Join(t.TempDir(), "seed")
	_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), seedPath, nil)
	require.NoError(t, err)
	seeded, err := db.OpenIsolatedContext(ctx, filepath.Join(seedPath, "sessions.db"))
	require.NoError(t, err)
	defer seeded.Close()
	seedArchive, err := Open(ctx, seeded, seedPath, nil)
	require.NoError(t, err)
	defer seedArchive.Close()
	seedReport, err := seedArchive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, 2, seedReport.Suppressed)
	assert.True(t, seeded.IsSessionTrashed(ctx, trashed))
	assert.True(t, seeded.IsSessionExcluded(ctx, deleted))
	// Suppressed content remains recoverable in the raw vault.
	_, err = archive.Extract(ctx, filepath.Join(t.TempDir(), "extracted"), "")
	require.NoError(t, err)
}

func TestForeignCodexParentCanBeParsedLater(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const parent = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const child = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	parentPath := filepath.Join(root, "rollout-2026-01-01T00-00-00-"+parent+".jsonl")
	childPath := filepath.Join(root, "rollout-2026-01-01T01-00-00-"+child+".jsonl")
	dbtest.WriteTestFile(t, parentPath, []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", parent, "/workspace/project-a", "codex_cli_rs").AddCodexMessage("2026-01-01T00:00:01Z", "user", "parent history").String()))
	dbtest.WriteTestFile(t, childPath, []byte(testjsonl.JoinJSONL(testjsonl.CodexSubagentSessionMetaJSON(child, parent, "/workspace/project-a", "codex_cli_rs", "2026-01-01T01:00:00Z"), testjsonl.CodexMsgJSON("user", "child history", "2026-01-01T01:00:01Z"))))
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	capture := newImportCapture(t, foreign, "foreign", RootSpec{Provider: "codex", Path: root})
	report, err := archive.Import(ctx, loadTestCapture(t, &capture))
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	sources, err := database.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	var parentManifest, childManifest string
	for _, s := range sources {
		if s.OriginalPath == childPath {
			childManifest = s.ManifestID
		} else {
			parentManifest = s.ManifestID
		}
	}
	for _, manifest := range []string{childManifest, parentManifest} {
		_, err = archive.Reparse(ctx, ReparseOptions{ManifestIDs: []string{manifest}, ScratchBytes: 1 << 20})
		require.NoError(t, err)
		session, err := database.GetSessionFull(ctx, foreign+"~codex:"+child)
		require.NoError(t, err)
		require.NotNil(t, session)
		require.NotNil(t, session.ParentSessionID)
		assert.Equal(t, foreign+"~codex:"+parent, *session.ParentSessionID)
	}
	session, err := database.GetSessionFull(ctx, foreign+"~codex:"+parent)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, foreign, session.Machine)
}

func TestForeignClaudeForkSuppressionKeepsOtherBranch(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	opts := captureFixture(t)
	dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"), []byte(foreign))
	require.NoError(t, os.Remove(filepath.Join(opts.Roots[0].Path, "project-a", "saved.jsonl")))
	path := filepath.Join(opts.Roots[0].Path, "project-a", id+".jsonl")
	// A distant branch in one physical file has its own parser ID. Suppress
	// only that branch, not everything sharing the provider's source ID.
	b := testjsonl.NewSessionBuilder().AddClaudeUserWithUUID("2026-01-01T00:00:00Z", "start", "a", "").AddClaudeAssistantWithUUID("2026-01-01T00:00:01Z", "answer", "b", "a").
		AddClaudeUserWithUUID("2026-01-01T00:00:02Z", "q1", "c", "b").AddClaudeAssistantWithUUID("2026-01-01T00:00:03Z", "a1", "d", "c").
		AddClaudeUserWithUUID("2026-01-01T00:00:04Z", "q2", "e", "d").AddClaudeAssistantWithUUID("2026-01-01T00:00:05Z", "a2", "f", "e").
		AddClaudeUserWithUUID("2026-01-01T00:00:06Z", "q3", "g", "f").AddClaudeAssistantWithUUID("2026-01-01T00:00:07Z", "a3", "h", "g").
		AddClaudeUserWithUUID("2026-01-01T00:00:08Z", "q4", "i", "h").AddClaudeAssistantWithUUID("2026-01-01T00:00:09Z", "a4", "j", "i").
		AddClaudeUserWithUUID("2026-01-01T00:01:00Z", "branch", "fork-u1", "b").AddClaudeAssistantWithUUID("2026-01-01T00:01:01Z", "branch answer", "fork-a1", "fork-u1")
	dbtest.WriteTestFile(t, path, []byte(b.String()))
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	engine := syncer.NewEngine(ctx, source, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {opts.Roots[0].Path}}, Machine: foreign, Ephemeral: true, DisableFilesystemProjectDiscovery: true})
	require.NoError(t, engine.SyncPathsContext(ctx, []string{path}))
	engine.Close()
	fork, err := source.GetSessionFull(ctx, id+"-fork-u1")
	require.NoError(t, err)
	require.NotNil(t, fork)
	require.NoError(t, source.SoftDeleteSession(ctx, id+"-fork-u1"))
	require.NoError(t, source.Close())
	_, err = Capture(ctx, opts)
	require.NoError(t, err)
	spec, err := LoadImportSpec(ctx, filepath.Join(opts.Destination, "capture.json"))
	require.NoError(t, err)
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	_, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	report, err := archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Suppressed)
	main, err := database.GetSessionFull(ctx, foreign+"~"+id)
	require.NoError(t, err)
	require.NotNil(t, main)
	fork, err = database.GetSessionFull(ctx, foreign+"~"+id+"-fork-u1")
	require.NoError(t, err)
	assert.Nil(t, fork)
	var mapped int
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT count(*) FROM raw_archive_sessions").Scan(&mapped))
	assert.Equal(t, 2, mapped, "each branch keeps its own mapping even when suppressed")
}
