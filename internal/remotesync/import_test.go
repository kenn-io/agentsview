package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestPreparedHTTPSyncRebuildContributor(t *testing.T) {
	assert := assert.New(t)
	parentRequire := require.New(t)

	const (
		sessionID   = "session"
		remoteDir   = "/remote"
		remoteFile  = "/remote/path/session.jsonl"
		skippedDir  = "/remote-skips"
		skippedFile = "/remote-skips/path/intentionally-empty.jsonl"
	)
	sessionBody := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(
		"2024-01-01T00:00:00Z", "remote rebuild searchable", sessionID,
	).String()
	usageBody := testjsonl.ClaudeUserJSON(
		"<command-name>/usage</command-name>\n"+
			"<command-message>usage</command-message>\n"+
			"<command-args></command-args>",
		"2024-01-01T00:01:00Z",
	)
	files := map[string]string{
		remoteFile:  sessionBody,
		skippedFile: usageBody,
	}
	mtime := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	manifest := Manifest{Files: []ManifestEntry{
		{Path: remoteFile, Size: int64(len(sessionBody)), MtimeNS: mtime.UnixNano()},
		{Path: skippedFile, Size: int64(len(usageBody)), MtimeNS: mtime.UnixNano()},
	}}
	archive := func(t *testing.T) []byte {
		require := require.New(t)

		t.Helper()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, path := range []string{remoteFile, skippedFile} {
			name, err := safeRemotePathArchiveName(path)
			require.NoError(err)
			body := files[path]
			require.NoError(tw.WriteHeader(&tar.Header{
				Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mtime,
			}))
			_, err = tw.Write([]byte(body))
			require.NoError(err)
		}
		require.NoError(tw.Close())
		return buf.Bytes()
	}(t)
	targets := TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {remoteDir, skippedDir},
	}}
	targetsJSON, err := json.Marshal(targets)
	parentRequire.NoError(err)
	manifestJSON, err := json.Marshal(manifest)
	parentRequire.NoError(err)
	server := newCurrentProtocolServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(targetsJSON)
		case "/api/v1/remote-sync/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifestJSON)
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "active.db"))
	parentRequire.NoError(err)
	t.Cleanup(func() { parentRequire.NoError(database.Close()) })
	parentRequire.NoError(database.ReplaceRemoteSkippedFiles("devbox", map[string]int64{
		remoteFile: mtime.UnixNano(),
	}))
	hs := HTTPSync{
		Host: "devbox", URL: server.URL, DataDir: t.TempDir(), DB: database,
	}
	prepared, err := hs.Prepare(t.Context())
	parentRequire.NoError(err)
	t.Cleanup(func() { parentRequire.NoError(prepared.Close()) })
	assert.FileExists(remappedRemotePath(prepared.Root(), remoteFile))
	contributor, err := prepared.RebuildContributor()
	parentRequire.NoError(err)

	localEngine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
	t.Cleanup(localEngine.Close)
	stats, err := localEngine.ResyncAllWithOptions(
		t.Context(), nil,
		syncpkg.RebuildOptions{Contributors: []syncpkg.RebuildContributor{contributor}},
	)
	parentRequire.NoError(err)
	assert.False(stats.Aborted)
	journalPath := mirrorJournalPath(MirrorDir(hs.DataDir, hs.Host))
	assert.FileExists(journalPath,
		"AfterSync runs before swap and must retain the journal")
	parentRequire.NotNil(prepared.mirrorImport)
	assert.Equal(JournalPendingSwap, prepared.mirrorImport.outcome)
	parentRequire.NoError(prepared.Commit())
	assert.NoFileExists(journalPath)
	parentRequire.NoError(prepared.Commit(), "post-swap commit is idempotent")

	full, err := database.GetSessionFull(t.Context(), "devbox~"+sessionID)
	parentRequire.NoError(err)
	parentRequire.NotNil(full)
	assert.Equal("devbox", full.Machine)
	parentRequire.NotNil(full.FilePath)
	assert.Equal("devbox:"+remoteFile, *full.FilePath)
	messages, err := database.GetMessages(
		t.Context(), "devbox~"+sessionID, 0, 10, true,
	)
	parentRequire.NoError(err)
	parentRequire.Len(messages, 1)
	assert.Equal("remote rebuild searchable", messages[0].Content)
	search, err := database.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "searchable", Limit: 10, IncludeOneShot: true,
	})
	parentRequire.NoError(err)
	parentRequire.Len(search.Matches, 1)
	assert.Equal("devbox~"+sessionID, search.Matches[0].SessionID)
	remoteCache, err := database.LoadRemoteSkippedFiles("devbox")
	parentRequire.NoError(err)
	parentRequire.Len(remoteCache, 1)
	for cachedPath := range remoteCache {
		assert.True(strings.HasPrefix(
			cachedPath, skippedFile+"?agent=claude?source_hash=",
		), "rowless Claude skips must persist their content hash")
	}
	assert.NotContains(remoteCache, remoteFile,
		"a full contributor must not load the active database's stale host cache")

	prepared.targets = TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {skippedDir},
	}}
	activeStats, err := prepared.ImportActive(t.Context())
	parentRequire.NoError(err)
	assert.Equal(0, activeStats.Failed)
	assert.Equal(0, activeStats.Skipped,
		"reusing a prepared bootstrap import must preserve its full-parse scope")
}

func TestPreparedHTTPSyncRebuildOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		cachePersist bool
		want         JournalOutcome
	}{
		{name: "cancelled", err: context.Canceled, want: JournalCancelled},
		{name: "cache persistence", cachePersist: true, want: JournalCachePersistFailed},
		{name: "processing", err: errors.New("processing sentinel"), want: JournalProcessingFailures},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			remote := newMirrorTestRemote(t)
			remote.writeSession(t, "session.jsonl",
				time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC), "rebuild outcome")
			database, hs := newMirrorSync(t, remote, t.TempDir())
			prepared, err := hs.Prepare(t.Context())
			require.NoError(err)
			t.Cleanup(func() { require.NoError(prepared.Close()) })
			contributor, err := prepared.RebuildContributor()
			require.NoError(err)

			outcomeErr := tc.err
			if tc.cachePersist {
				engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
				t.Cleanup(engine.Close)
				replacement, openErr := db.Open(filepath.Join(t.TempDir(), "replacement.db"))
				require.NoError(openErr)
				require.NoError(replacement.Close())
				outcomeErr = contributor.AfterSync(engine, replacement)
				require.Error(outcomeErr)
			}
			contributor.Finished(syncpkg.SyncStats{}, outcomeErr)
			assert.Equal(tc.want, prepared.mirrorImport.outcome)
			assert.Equal(tc.want, prepared.mirrorImport.pending.Stats.JournalOutcome)
		})
	}
}

func TestPreparedHTTPSyncDeferredRebuildIsNotCommitReady(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl",
		time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC), "deferred rebuild")
	_, hs := newMirrorSync(t, remote, t.TempDir())
	prepared, err := hs.Prepare(t.Context())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(prepared.Close()) })
	contributor, err := prepared.RebuildContributor()
	require.NoError(err)

	contributor.Finished(syncpkg.SyncStats{Deferred: 1}, nil)
	assert.False(prepared.commitReady)
	assert.Equal(JournalProcessingFailures, prepared.mirrorImport.outcome)
	require.ErrorContains(prepared.Commit(), "not ready to commit")
	assert.FileExists(mirrorJournalPath(MirrorDir(hs.DataDir, hs.Host)))
}

func TestPreparedHTTPSyncRebuildRetirementFailureRecordsDuration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl",
		time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC), "rebuild retirement")
	_, hs := newMirrorSync(t, remote, t.TempDir())
	prepared, err := hs.Prepare(t.Context())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(prepared.Close()) })
	prepared.commitReady = true
	prepared.retireJournal = func(string) error {
		time.Sleep(time.Millisecond)
		return errors.New("retirement sentinel")
	}

	require.ErrorContains(prepared.Commit(), "retirement sentinel")
	assert.Equal(JournalRetirementFailed, prepared.mirrorImport.outcome)
	assert.Positive(prepared.mirrorImport.pending.Stats.RetirementDuration)
}

