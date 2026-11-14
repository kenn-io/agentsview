package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	musePersonalSyncID     = "11111111-2222-4333-8444-555555555555"
	musePersonalSyncHeader = `{"type":"session_header","version":1,"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z"}` + "\n"
)

func musePersonalSyncItem(seq int, item string) string {
	return fmt.Sprintf(`{"type":"item","seq":%d,"source":"scheduler.cron","created_at":"2026-10-05T15:04:06Z","item":%s}`+"\n", seq, item)
}

func musePersonalSyncFile(t *testing.T, body string) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "agent-66666666-7777-4888-8999-000000000000", "sessions", musePersonalSyncID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+body), 0o600))
	return root, path
}

func TestMusePersonalArchiveReplacementAndIndexPolling(t *testing.T) {
	user := musePersonalSyncItem(0, `{"type":"message","role":"user","text":"original"}`)
	call := musePersonalSyncItem(1, `{"type":"function_call","call_id":"late","name":"exec","arguments":"{}"}`)
	root, path := musePersonalSyncFile(t, user+call)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	id := "muse-personal:" + musePersonalSyncID
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	assert.Zero(t, engine.SyncAll(ctx, nil).Synced)
	output := musePersonalSyncItem(2, `{"type":"function_call_output","call_id":"late","output":"late output","success":true}`)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+user+call+output), 0o600))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	messages, err := database.GetMessages(ctx, id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "late output", messages[1].ToolCalls[0].ResultEvents[0].Content)
	before := engine.SourceMtime(ctx, id)
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	indexText := `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`
	require.NoError(t, os.WriteFile(index, []byte(indexText), 0o600))
	assert.NotEqual(t, before, engine.SourceMtime(ctx, id))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	session, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2026-10-05T16:00:00Z", *session.EndedAt)
	// Same-size, same-mtime index replacement must still refresh metadata.
	info, err := os.Stat(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(index, []byte(strings.ReplaceAll(indexText, "16:00:00", "17:00:00")), 0o600))
	require.NoError(t, os.Chtimes(index, info.ModTime(), info.ModTime()))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	session, err = database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-05T17:00:00Z", *session.EndedAt)
	require.NoError(t, os.Remove(index))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	session, err = database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-05T15:04:06Z", *session.EndedAt)
	// A shorter unfinished rewrite must preserve all previously archived results.
	replacement := musePersonalSyncItem(0, `{"type":"message","role":"user","text":"replacement"}`)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+replacement+`{"type":"item"`), 0o600))
	assert.Positive(t, engine.SyncAll(ctx, nil).Failed)
	messages, err = database.GetMessages(ctx, id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "late output", messages[1].ToolCalls[0].ResultContent)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+replacement), 0o600))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	messages, err = database.GetMessages(ctx, id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "replacement", messages[0].Content)
	found, err := database.Search(ctx, db.SearchFilter{Query: "original"})
	require.NoError(t, err)
	assert.Empty(t, found.Results)
}

