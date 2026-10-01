package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Exercise the commands people use to leave a machine behind. Both original
// source directories and the first archive are gone before restored reparsing.
func TestArchiveMoveAndReparse(t *testing.T) {
	ctx := t.Context()
	dataDir := testDataDir(t)
	capture := t.TempDir()
	const nativeID = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	claudeRoot := filepath.Join(capture, "claude")
	codexRoot := filepath.Join(capture, "codex")
	claudePath := filepath.Join(claudeRoot, "projects", "project-a", nativeID+".jsonl")
	codexPath := filepath.Join(codexRoot, "sessions", "rollout-2026-01-01T00-00-00-"+nativeID+".jsonl")
	dbtest.WriteTestFile(t, claudePath, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID("2026-01-01T00:00:01Z", "hello archive", nativeID).
		AddClaudeAssistant("2026-01-01T00:00:02Z", "archive reply").String()))
	dbtest.WriteTestFile(t, codexPath, []byte(testjsonl.NewSessionBuilder().
		AddCodexMeta("2026-01-01T00:00:00Z", nativeID, "/workspace/project-a", "codex_cli_rs").
		AddCodexMessage("2026-01-01T00:00:01Z", "user", "hello archive").
		AddCodexMessage("2026-01-01T00:00:02Z", "assistant", "archive reply").String()))
	dbtest.WriteTestFile(t, filepath.Join(claudeRoot, "file-history", "extra"), []byte("supplemental history"))
	dbtest.WriteTestFile(t, filepath.Join(dataDir, "assets", "example.bin"), []byte("retained asset"))
	dbtest.WriteTestFile(t, filepath.Join(dataDir, "config.json"), []byte(`{"local_machine_name":"origin-device"}`))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {filepath.Join(claudeRoot, "projects")}, parser.AgentCodex: {filepath.Join(codexRoot, "sessions")}},
		Machine:   "origin-device", Ephemeral: true, DisableFilesystemProjectDiscovery: true,
	})
	require.NoError(t, engine.SyncPathsContext(ctx, []string{claudePath, codexPath}))
	engine.Close()
	ids := []string{nativeID, "codex:" + nativeID}
	before := make(map[string][]db.Message)
	for _, id := range ids {
		before[id], err = database.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Len(t, before[id], 2)
	}
	name := "Kept name"
	require.NoError(t, database.RenameSession(ctx, nativeID, &name))
	_, err = database.StarSession(ctx, nativeID)
	require.NoError(t, err)
	_, err = database.PinMessage(ctx, nativeID, before[nativeID][0].ID, nil)
	require.NoError(t, err)
	initialExport, err := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initialExport.Changes, 4)
	wantMessageIDs := make([]string, 0, 4)
	for _, change := range initialExport.Changes {
		wantMessageIDs = append(wantMessageIDs, change.MessageID)
	}
	// A recovery point from an older parser generation must become readable
	// after restore without rediscovering the now-missing original files.
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", db.CurrentDataVersion()-1))
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	installation, err := os.ReadFile(filepath.Join(dataDir, "telemetry-install-id"))
	if os.IsNotExist(err) {
		installation = []byte("019eb791cf7d75c184399ed74c122e04")
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "telemetry-install-id"), installation, 0o600))
	} else {
		require.NoError(t, err)
	}
	spec := rawarchive.ImportSpec{DeviceID: "original-device", Machine: "origin-device", Roots: []rawarchive.RootSpec{
		{ID: "claude-root", Provider: "claude", Path: claudeRoot, OriginalPath: claudeRoot, SessionDirs: []string{"projects"}},
		{ID: "codex-root", Provider: "codex", Path: codexRoot, OriginalPath: codexRoot, SessionDirs: []string{"sessions"}},
	}}
	specPath := filepath.Join(t.TempDir(), "import.json")
	writeSpec := func() {
		b, e := json.Marshal(spec)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(specPath, b, 0o600))
	}
	writeSpec()
	run := func(args ...string) (rawarchive.Report, error) {
		root := newRootCommand()
		var out, diagnostics bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&diagnostics)
		root.SetArgs(append([]string{"archive"}, args...))
		err := root.ExecuteContext(ctx)
		if err != nil {
			return rawarchive.Report{}, err
		}
		var report rawarchive.Report
		require.NoError(t, json.Unmarshal(out.Bytes(), &report), diagnostics.String())
		return report, nil
	}
	report, err := run("import", "--spec", specPath)
	require.NoError(t, err)
	assert.Equal(t, 3, report.Files)
	assert.Equal(t, 2, report.Sources)
	assert.Equal(t, 1, report.Supplemental)
	// A changed mount path is not a changed source identity.
	moved := filepath.Join(t.TempDir(), "capture")
	require.NoError(t, os.Rename(capture, moved))
	spec.Roots[0].Path = filepath.Join(moved, "claude")
	spec.Roots[1].Path = filepath.Join(moved, "codex")
	writeSpec()
	report, err = run("import", "--spec", specPath)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Sources)
	require.NoError(t, os.RemoveAll(moved))
	sourceList, err := executeCommand(newRootCommand(), "archive", "sources")
	require.NoError(t, err)
	assert.Contains(t, sourceList, nativeID)
	backup := filepath.Join(t.TempDir(), "recovery")
	_, err = run("backup", backup)
	require.NoError(t, err)
	t.Chdir(filepath.Dir(backup))
	_, err = run("restore", filepath.Base(backup), filepath.Join(filepath.Base(backup), "nested"))
	require.Error(t, err)
	_, err = rawarchive.VerifyRecovery(ctx, backup)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(dataDir))
	restored := filepath.Join(t.TempDir(), "restored")
	report, err = run("restore", backup, restored)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Sources)
	restoredConfig, err := os.ReadFile(filepath.Join(restored, "config.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"local_machine_name":"origin-device"}`, string(restoredConfig))
	restoredInstallation, err := os.ReadFile(filepath.Join(restored, "telemetry-install-id"))
	require.NoError(t, err)
	assert.Equal(t, installation, restoredInstallation)
	t.Setenv("AGENTSVIEW_DATA_DIR", restored)
	extracted := filepath.Join(t.TempDir(), "native")
	report, err = run("extract", extracted)
	require.NoError(t, err)
	assert.Equal(t, 3, report.Files)
	extra, err := os.ReadFile(filepath.Join(extracted, "claude-root", "file-history", "extra"))
	require.NoError(t, err)
	assert.Equal(t, "supplemental history", string(extra))
	require.NoError(t, os.RemoveAll(extracted))
	report, err = run("reparse", "--all")
	require.NoError(t, err)
	assert.Equal(t, 2, report.Parsed)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(restored, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	assert.False(t, database.NeedsResync())
	check := func() {
		for _, id := range ids {
			session, e := database.GetSessionFull(ctx, id)
			require.NoError(t, e)
			require.NotNil(t, session)
			assert.Equal(t, "origin-device", session.Machine)
			messages, e := database.GetAllMessages(ctx, id)
			require.NoError(t, e)
			require.Len(t, messages, 2)
			for i := range messages {
				assert.Equal(t, before[id][i].Content, messages[i].Content)
			}
		}
		session, e := database.GetSessionFull(ctx, nativeID)
		require.NoError(t, e)
		assert.Equal(t, &name, session.DisplayName)
		stars, e := database.ListStarredSessionIDs(ctx)
		require.NoError(t, e)
		assert.Equal(t, []string{nativeID}, stars)
		pins, e := database.GetPinnedMessageIDs(ctx, nativeID)
		require.NoError(t, e)
		pinnedMessages, e := database.GetAllMessages(ctx, nativeID)
		require.NoError(t, e)
		assert.Contains(t, pins, pinnedMessages[0].ID)
		current, e := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
		require.NoError(t, e)
		var gotMessageIDs []string
		for _, change := range current.Changes {
			if change.MessageID != "" && (change.SessionID == nativeID || change.SessionID == "codex:"+nativeID) {
				gotMessageIDs = append(gotMessageIDs, change.MessageID)
			}
		}
		assert.ElementsMatch(t, wantMessageIDs, gotMessageIDs)
		sources, e := database.ListRawArchiveSources(ctx, "", 10)
		require.NoError(t, e)
		assert.Len(t, sources, 2)
	}
	check()
	asset, err := os.ReadFile(filepath.Join(restored, "assets", "example.bin"))
	require.NoError(t, err)
	assert.Equal(t, "retained asset", string(asset))
	// A parser-version rebuild with no live source retains archived history and
	// acceptance records. It must not open and reparse the raw vault on startup.
	engine = syncer.NewEngine(ctx, database, syncer.EngineConfig{Ephemeral: true, Machine: "destination-device", AgentDirs: map[parser.AgentType][]string{}})
	stats, err := engine.ResyncAllWithOptions(ctx, nil, syncer.RebuildOptions{})
	engine.Close()
	require.NoError(t, err)
	assert.True(t, stats.Aborted)
	check()
	// Once a live source exists, exercise the actual replacement database swap.
	liveRoot := t.TempDir()
	newID := "019eb791-cf7d-75c1-8439-9ed74c122e03"
	dbtest.WriteTestFile(t, filepath.Join(liveRoot, "project-b", newID+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-02-01T00:00:00Z", "new machine session", newID).String()))
	engine = syncer.NewEngine(ctx, database, syncer.EngineConfig{Ephemeral: true, Machine: "destination-device", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {liveRoot}}})
	stats, err = engine.ResyncAllWithOptions(ctx, nil, syncer.RebuildOptions{})
	engine.Close()
	require.NoError(t, err)
	require.True(t, stats.ArchiveRebuilt)
	check()
	require.NoError(t, database.Close())
	// The accepted ledger by itself is not a backup. Missing raw bytes must be
	// detected, and a rejected reparse must leave browsable history unchanged.
	require.NoError(t, os.RemoveAll(filepath.Join(restored, rawarchive.Directory)))
	_, err = run("verify")
	require.Error(t, err)
	_, err = run("reparse", "--all")
	require.Error(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(restored, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	check()
	sources, err := database.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	var failures int
	for _, source := range sources {
		if source.ParseError != "" {
			failures++
		}
	}
	assert.Equal(t, 1, failures)
}

func TestArchiveRestorePreservesCodexSharedPath(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	path := filepath.Join(data, "sessions.db")
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	sourcePath := "/original/rollout.jsonl"
	for _, id := range []string{"codex:trashed-parent", "codex:active-fork"} {
		require.NoError(t, database.UpsertSession(ctx, db.Session{ID: id, Agent: "codex", Project: "example", Machine: "original", FilePath: &sourcePath, MessageCount: 1}))
		require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: id}}))
	}
	require.NoError(t, database.SoftDeleteSession(ctx, "codex:trashed-parent"))
	archive, err := rawarchive.Open(ctx, database, data, nil)
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", db.CurrentDataVersion()-1))
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	backup := filepath.Join(t.TempDir(), "backup")
	_, err = rawarchive.Backup(ctx, path, data, backup)
	require.NoError(t, err)
	restored := filepath.Join(t.TempDir(), "restored")
	_, err = restoreRawArchive(ctx, backup, restored, nil)
	require.NoError(t, err)
	got, err := db.OpenReadOnly(ctx, filepath.Join(restored, "sessions.db"))
	require.NoError(t, err)
	defer got.Close()
	session, err := got.GetSessionFull(ctx, "codex:active-fork")
	require.NoError(t, err)
	require.NotNil(t, session, "archive-only restore must retain the active row beside its historical trashed same-path row")
	assert.False(t, got.NeedsResync())
	for _, id := range []string{"codex:trashed-parent", "codex:active-fork"} {
		messages, err := got.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.Equal(t, id, messages[0].Content)
	}
}