func TestPreparedHTTPSyncImportActiveImportsPreparedRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "prepared import")
	database, hs := newMirrorSync(t, remote, t.TempDir())

	prepared, err := hs.Prepare(t.Context())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(prepared.Close()) })
	before, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	assert.Empty(before.Sessions, "Prepare must not import active sessions")

	stats, err := prepared.ImportActive(t.Context())
	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)
	after, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(after.Sessions, 1)
	assert.Equal("devbox", after.Sessions[0].Machine)
}

func TestImporterImportsExtractedRemoteFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	tmp := t.TempDir()
	remoteDir := "/home/wes/.claude/projects"
	localDir := filepath.Join(tmp, "home", "wes", ".claude", "projects", "test-project")
	require.NoError(os.MkdirAll(localDir, 0o755))
	sessionPath := filepath.Join(localDir, "session.jsonl")
	require.NoError(os.WriteFile(sessionPath, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "remote import").
			String(),
	), 0o644))

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(t.Context(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remoteDir},
		},
	}, tmp)

	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	assert.Equal("devbox", page.Sessions[0].Machine)
	full, err := database.GetSessionFull(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	require.NotNil(full)
	require.NotNil(full.FilePath)
	assert.Contains(*full.FilePath, "devbox:/home/wes/.claude/projects/test-project/session.jsonl")
}

func TestImporterAppliesDBOwnedToolResultImagePolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	database.SetToolResultImages(config.ToolResultImagesDrop)

	extracted := t.TempDir()
	remoteRoot := "/home/remote/.codex/sessions"
	const sessionUUID = "019eb791-cf7d-75c1-8439-9ed74c1229f6"
	localDir := filepath.Join(
		remappedRemotePath(extracted, remoteRoot), "2024", "01", "01",
	)
	require.NoError(os.MkdirAll(localDir, 0o755))
	imageContent := `[{
  "type":"input_image",
  "image_url":"data:image/png;base64,AAEC"
}]`
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			sessionUUID, "/work", "codex", "2024-01-01T00:00:00Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "show the image", "2024-01-01T00:00:01Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"Read", "call_image", map[string]string{
				"file_path": "image.png",
			}, "2024-01-01T00:00:02Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_image", imageContent, "2024-01-01T00:00:03Z",
		),
	)
	require.NoError(os.WriteFile(
		filepath.Join(
			localDir,
			"rollout-2024-01-01T10-00-00-"+sessionUUID+".jsonl",
		),
		[]byte(transcript), 0o644,
	))

	stats, err := (Importer{Host: "devbox", DB: database}).ImportExtracted(
		t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentCodex: {remoteRoot},
		}}, extracted,
	)
	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)

	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	messages, err := database.GetAllMessages(
		t.Context(), page.Sessions[0].ID,
	)
	require.NoError(err)
	require.Len(messages, 2)
	require.Len(messages[1].ToolCalls, 1)
	call := messages[1].ToolCalls[0]
	require.Len(call.ResultEvents, 1)
	assert.Contains(call.ResultContent, "agentsview_image")
	assert.NotContains(call.ResultContent, "input_image")
	assert.Contains(call.ResultEvents[0].Content, "agentsview_image")
	assert.NotContains(call.ResultEvents[0].Content, "input_image")
}

func TestImporterHonorsUsageOnlyStorageBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	database.SetArchiveContent(config.ArchiveContentUsage)

	extracted := t.TempDir()
	remoteDir := "/home/remote-user/.claude/projects"
	localDir := filepath.Join(
		extracted, "home", "remote-user", ".claude", "projects", "private-project",
	)
	require.NoError(os.MkdirAll(localDir, 0o755))
	const sessionID = "usage-only-remote"
	body := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			"2026-08-31T10:00:00Z", "private remote prompt", sessionID,
		).
		AddClaudeAssistantUsage(
			"2026-08-31T10:00:01Z", "private remote response",
			testjsonl.ClaudeAssistantUsage{
				MessageID: "message-id", RequestID: "request-id",
				Model: "claude-sonnet-4-6", InputTokens: 100, OutputTokens: 20,
			},
		).
		String()
	require.NoError(os.WriteFile(
		filepath.Join(localDir, "session.jsonl"), []byte(body), 0o600,
	))

	stats, err := (Importer{Host: "remote-host", DB: database}).ImportExtracted(
		t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remoteDir},
		}}, extracted,
	)
	require.NoError(err)
	require.Equal(1, stats.SessionsSynced)

	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	stored, err := database.GetSessionFull(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Nil(stored.FirstMessage)
	assert.Nil(stored.DisplayName)
	assert.Nil(stored.SessionName)
	messages, err := database.GetAllMessages(t.Context(), stored.ID)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("assistant", messages[0].Role)
	assert.Empty(messages[0].Content)
	assert.Empty(messages[0].ToolCalls)
}

func TestRequireCompleteRejectsDeferredWithoutHardFailure(t *testing.T) {
	err := requireCompleteProcessing(syncpkg.SyncStats{Deferred: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed=0")
	assert.Contains(t, err.Error(), "deferred=1")
}

func TestImporterHydratesIcodematePersistedToolResult(t *testing.T) {
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	extracted := t.TempDir()
	remoteRoot := "/remote/icodemate/projects"
	remoteSession := remoteRoot + "/project/sidecar.jsonl"
	remoteResult := remoteRoot + "/project/sidecar/tool-results/output.txt"
	localSession := remappedRemotePath(extracted, remoteSession)
	localResult := remappedRemotePath(extracted, remoteResult)
	require.NoError(os.MkdirAll(filepath.Dir(localResult), 0o755))
	require.NoError(os.WriteFile(localResult, []byte("remote persisted output\n"), 0o644))

	persistedPath, err := json.Marshal(remoteResult)
	require.NoError(err)
	placeholder := "<persisted-output>\nOutput too large. Full output saved to: " +
		remoteResult + "\n</persisted-output>"
	placeholderJSON, err := json.Marshal(placeholder)
	require.NoError(err)
	transcript := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + string(placeholderJSON) + `}]},"toolUseResult":{"persistedOutputPath":` + string(persistedPath) + `}}`,
	}, "\n") + "\n"
	require.NoError(os.WriteFile(localSession, []byte(transcript), 0o644))

	stats, err := Importer{Host: "devbox", DB: database}.ImportExtracted(
		t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {remoteRoot},
		}}, extracted,
	)
	require.NoError(err)
	require.Equal(1, stats.SessionsSynced)
	messages, err := database.GetMessages(
		t.Context(), "devbox~icodemate:sidecar", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 2)
	require.Len(messages[1].ToolCalls, 1)
	assert.Equal(t, "remote persisted output\n",
		messages[1].ToolCalls[0].ResultContent)
}

func TestImporterReturnsPartialStatsWhenOneSourceFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	extracted := t.TempDir()
	claudeRoot := "/remote/claude"
	claudeDir := filepath.Join(remappedRemotePath(extracted, claudeRoot), "project")
	require.NoError(os.MkdirAll(claudeDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(claudeDir, "healthy.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(
			"2026-08-15T10:00:00Z", "healthy remote source", "healthy",
		).String()),
		0o644,
	))
	qwenPawRoot := "/remote/qwenpaw"
	qwenPawDir := filepath.Join(
		remappedRemotePath(extracted, qwenPawRoot), "default", "sessions",
	)
	require.NoError(os.MkdirAll(qwenPawDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(qwenPawDir, "broken.json"), []byte("{not valid json"), 0o644,
	))

	stats, err := Importer{Host: "devbox", DB: database}.ImportExtracted(
		t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude:  {claudeRoot},
			parser.AgentQwenPaw: {qwenPawRoot},
		}}, extracted,
	)

	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)
	assert.Positive(stats.Failed)
	session, err := database.GetSession(t.Context(), "devbox~healthy")
	require.NoError(err)
	assert.NotNil(session)
}

func TestImporterImportsEveryRemoteProviderTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	extracted := t.TempDir()
	claudeRoot := "/remote/claude"
	claudeDir := filepath.Join(
		remappedRemotePath(extracted, claudeRoot), "project",
	)
	require.NoError(os.MkdirAll(claudeDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(claudeDir, "enabled.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUserWithSessionID(
				"2026-08-09T10:00:00Z", "enabled remote", "enabled-remote",
			).
			String()),
		0o644,
	))

	geminiRoot := "/remote/gemini"
	geminiDir := filepath.Join(
		remappedRemotePath(extracted, geminiRoot),
		"tmp", "project", "chats",
	)
	require.NoError(os.MkdirAll(geminiDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(geminiDir,
			"session-2026-08-09T10-00-disabled-remote.json"),
		[]byte(testjsonl.GeminiSessionJSON(
			"disabled-remote",
			"project",
			"2026-08-09T10:00:00Z",
			"2026-08-09T10:01:00Z",
			[]map[string]any{
				testjsonl.GeminiUserMsg(
					"user", "2026-08-09T10:00:00Z", "disabled remote",
				),
			},
		)),
		0o644,
	))

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(t.Context(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
			parser.AgentGemini: {geminiRoot},
		},
	}, extracted)

	require.NoError(err)
	assert.Equal(2, stats.SessionsSynced)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(page.Sessions, 2)
	ids := []string{page.Sessions[0].ID, page.Sessions[1].ID}
	assert.ElementsMatch([]string{
		"devbox~enabled",
		"devbox~gemini:disabled-remote",
	}, ids)
}

func TestImporterImportsHermesDatabaseOnlySession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	extracted := t.TempDir()
	remoteSessionsDir := "/home/remote/.hermes/profiles/research/sessions"
	remoteStateDB := "/home/remote/.hermes/profiles/research/state.db"
	localStateDB := remappedRemotePath(extracted, remoteStateDB)
	require.NoError(os.MkdirAll(filepath.Dir(localStateDB), 0o755))
	require.NoError(os.MkdirAll(
		remappedRemotePath(extracted, remoteSessionsDir), 0o755,
	))
	writeHermesImportStateDB(t, localStateDB)

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(t.Context(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {remoteSessionsDir},
		},
	}, extracted)

	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)
	session, err := database.GetSession(
		t.Context(), "devbox~hermes:database-only",
	)
	require.NoError(err)
	require.NotNil(session)
	require.NotNil(session.DisplayName)
	assert.Equal("Database-only profile", *session.DisplayName)
	require.NotNil(session.ParentSessionID)
	assert.Equal("devbox~hermes:parent", *session.ParentSessionID)
	assert.Equal(70, session.TotalOutputTokens)
	assert.Equal(320, session.PeakContextTokens)
}