func TestMusePersonalPartialDiscoverySyncsHealthySourcesWithoutTombstones(t *testing.T) {
	root, existingPath := musePersonalSyncFile(t, musePersonalSyncItem(0, `{"type":"message","role":"user","text":"existing"}`))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	existingID := "muse-personal:" + musePersonalSyncID
	before, err := database.GetSessionFull(ctx, existingID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NoError(t, os.Remove(existingPath))

	sessionsDir := filepath.Dir(existingPath)
	incompleteID := "00000000-2222-4333-8444-555555555555"
	require.NoError(t, os.WriteFile(filepath.Join(sessionsDir, incompleteID+".jsonl"), []byte(`{"type":"session_header"`), 0o600))
	healthyID := "99999999-2222-4333-8444-555555555555"
	healthyHeader := strings.ReplaceAll(musePersonalSyncHeader, musePersonalSyncID, healthyID)
	healthyPath := filepath.Join(sessionsDir, healthyID+".jsonl")
	require.NoError(t, os.WriteFile(healthyPath, []byte(healthyHeader+musePersonalSyncItem(0, `{"type":"message","role":"user","text":"healthy"}`)), 0o600))

	syncStats := engine.SyncAll(ctx, nil)
	assert.Positive(t, syncStats.Failed, "partial discovery must report the unfinished header")
	healthy, err := database.GetSessionFull(ctx, "muse-personal:"+healthyID)
	require.NoError(t, err)
	require.NotNil(t, healthy, "healthy sources after an incomplete header must still sync")

	stats, tombstoned, err := engine.ReconcileWatchRootsWithStats(ctx, []string{root}, true, nil)
	var incomplete *incompleteReconciliationError
	require.ErrorAs(t, err, &incomplete, "partial discovery must leave the root for a retry")
	assert.Positive(t, incomplete.failures)
	assert.Contains(t, incomplete.ReconciliationRetryRoots(), root)
	assert.Zero(t, stats.Failed)
	assert.Zero(t, tombstoned, "partial discovery must not tombstone the removed source")
	existingAfter, err := database.GetSessionFull(ctx, existingID)
	require.NoError(t, err)
	require.NotNil(t, existingAfter, "partial discovery must preserve a previously archived session")
	assert.Nil(t, existingAfter.SourceMissingAt)
}

func TestMusePersonalSyncAllSinceIncludesIndexOnlyChanges(t *testing.T) {
	root, transcript := musePersonalSyncFile(t, musePersonalSyncItem(0, `{"type":"message","role":"user","text":"indexed"}`))
	index := filepath.Join(filepath.Dir(transcript), "sessions.json")
	indexText := `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`
	require.NoError(t, os.WriteFile(index, []byte(indexText), 0o600))
	old := time.Now().Add(-24 * time.Hour)
	require.NoError(t, os.Chtimes(transcript, old, old))
	require.NoError(t, os.Chtimes(index, old, old))
	require.NoError(t, os.Chtimes(filepath.Dir(transcript), old, old))

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	id := "muse-personal:" + musePersonalSyncID
	initialMtime, err := musePersonalCompositeMtime(transcript)
	require.NoError(t, err)
	cutoff := time.Unix(0, initialMtime+1)

	info, err := os.Stat(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(index, []byte(strings.ReplaceAll(indexText, "16:00:00", "17:00:00")), 0o600))
	require.NoError(t, os.Chtimes(index, info.ModTime(), info.ModTime()))
	if _, ok := fileChangeTime(index, info); !ok {
		t.Skip("platform does not expose file change time")
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, os.Chtimes(index, info.ModTime(), info.ModTime()))
		indexInfo, err := os.Stat(index)
		require.NoError(c, err)
		changeTime, ok := fileChangeTime(index, indexInfo)
		require.True(c, ok)
		assert.GreaterOrEqual(c, changeTime, cutoff.UnixNano())
	}, 2*time.Second, time.Millisecond)
	// A new engine must observe persisted archive freshness correctly too.
	engine.Close()
	engine = NewEngine(ctx, database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAllSince(ctx, cutoff, nil).Failed)
	session, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2026-10-05T17:00:00Z", *session.EndedAt)

	updatedMtime, err := musePersonalCompositeMtime(transcript)
	require.NoError(t, err)
	removeCutoff := time.Unix(0, updatedMtime+1)
	require.NoError(t, os.Remove(index))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		now := time.Now()
		require.NoError(c, os.Chtimes(filepath.Dir(transcript), now, now))
		dirInfo, err := os.Stat(filepath.Dir(transcript))
		require.NoError(c, err)
		assert.GreaterOrEqual(c, dirInfo.ModTime().UnixNano(), removeCutoff.UnixNano())
	}, 2*time.Second, time.Millisecond)
	engine.Close()
	engine = NewEngine(ctx, database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAllSince(ctx, removeCutoff, nil).Failed)
	session, err = database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2026-10-05T15:04:06Z", *session.EndedAt)
}

