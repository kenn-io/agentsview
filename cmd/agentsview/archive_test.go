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
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank"
)

func TestArchiveExtractCaptureAfterRestore(t *testing.T) {
	ctx := t.Context()
	run := func(args ...string) (string, error) {
		root := newRootCommand()
		var out, diagnostics bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&diagnostics)
		root.SetArgs(args)
		err := root.ExecuteContext(ctx)
		return out.String(), err
	}
	sourceData := testDataDir(t)
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(sourceData, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	provider := t.TempDir()
	const nativeID = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	rel := filepath.Join("projects", "project-a", nativeID+".jsonl")
	firstBytes := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "first capture", nativeID).String()
	secondBytes := firstBytes + testjsonl.NewSessionBuilder().AddClaudeAssistant("2026-01-01T00:00:01Z", "later reply").String()
	dbtest.WriteTestFile(t, filepath.Join(provider, rel), []byte(firstBytes))
	dbtest.WriteTestFile(t, filepath.Join(provider, "file-history", "removed-later"), []byte("first-only history"))
	opts := rawarchive.CaptureOptions{
		DataDir: sourceData, Destination: filepath.Join(t.TempDir(), "first"),
		Roots:    []rawarchive.RootSpec{{Provider: "claude", Path: provider}, {Provider: "files", Path: t.TempDir()}},
		Settings: rawarchive.RecoverySettings{LocalMachineName: "source-device"},
	}
	first, err := rawarchive.Capture(ctx, opts)
	require.NoError(t, err)
	firstDir := opts.Destination
	dbtest.WriteTestFile(t, filepath.Join(provider, rel), []byte(secondBytes))
	require.NoError(t, os.Remove(filepath.Join(provider, "file-history", "removed-later")))
	dbtest.WriteTestFile(t, filepath.Join(provider, "file-history", "added-later"), []byte("second-only history"))
	opts.IdentityFrom = filepath.Join(firstDir, "capture.json")
	opts.Destination = filepath.Join(t.TempDir(), "second")
	second, err := rawarchive.Capture(ctx, opts)
	require.NoError(t, err)
	require.NotEqual(t, first.CaptureID, second.CaptureID)
	archiveData := filepath.Join(t.TempDir(), "archive")
	t.Setenv("AGENTSVIEW_DATA_DIR", archiveData)
	_, err = run("archive", "import", "--seed", "--spec", filepath.Join(firstDir, "capture.json"))
	require.NoError(t, err)
	_, err = run("archive", "import", "--spec", filepath.Join(opts.Destination, "capture.json"))
	require.ErrorContains(t, err, "coverage gaps")
	repository := filepath.Join(t.TempDir(), "backup")
	output, err := run("archive", "backup", repository)
	require.NoError(t, err)
	var backup rawarchive.Report
	require.NoError(t, json.Unmarshal([]byte(output), &backup))
	for _, path := range []string{sourceData, provider, opts.Roots[1].Path, firstDir, opts.Destination, archiveData} {
		require.NoError(t, os.RemoveAll(path))
	}
	restored := filepath.Join(t.TempDir(), "restored")
	_, err = run("archive", "restore", repository, restored, "--snapshot", backup.SnapshotID)
	require.NoError(t, err)
	t.Setenv("AGENTSVIEW_DATA_DIR", restored)
	for _, tc := range []struct {
		capture                         rawarchive.CaptureDescriptor
		content, present, absent, extra string
	}{
		{first, firstBytes, "removed-later", "added-later", "first-only history"},
		{second, secondBytes, "added-later", "removed-later", "second-only history"},
	} {
		t.Run(tc.capture.CaptureID, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "capture")
			output, err := run("archive", "extract", target, "--capture", tc.capture.CaptureID)
			require.NoError(t, err)
			var report rawarchive.Report
			require.NoError(t, json.Unmarshal([]byte(output), &report))
			assert.Equal(t, tc.capture.CaptureID, report.CaptureID)
			descriptor := filepath.Join(target, "capture.json")
			got, err := rawarchive.LoadCapture(ctx, descriptor)
			require.NoError(t, err, "the recovered package must validate without any original files")
			assert.Equal(t, tc.capture.CaptureID, got.CaptureID)
			assert.Equal(t, tc.capture.Source, got.Source)
			root := filepath.Join(target, got.Source.Roots[0].Path)
			content, err := os.ReadFile(filepath.Join(root, rel))
			require.NoError(t, err)
			assert.Equal(t, tc.content, string(content))
			extra, err := os.ReadFile(filepath.Join(root, "file-history", tc.present))
			require.NoError(t, err)
			assert.Equal(t, tc.extra, string(extra))
			assert.NoFileExists(t, filepath.Join(root, "file-history", tc.absent))
			// A conflicting capture remains usable as a portable source in its own
			// archive; extracting it must not advance the first archive's head.
			t.Setenv("AGENTSVIEW_DATA_DIR", filepath.Join(t.TempDir(), "seed"))
			_, err = run("archive", "import", "--seed", "--spec", descriptor)
			require.NoError(t, err)
		})
	}
	_, err = run("archive", "reparse", "--all")
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(restored, "sessions.db"))
	require.NoError(t, err)
	messages, err := database.GetAllMessages(ctx, nativeID)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	require.Len(t, messages, 1, "extraction must not advance the accepted source")
	assert.Equal(t, "first capture", messages[0].Content)
	for _, args := range [][]string{{"--capture", "missing"}} {
		target := filepath.Join(t.TempDir(), "rejected")
		_, err = run(append([]string{"archive", "extract", target}, args...)...)
		require.Error(t, err)
		assert.NoDirExists(t, target)
	}
	existing := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(existing, "keep"), []byte("existing data"))
	_, err = run("archive", "extract", existing, "--capture", first.CaptureID)
	require.Error(t, err)
	kept, err := os.ReadFile(filepath.Join(existing, "keep"))
	require.NoError(t, err)
	assert.Equal(t, "existing data", string(kept))
}

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
	dbtest.WriteTestFile(t, filepath.Join(dataDir, "config.json"), []byte(`{"local_machine_name":"origin-device","auth_token":"original-test-token","cursor_secret":"b3JpZ2luYWwtdGVzdC1zZWNyZXQ="}`))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	installation, err := os.ReadFile(filepath.Join(dataDir, "telemetry-install-id"))
	if os.IsNotExist(err) {
		installation = []byte("019eb791cf7d75c184399ed74c122e04")
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "telemetry-install-id"), installation, 0o600))
	} else {
		require.NoError(t, err)
	}
	engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {filepath.Join(claudeRoot, "projects")}, parser.AgentCodex: {filepath.Join(codexRoot, "sessions")}},
		Machine:   string(bytes.TrimSpace(installation)), Ephemeral: true, DisableFilesystemProjectDiscovery: true,
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
	archiveID, err := database.GetOrCreateArchiveID(ctx)
	require.NoError(t, err)
	originalGeneration, err := database.GetOrCreateDatabaseID(ctx)
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
	closed := filepath.Join(t.TempDir(), "capture")
	captured, err := rawarchive.Capture(ctx, rawarchive.CaptureOptions{
		Destination: closed, DataDir: dataDir,
		Roots:    []rawarchive.RootSpec{{Provider: "claude", Path: claudeRoot}, {Provider: "codex", Path: codexRoot}},
		Settings: rawarchive.RecoverySettings{LocalMachineName: "origin-device"},
	})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(capture))
	require.NoError(t, os.RemoveAll(dataDir))
	dataDir = filepath.Join(t.TempDir(), "seed")
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	capture = closed
	specPath := filepath.Join(capture, "capture.json")
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
	report, err := run("import", "--seed", "--spec", specPath)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, report.Files, 3)
	assert.Equal(t, 2, report.Sources)
	assert.GreaterOrEqual(t, report.Supplemental, 1)
	// A changed mount path is not a changed source identity.
	moved := filepath.Join(t.TempDir(), "capture")
	require.NoError(t, os.Rename(capture, moved))
	specPath = filepath.Join(moved, "capture.json")
	report, err = run("import", "--spec", specPath)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Sources)
	require.NoError(t, os.RemoveAll(moved))
	sourceList, err := executeCommand(newRootCommand(), "archive", "sources")
	require.NoError(t, err)
	assert.Contains(t, sourceList, nativeID)
	for line := range bytes.SplitSeq(bytes.TrimSpace([]byte(sourceList)), []byte("\n")) {
		var source map[string]any
		require.NoError(t, json.Unmarshal(line, &source))
		assert.Len(t, source, 6)
		for _, key := range []string{"manifest_id", "root_id", "source_key", "original_path", "parse_error", "processing_version"} {
			assert.Contains(t, source, key)
		}
		assert.NotEmpty(t, source["manifest_id"])
	}
	backup := filepath.Join(t.TempDir(), "recovery")
	report, err = run("backup", backup)
	require.NoError(t, err)
	repository, err := docbank.OpenBackupRepository(backup)
	require.NoError(t, err, "archive backup must produce a portable Docbank repository")
	snapshots, err := repository.Snapshots()
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	first := snapshots[0].ID
	assert.Equal(t, first, report.SnapshotID)
	assert.Equal(t, repository.ID(), report.RepositoryID)
	assert.Equal(t, snapshots[0].MinReaderVersion, report.MinReaderVersion)
	assert.NotEmpty(t, report.Excluded)
	verified, err := executeCommand(newRootCommand(), "archive", "verify", "--repository", backup, "--snapshot", first)
	require.NoError(t, err)
	var verification struct {
		Snapshots    []string `json:"snapshots"`
		BlobsChecked int64    `json:"blobs_checked"`
		BytesRead    int64    `json:"bytes_read"`
		Problems     []any    `json:"problems"`
	}
	require.NoError(t, json.Unmarshal([]byte(verified), &verification))
	assert.Equal(t, []string{first}, verification.Snapshots)
	assert.Positive(t, verification.BlobsChecked)
	assert.Positive(t, verification.BytesRead)
	assert.Empty(t, verification.Problems)
	dbtest.WriteTestFile(t, filepath.Join(dataDir, "assets", "example.bin"), []byte("newer asset"))
	_, err = run("backup", backup)
	require.NoError(t, err)
	snapshots, err = repository.Snapshots()
	require.NoError(t, err)
	require.Len(t, snapshots, 2)
	t.Chdir(filepath.Dir(backup))
	_, err = run("restore", filepath.Base(backup), filepath.Join(filepath.Base(backup), "nested"), "--snapshot", first)
	require.Error(t, err)
	_, err = rawarchive.VerifyRecovery(ctx, backup, first)
	require.NoError(t, err)
	_, err = run("restore", backup, filepath.Join(t.TempDir(), "missing-selector"))
	require.ErrorContains(t, err, "--snapshot is required")
	existing := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(existing, "keep"), []byte("existing data"))
	_, err = run("restore", backup, existing, "--snapshot", first)
	require.ErrorContains(t, err, "requires a new destination")
	kept, err := os.ReadFile(filepath.Join(existing, "keep"))
	require.NoError(t, err)
	assert.Equal(t, "existing data", string(kept))
	require.NoError(t, os.RemoveAll(dataDir))
	restored := filepath.Join(t.TempDir(), "restored")
	report, err = run("restore", backup, restored, "--snapshot", first)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Sources)
	t.Setenv("AGENTSVIEW_DATA_DIR", restored)
	restoredConfig, err := config.LoadReadOnly()
	require.NoError(t, err)
	assert.Equal(t, "origin-device", restoredConfig.LocalMachineName)
	assert.Equal(t, "127.0.0.1", restoredConfig.Host)
	assert.True(t, restoredConfig.RequireAuth)
	assert.NotEmpty(t, restoredConfig.AuthToken)
	assert.NotEqual(t, "original-test-token", restoredConfig.AuthToken)
	assert.NotEqual(t, "b3JpZ2luYWwtdGVzdC1zZWNyZXQ=", restoredConfig.CursorSecret)
	restoredInstallation, err := os.ReadFile(filepath.Join(restored, "telemetry-install-id"))
	require.NoError(t, err)
	assert.Equal(t, bytes.TrimSpace(installation), bytes.TrimSpace(restoredInstallation))
	t.Setenv("AGENTSVIEW_DATA_DIR", restored)
	extracted := filepath.Join(t.TempDir(), "native")
	report, err = run("extract", extracted)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, report.Files, 3)
	extraPaths, err := filepath.Glob(filepath.Join(extracted, "*", "file-history", "extra"))
	require.NoError(t, err)
	require.Len(t, extraPaths, 1)
	extra, err := os.ReadFile(extraPaths[0])
	require.NoError(t, err)
	assert.Equal(t, "supplemental history", string(extra))
	evidencePaths, err := filepath.Glob(filepath.Join(extracted, "*", captured.CaptureID, "inventory.json"))
	require.NoError(t, err)
	assert.Len(t, evidencePaths, 1)
	require.NoError(t, os.RemoveAll(extracted))
	report, err = run("reparse", "--all")
	require.NoError(t, err)
	assert.Equal(t, 2, report.Parsed)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(restored, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	restoredArchiveID, err := database.GetOrCreateArchiveID(ctx)
	require.NoError(t, err)
	assert.Equal(t, archiveID, restoredArchiveID)
	rebuiltGeneration, err := database.GetOrCreateDatabaseID(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, originalGeneration, rebuiltGeneration)
	assert.False(t, database.NeedsResync())
	check := func() {
		for _, id := range ids {
			session, e := database.GetSessionFull(ctx, id)
			require.NoError(t, e)
			require.NotNil(t, session)
			assert.Equal(t, string(bytes.TrimSpace(installation)), session.Machine)
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
	assert.True(t, stats.ArchiveRebuilt)
	check()
	require.NoError(t, database.Close())
	// The accepted ledger by itself is not a backup. Missing raw bytes must be
	// detected, and a rejected reparse must leave browsable history unchanged.
	require.NoError(t, os.RemoveAll(filepath.Join(restored, rawarchive.Directory)))
	_, err = run("verify")
	require.Error(t, err)
	_, err = run("extract", extracted, "--capture", captured.CaptureID)
	require.Error(t, err)
	assert.NoDirExists(t, extracted)
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
	originalGeneration, err := database.GetOrCreateDatabaseID(ctx)
	require.NoError(t, err)
	require.NoError(t, database.EnableArchiveOnly(ctx))
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
	database, err = db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	archive, err = rawarchive.Open(ctx, database, data, nil)
	require.NoError(t, err)
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("019eb791cf7d75c184399ed74c122e04"))
	recovery, err := archive.Backup(ctx, backup, rawarchive.RecoverySettings{LocalMachineName: "original"}, "test")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	restored := filepath.Join(t.TempDir(), "restored")
	_, err = rawarchive.Restore(ctx, backup, recovery.SnapshotID, restored, nil)
	require.NoError(t, err)
	got, err := db.OpenReadOnly(ctx, filepath.Join(restored, "sessions.db"))
	require.NoError(t, err)
	defer got.Close()
	rebuiltGeneration, err := got.GetOrCreateDatabaseID(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, originalGeneration, rebuiltGeneration)
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