func writeHermesImportStateDB(t *testing.T, path string) {
	t.Helper()
	stateDB, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, stateDB.Close()) }()

	_, err = stateDB.ExecContext(t.Context(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			model TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			message_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT,
			title TEXT,
			api_call_count INTEGER DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			tool_call_id TEXT,
			tool_calls TEXT,
			timestamp REAL NOT NULL,
			finish_reason TEXT,
			reasoning TEXT,
			reasoning_content TEXT,
			reasoning_details TEXT,
			codex_reasoning_items TEXT,
			codex_message_items TEXT
		);
		INSERT INTO sessions (
			id, source, model, parent_session_id, started_at, ended_at,
			message_count, input_tokens, output_tokens, cache_read_tokens,
			title
		) VALUES (
			'database-only', 'cli', 'gpt-5.4', 'parent',
			1778767200.0, 1778767800.0, 1, 300, 70, 20,
			'Database-only profile'
		);
		INSERT INTO messages (session_id, role, content, timestamp)
		VALUES ('database-only', 'user', 'database-only message', 1778767210.0);
	`)
	require.NoError(t, err)
}

// TestImporterMapsHermesStateDBExtraFileAndRefreshesWALChanges verifies both
// remote archive path translation and repeated-import freshness. A committed
// WAL-only metadata update must invalidate the skip entry even though the main
// state.db file remains unchanged.
func TestImporterMapsHermesStateDBExtraFileAndRefreshesWALChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	extracted := t.TempDir()
	remoteSessionsDir := "/home/remote/.hermes/profiles/research/sessions"
	remoteStateDB := "/home/remote/.hermes/profiles/research/state.db"
	localStateDB := remappedRemotePath(extracted, remoteStateDB)
	require.NoError(os.MkdirAll(filepath.Dir(localStateDB), 0o755))
	require.NoError(os.MkdirAll(
		remappedRemotePath(extracted, remoteSessionsDir), 0o755,
	))
	writeHermesImportStateDB(t, localStateDB)
	writer := openHermesImportWALWriter(t, localStateDB)
	remoteStateWAL := remoteStateDB + "-wal"

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {remoteSessionsDir},
		},
		ExtraFiles: []string{remoteStateDB, remoteStateWAL},
	}

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(t.Context(), targets, extracted)
	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)

	full, err := database.GetSessionFull(
		t.Context(), "devbox~hermes:database-only",
	)
	require.NoError(err)
	require.NotNil(full)
	require.NotNil(full.FilePath)
	assert.Equal("devbox:"+remoteStateDB, *full.FilePath,
		"state.db extra file must map to its original remote path")
	assert.NotContains(*full.FilePath, extracted,
		"the import must not leak the local extraction dir into FilePath")

	// A second import of the unchanged archive must skip the state.db and
	// persist its skip entry keyed by the remote path. Without the
	// extra-file mapping the local temp path cannot be translated back, so
	// the entry is silently dropped and the archive is re-fingerprinted on
	// every subsequent sync.
	second, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(t.Context(), targets, extracted)
	require.NoError(err)
	assert.Zero(second.SessionsSynced,
		"unchanged state.db must not re-sync on the second import")

	remoteCache, err := database.LoadRemoteSkippedFiles("devbox")
	require.NoError(err)
	require.NotEmpty(remoteCache,
		"the state.db skip entry must survive the import, not be discarded")
	_, ok := remoteCache[remoteStateDB+"?agent=hermes"]
	assert.True(ok,
		"skip cache must key the state.db entry by its remote path, got %v",
		remoteCache)

	stateBefore, err := os.Stat(localStateDB)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), `
		UPDATE sessions
		SET title = 'WAL-refreshed profile'
		WHERE id = 'database-only'
	`)
	require.NoError(err)
	localStateWAL := remappedRemotePath(extracted, remoteStateWAL)
	require.FileExists(localStateWAL)
	stateAfter, err := os.Stat(localStateDB)
	require.NoError(err)
	assert.Equal(stateBefore.Size(), stateAfter.Size())
	assert.Equal(stateBefore.ModTime(), stateAfter.ModTime(),
		"the committed update must remain WAL-only for this regression")
	walTime := stateAfter.ModTime().Add(2 * time.Second)
	require.NoError(os.Chtimes(localStateWAL, walTime, walTime))

	changed, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(t.Context(), targets, extracted)
	require.NoError(err)
	assert.Equal(1, changed.SessionsSynced,
		"a WAL-only commit must invalidate the remote archive skip entry")
	refreshed, err := database.GetSession(
		t.Context(), "devbox~hermes:database-only",
	)
	require.NoError(err)
	require.NotNil(refreshed)
	require.NotNil(refreshed.DisplayName)
	assert.Equal("WAL-refreshed profile", *refreshed.DisplayName)
}

func openHermesImportWALWriter(t *testing.T, stateDB string) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })

	var journalMode string
	require.NoError(t, writer.QueryRowContext(t.Context(), `PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint = 0`)
	require.NoError(t, err)
	return writer
}

func TestRemotePathMappingHandlesWindowsDrivePath(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `C:\Users\wes\.codex\sessions`
	remoteFile := `C:\Users\wes\.codex\sessions\2026\session.jsonl`
	wantLocalDir := filepath.Join(
		tempDir, "__drive_C", "Users", "wes", ".codex", "sessions",
	)
	wantLocalFile := filepath.Join(
		tempDir, "__drive_C", "Users", "wes", ".codex", "sessions",
		"2026", "session.jsonl",
	)

	assert.Equal(t, wantLocalDir, RemappedDir(tempDir, remoteDir))
	assert.Equal(t, wantLocalFile, remappedRemotePath(tempDir, remoteFile))
	assert.Equal(t,
		remoteFile,
		RemapToRemotePath(tempDir, remoteDir, wantLocalFile),
	)
}

func TestRemotePathMappingHandlesForwardSlashUNCPath(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `//server/share/.codex/sessions`
	remoteFile := `//server/share/.codex/sessions/2026/session.jsonl`
	wantLocalDir := filepath.Join(
		tempDir, "__unc", "server", "share", ".codex", "sessions",
	)
	wantLocalFile := filepath.Join(
		tempDir, "__unc", "server", "share", ".codex", "sessions",
		"2026", "session.jsonl",
	)

	assert.Equal(t, wantLocalDir, RemappedDir(tempDir, remoteDir))
	assert.Equal(t, wantLocalFile, remappedRemotePath(tempDir, remoteFile))
	assert.Equal(t,
		remoteFile,
		RemapToRemotePath(tempDir, remoteDir, wantLocalFile),
	)
}