// A shared index edit must replace only the session whose identity changed.
func TestMusePersonalIndexRefreshesOnlyChangedSession(t *testing.T) {
	for _, watcher := range []bool{false, true} {
		t.Run(strconv.FormatBool(watcher), func(t *testing.T) {
			root, path := musePersonalSyncFile(t, musePersonalSyncItem(0, `{"type":"message","role":"user","text":"original"}`))
			secondID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			second := filepath.Join(filepath.Dir(path), secondID+".jsonl")
			require.NoError(t, os.WriteFile(second, []byte(strings.ReplaceAll(musePersonalSyncHeader, musePersonalSyncID, secondID)+musePersonalSyncItem(0, `{"type":"message","role":"user","text":"sibling"}`)), 0o600))
			index := filepath.Join(filepath.Dir(path), "sessions.json")
			row := func(id, updated string) string {
				return fmt.Sprintf(`{"session_id":%q,"agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":%q}`, id, updated)
			}
			writeIndex := func(a string) {
				rows := row(secondID, "2026-10-05T16:00:00Z")
				if a != "" {
					rows = row(musePersonalSyncID, a) + "," + rows
				}
				require.NoError(t, os.WriteFile(index, []byte(`{"version":1,"sessions":[`+rows+`]}`), 0o600))
			}
			writeIndex("2026-10-05T16:00:00Z")
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			require.Equal(t, 2, stats.Synced)
			written := 0
			engine.writeBatchOverride = func(batch []pendingWrite, mode syncWriteMode, force bool) (int, int, int, int) {
				written += len(batch)
				out := engine.writeBatchWithOutcomeContext(t.Context(), batch, mode, force)
				return out.writtenSessions, out.writtenMessages, out.failedSessions, out.cwdFiltered
			}
			sibling, err := database.GetMessages(t.Context(), "muse-personal:"+secondID, 0, 100, true)
			require.NoError(t, err)
			for _, step := range []struct{ updated, want string }{
				{"2026-10-05T17:00:00.123Z", "2026-10-05T17:00:00.123Z"},
				{"", "2026-10-05T15:04:06Z"},
			} {
				writeIndex(step.updated)
				written = 0
				if watcher {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{index}))
				} else {
					stats = engine.SyncAll(t.Context(), nil)
					require.Zero(t, stats.Failed)
					assert.Equal(t, 1, stats.Synced)
				}
				assert.Equal(t, 1, written, "index event must admit exactly one archive replacement")
				session, err := database.GetSessionFull(t.Context(), "muse-personal:"+musePersonalSyncID)
				require.NoError(t, err)
				require.NotNil(t, session.EndedAt)
				assert.Equal(t, step.want, *session.EndedAt)
				b, err := database.GetSessionFull(t.Context(), "muse-personal:"+secondID)
				require.NoError(t, err)
				require.NotNil(t, b.EndedAt)
				assert.Equal(t, "2026-10-05T16:00:00Z", *b.EndedAt)
				messages, err := database.GetMessages(t.Context(), "muse-personal:"+secondID, 0, 100, true)
				require.NoError(t, err)
				assert.Equal(t, sibling, messages, "unchanged sibling must retain its stored messages")
				stats = engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				assert.Zero(t, stats.Synced)
			}
			require.NoError(t, os.Remove(index))
			written = 0
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{index}))
			assert.Equal(t, 1, written, "whole-index removal changes only the remaining indexed sibling")
			// Explicit reparse remains available even when fingerprints match.
			written = 0
			require.NoError(t, engine.ReparsePathsContext(t.Context(), []string{second}))
			assert.Equal(t, 1, written)
			engine.Close()
			engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			stats = engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			assert.Zero(t, stats.Synced)
		})
	}
}

func TestMusePersonalPollingWithUnusableIndex(t *testing.T) {
	body := musePersonalSyncItem(0, `{"type":"message","role":"user","text":"first"}`)
	root, path := musePersonalSyncFile(t, body)
	// A directory stands in for an unusable optional index: it forces the zero
	// stat digest also returned when a regular index cannot be inspected.
	require.NoError(t, os.Mkdir(filepath.Join(filepath.Dir(path), "sessions.json"), 0o700))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	id := "muse-personal:" + musePersonalSyncID
	before := engine.SourceMtime(t.Context(), id)
	require.NotZero(t, before, "unusable optional metadata must not hide the transcript")
	body += musePersonalSyncItem(1, `{"type":"message","role":"assistant","text":"reply"}`)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+body), 0o600))
	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, later, later))
	after := engine.SourceMtime(t.Context(), id)
	assert.NotZero(t, after)
	assert.NotEqual(t, before, after, "polling must notice the appended reply")
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	messages, err := database.GetMessages(t.Context(), id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "reply", messages[1].Content)
}