func TestImporterRejectsEscapingRemoteTargets(t *testing.T) {
	stats, err := Importer{
		Host: "devbox",
	}.ImportExtracted(t.Context(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"../../outside"},
		},
	}, t.TempDir())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe remote path")
	assert.Zero(t, stats)
}

func TestLocalArchivePathRejectsDotDotComponents(t *testing.T) {
	_, err := safeLocalArchivePath(t.TempDir(), "safe/../escape.jsonl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe archive path")
}

func TestRemoteSkipCacheUsesArchivePathMapping(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `C:\Users\wes\.codex\sessions`
	remoteFile := `C:\Users\wes\.codex\sessions\2026\session.jsonl`
	tempRoot := RemappedDir(tempDir, remoteDir)
	tempFile := filepath.Join(tempRoot, "2026", "session.jsonl")

	translated := translateRemoteCacheToTemp(
		map[string]int64{remoteFile: 123},
		[]string{remoteDir},
		[]string{tempRoot},
	)
	assert.Equal(t, map[string]int64{tempFile: 123}, translated)

	got, ok := tempPathToRemotePath(
		tempFile,
		[]string{remoteDir},
		[]string{tempRoot},
	)
	require.True(t, ok)
	assert.Equal(t, remoteFile, got)
}

func TestRemoteSkipCacheRoundTripsQualifiedExtraFile(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	const (
		host       = "devbox"
		remoteFile = "/home/remote/.hermes/state.db"
		qualified  = remoteFile + "?agent=hermes"
	)
	targets := TargetSet{ExtraFiles: []string{remoteFile}}
	layout, cfg, err := newImportInputs(host, nil, targets, root)
	require.NoError(err)

	localFile := remappedRemotePath(root, remoteFile) + "?agent=hermes"
	translated := translateRemoteCacheToTemp(
		map[string]int64{qualified: 123},
		layout.paths.remoteDirs,
		layout.paths.localDirs,
	)
	require.Equal(map[string]int64{localFile: 123}, translated,
		"remote translation must preserve the provider qualifier")

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	engine := syncpkg.NewEngine(database, cfg)
	t.Cleanup(engine.Close)
	engine.InjectSkipCache(translated)
	require.NoError(saveEngineSkipCache(database, engine, layout.paths))

	remoteCache, err := database.LoadRemoteSkippedFiles(host)
	require.NoError(err)
	assert.Equal(t, map[string]int64{qualified: 123}, remoteCache,
		"temporary translation must restore the identical remote cache key")
}
