package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Codex thread/revert writes rollout-<ts>-<thread>_<rollout>.jsonl into the
// dated folder of the undo. Its session_meta keeps the thread id and names the
// rollout it builds on in history_base. Each file is its own session, linked to
// its base as a continuation.

const paginationThread = "11111111-1111-4111-8111-111111111111"

var paginationRollouts = []string{
	paginationThread,
	"22222222-2222-4222-8222-222222222222",
	"33333333-3333-4333-8333-333333333333",
	"44444444-4444-4444-8444-444444444444",
	"55555555-5555-4555-8555-555555555555",
}

func codexUserEntryJSON(timestamp, text string) string {
	return `{"type":"response_item","timestamp":"` + timestamp +
		`","payload":{"type":"message","role":"user","content":` +
		`[{"type":"input_text","text":"` + text + `"}]}}`
}

func codexAssistantEntryJSON(timestamp, text string) string {
	return fmt.Sprintf(`{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, timestamp, text)
}

func codexTokenCountJSON(timestamp string, input, output int) string {
	return fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"output_tokens":%d}}}}`, timestamp, input, output)
}

// writeCodexRollout writes one rollout file into the dated Codex layout and
// returns its path.
func writeCodexRollout(t *testing.T, root, day, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "09", day)
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir codex day dir")
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write rollout")
	return path
}

// codexRevertMeta returns a page session_meta; an empty base omits history_base.
func codexRevertMeta(timestamp, base string, ordinal int) string {
	extra := map[string]any{"session_id": paginationThread, "history_mode": "paginated"}
	if base != "" {
		extra["history_base"] = map[string]any{"thread_id": base, "end_ordinal_exclusive": ordinal}
	}
	return testjsonl.CodexSessionMetaWithFieldsJSON(paginationThread, "/work/project", "codex_cli_rs", timestamp, extra)
}

// paginationSessionID is the session id of file page: the thread for page 0,
// else the thread's revert page for paginationRollouts[page].
func paginationSessionID(page int) string {
	if page == 0 {
		return "codex:" + paginationThread
	}
	return "codex:" + paginationThread + "_" + paginationRollouts[page]
}

// paginationParent is the parent a page names: the thread when every page
// bases on it (direct), else the previous page.
func paginationParent(page int, previousRollout bool) string {
	if page == 0 {
		return ""
	}
	if previousRollout {
		return paginationSessionID(page - 1)
	}
	return paginationSessionID(0)
}

func writeCodexUsagePage(t *testing.T, root string, page int, crossDay, previousRollout bool) string {
	t.Helper()
	day := 24 + page
	timestamp := fmt.Sprintf("2026-09-%02dT04:00:00Z", day)
	base := ""
	if page > 0 {
		base = paginationThread
		if previousRollout {
			base = paginationRollouts[page-1]
		}
	}
	identity := paginationThread
	if page > 0 {
		identity += "_" + paginationRollouts[page]
	}
	dirDay := "24"
	if crossDay {
		dirDay = fmt.Sprintf("%02d", day)
	}
	return writeCodexRollout(t, root, dirDay,
		fmt.Sprintf("rollout-2026-09-%02dT12-00-00-%s.jsonl", day, identity),
		testjsonl.JoinJSONL(
			codexRevertMeta(timestamp, base, page*5),
			fmt.Sprintf(`{"type":"turn_context","timestamp":%q,"payload":{"model":"gpt-5","turn_id":"turn-%d"}}`, timestamp, page),
			codexUserEntryJSON(timestamp, fmt.Sprintf("Request %d", page)),
			codexAssistantEntryJSON(timestamp, fmt.Sprintf("Response %d", page)),
			codexTokenCountJSON(timestamp, (page+1)*100, (page+1)*10),
		))
}

func dailyOutputTokens(t *testing.T, database *db.DB) (map[string]int, int) {
	t.Helper()
	usage, err := database.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2026-09-24", To: "2026-09-28", Timezone: "Asia/Shanghai",
	})
	require.NoError(t, err)
	got := make(map[string]int)
	for _, day := range usage.Daily {
		got[day.Date] = day.OutputTokens
	}
	return got, usage.Totals.OutputTokens
}

// assertCodexPaginationUsage checks that each file's session holds its own two
// messages and parent, and that daily usage counts each file's tokens once.
func assertCodexPaginationUsage(t *testing.T, database *db.DB, pages int, previousRollout bool) {
	t.Helper()
	want := make(map[string]int)
	for page := range pages {
		id := paginationSessionID(page)
		messages, err := database.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		if assert.Len(t, messages, 2, "messages of %s", id) {
			assert.Equal(t, fmt.Sprintf("Request %d", page), messages[0].Content)
		}
		sess, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess, "session %s", id)
		if parent := paginationParent(page, previousRollout); parent != "" {
			require.NotNil(t, sess.ParentSessionID, "parent of %s", id)
			assert.Equal(t, parent, *sess.ParentSessionID)
			assert.Equal(t, "continuation", sess.RelationshipType)
		} else {
			assert.Nil(t, sess.ParentSessionID, "the thread's own rollout has no parent")
		}
		want[fmt.Sprintf("2026-09-%02d", 24+page)] = (page + 1) * 10
	}
	got, total := dailyOutputTokens(t, database)
	assert.Equal(t, want, got, "Usage must retain the original dates without double-counting")
	assert.Equal(t, pages*(pages+1)*5, total)
}

func newCodexRevertEngine(t *testing.T, database *db.DB, roots ...string) *Engine {
	t.Helper()
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: roots},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	return engine
}

func messageContents(t *testing.T, database *db.DB, id string) []string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		texts = append(texts, m.Content)
	}
	return texts
}

func TestCodexRevertPagesFormATree(t *testing.T) {
	const (
		pageA = "22222222-2222-4222-8222-222222222222"
		pageB = "33333333-3333-4333-8333-333333333333"
		pageC = "44444444-4444-4444-8444-444444444444"
	)
	root := t.TempDir()
	turn := func(ts, prompt, reply string, output int) []string {
		return []string{
			fmt.Sprintf(`{"type":"turn_context","timestamp":%q,"payload":{"model":"gpt-5","turn_id":%q}}`, ts, prompt),
			codexUserEntryJSON(ts, prompt),
			codexAssistantEntryJSON(ts, reply),
			codexTokenCountJSON(ts, output*10, output),
		}
	}
	head := []string{testjsonl.CodexSessionMetaWithFieldsJSON(
		paginationThread, "/work/project", "codex_cli_rs", "2026-09-01T10:00:00Z",
		map[string]any{"history_mode": "paginated"})}
	head = append(head, turn("2026-09-01T10:00:01Z", "turn 1", "reply 1", 1)...)
	head = append(head, turn("2026-09-01T10:00:02Z", "turn 2", "reply 2", 2)...)
	head = append(head, turn("2026-09-01T10:00:03Z", "turn 3", "reply 3", 4)...)
	writeCodexRollout(t, root, "01", "rollout-2026-09-01T10-00-00-"+paginationThread+".jsonl",
		testjsonl.JoinJSONL(head...))
	// Page A undoes turn 3, page B undoes turns 2 and 3; both base on the head.
	writeCodexRollout(t, root, "01", "rollout-2026-09-01T11-00-00-"+paginationThread+"_"+pageA+".jsonl",
		testjsonl.JoinJSONL(append([]string{codexRevertMeta("2026-09-01T11:00:00Z", paginationThread, 7)},
			turn("2026-09-01T11:00:01Z", "page A turn", "page A reply", 8)...)...))
	writeCodexRollout(t, root, "02", "rollout-2026-09-02T11-00-00-"+paginationThread+"_"+pageB+".jsonl",
		testjsonl.JoinJSONL(append([]string{codexRevertMeta("2026-09-02T11:00:00Z", paginationThread, 4)},
			turn("2026-09-02T11:00:01Z", "page B turn", "page B reply", 16)...)...))
	// Page C reverts to before the first turn and has no base.
	writeCodexRollout(t, root, "03", "rollout-2026-09-03T11-00-00-"+paginationThread+"_"+pageC+".jsonl",
		testjsonl.JoinJSONL(append([]string{codexRevertMeta("2026-09-03T11:00:00Z", "", 0)},
			turn("2026-09-03T11:00:01Z", "page C turn", "page C reply", 32)...)...))

	database := openTestDB(t)
	engine := newCodexRevertEngine(t, database, root)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	headID := "codex:" + paginationThread
	assert.Equal(t,
		[]string{"turn 1", "reply 1", "turn 2", "reply 2", "turn 3", "reply 3"},
		messageContents(t, database, headID),
		"the head keeps the undone turns")
	for _, tc := range []struct{ rollout, label string }{{pageA, "A"}, {pageB, "B"}, {pageC, "C"}} {
		id := headID + "_" + tc.rollout
		assert.Equal(t,
			[]string{"page " + tc.label + " turn", "page " + tc.label + " reply"},
			messageContents(t, database, id))
		sess, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess)
		require.NotNil(t, sess.ParentSessionID)
		assert.Equal(t, headID, *sess.ParentSessionID)
		assert.Equal(t, "continuation", sess.RelationshipType)
	}

	usage, err := database.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2026-09-01", To: "2026-09-03", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 1+2+4+8+16+32, usage.Totals.OutputTokens, "every file's tokens count once")
}

func TestCodexRevertPagesRetainDailyUsage(t *testing.T) {
	for _, layout := range []struct {
		name     string
		crossDay bool
		previous bool
	}{
		{"same_directory_direct", false, false},
		{"cross_directory_direct", true, false},
		{"same_directory_previous", false, true},
		{"cross_directory_previous", true, true},
	} {
		for _, staged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/staged=%t", layout.name, staged), func(t *testing.T) {
				root := t.TempDir()
				for page := range 3 {
					writeCodexUsagePage(t, root, page, layout.crossDay, layout.previous)
				}
				database := openTestDB(t)
				cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
				if staged {
					cfg.StagedCodexParseMinBytes = 1
				}
				engine := NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				assertCodexPaginationUsage(t, database, 3, layout.previous)

				// Reconciliation uses streaming discovery; pages stay their own sources.
				require.Zero(t, engine.SyncAllSince(t.Context(), time.Time{}, nil).Failed)
				assertCodexPaginationUsage(t, database, 3, layout.previous)

				path := writeCodexUsagePage(t, root, 3, layout.crossDay, layout.previous)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				assertCodexPaginationUsage(t, database, 4, layout.previous)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				assertCodexPaginationUsage(t, database, 4, layout.previous)

				// A removed page keeps its archived session and usage day.
				require.NoError(t, os.Remove(path))
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				assertCodexPaginationUsage(t, database, 4, layout.previous)
				removed, err := database.GetSessionFull(t.Context(), paginationSessionID(3))
				require.NoError(t, err)
				require.NotNil(t, removed)
				assert.NotNil(t, removed.SourceMissingAt, "the removed page is marked source-missing")
			})
		}
	}
}

func TestCodexRevertPageSinceSyncParsesOnlyChangedPage(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	for page := range 3 {
		path := writeCodexUsagePage(t, root, page, true, true)
		require.NoError(t, os.Chtimes(path, old, old))
	}
	database := openTestDB(t)
	engine := newCodexRevertEngine(t, database, root)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	assertCodexPaginationUsage(t, database, 3, true)
	before := make(map[string]*string)
	for page := range 3 {
		sess, err := database.GetSessionFull(t.Context(), paginationSessionID(page))
		require.NoError(t, err)
		require.NotNil(t, sess)
		require.NotNil(t, sess.LocalModifiedAt)
		before[sess.ID] = sess.LocalModifiedAt
	}

	writeCodexUsagePage(t, root, 3, true, true)
	stats := engine.SyncAllSince(t.Context(), time.Now().Add(-time.Hour), nil)
	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Synced, "only the new page is parsed")
	assertCodexPaginationUsage(t, database, 4, true)
	for id, modified := range before {
		sess, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, modified, sess.LocalModifiedAt, "%s must not be rewritten", id)
	}
}

func TestCodexRevertPageArchiveMoveKeepsOneSession(t *testing.T) {
	base := t.TempDir()
	dated := filepath.Join(base, "sessions")
	archived := filepath.Join(base, "archived_sessions")
	require.NoError(t, os.MkdirAll(archived, 0o755))
	writeCodexUsagePage(t, dated, 0, true, false)
	pagePath := writeCodexUsagePage(t, dated, 1, true, false)
	pageID := paginationSessionID(1)

	database := openTestDB(t)
	engine := newCodexRevertEngine(t, database, dated, archived)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	assert.Equal(t, pagePath, database.GetSessionFilePath(t.Context(), pageID))

	codexSessionIDs := func() []string {
		t.Helper()
		page, err := database.ListSessions(t.Context(), db.SessionFilter{Agent: "codex", Limit: 50})
		require.NoError(t, err)
		ids := make([]string, 0, len(page.Sessions))
		for _, s := range page.Sessions {
			ids = append(ids, s.ID)
		}
		return ids
	}
	wantIDs := []string{paginationSessionID(0), pageID}

	// Codex archiving moves the page into the flat archived root.
	archivedPath := filepath.Join(archived, filepath.Base(pagePath))
	require.NoError(t, os.Rename(pagePath, archivedPath))
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	assert.ElementsMatch(t, wantIDs, codexSessionIDs())
	assert.Equal(t, archivedPath, database.GetSessionFilePath(t.Context(), pageID))
	assertCodexPaginationUsage(t, database, 2, false)

	// With copies in both roots the dated copy wins.
	content, err := os.ReadFile(archivedPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pagePath, content, 0o600))
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	assert.ElementsMatch(t, wantIDs, codexSessionIDs())
	assert.Equal(t, pagePath, database.GetSessionFilePath(t.Context(), pageID))
	sess, err := database.GetSessionFull(t.Context(), pageID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Nil(t, sess.SourceMissingAt)
	assertCodexPaginationUsage(t, database, 2, false)
}

func TestCodexRevertPageSourceMtimeTracksThePage(t *testing.T) {
	root := t.TempDir()
	writeCodexUsagePage(t, root, 0, true, false)
	pagePath := writeCodexUsagePage(t, root, 1, true, false)
	database := openTestDB(t)
	engine := newCodexRevertEngine(t, database, root)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	pageID := paginationSessionID(1)
	before := engine.SourceMtime(t.Context(), pageID)
	require.NotZero(t, before)
	content, err := os.ReadFile(pagePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pagePath, append(content, '\n'), 0o600))
	later := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(pagePath, later, later))
	after := engine.SourceMtime(t.Context(), pageID)
	assert.NotEqual(t, before, after, "polling follows the page's own file")
}

func TestSyncPathsCodexIndexEventRefreshesRevertPageName(t *testing.T) {
	const (
		otherThread = "66666666-6666-4666-8666-666666666666"
		otherPage   = "77777777-7777-4777-8777-777777777777"
	)
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	headPath := writeCodexUsagePage(t, root, 0, true, true)
	page1Path := writeCodexUsagePage(t, root, 1, true, true)
	page2Path := writeCodexUsagePage(t, root, 2, true, true)
	writeCodexRollout(t, root, "24", "rollout-2026-09-24T13-00-00-"+otherThread+".jsonl",
		testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(otherThread, "/work/project", "codex_cli_rs", "2026-09-24T05:00:00Z"),
			codexUserEntryJSON("2026-09-24T05:00:01Z", "other thread"),
		))
	writeCodexRollout(t, root, "24", "rollout-2026-09-24T14-00-00-"+otherThread+"_"+otherPage+".jsonl",
		testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaWithFieldsJSON(otherThread, "/work/project", "codex_cli_rs", "2026-09-24T06:00:00Z",
				map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": otherThread}}),
			codexUserEntryJSON("2026-09-24T06:00:01Z", "other page"),
		))
	indexPath := filepath.Join(home, parser.CodexSessionIndexFilename)
	writeIndex := func(title string, mtime time.Time) {
		t.Helper()
		require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
			`{"id":"%s","thread_name":"%s"}`+"\n"+`{"id":"%s","thread_name":"Other title"}`+"\n",
			paginationThread, title, otherThread), 0o644))
		require.NoError(t, os.Chtimes(indexPath, mtime, mtime))
		parser.EvictCodexSessionIndex(indexPath)
	}
	writeIndex("Original title", time.Now().Add(-2*time.Hour))

	database := openTestDB(t)
	engine := newCodexRevertEngine(t, database, root)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	sessionName := func(id string) string {
		t.Helper()
		name, found, err := database.GetSessionName(t.Context(), id)
		require.NoError(t, err)
		require.True(t, found, "session %s", id)
		return name
	}
	threadIDs := []string{paginationSessionID(0), paginationSessionID(1), paginationSessionID(2)}
	otherIDs := []string{"codex:" + otherThread, "codex:" + otherThread + "_" + otherPage}
	for _, id := range threadIDs {
		assert.Equal(t, "Original title", sessionName(id), "pages take the thread's index title")
	}
	for _, id := range otherIDs {
		assert.Equal(t, "Other title", sessionName(id))
	}

	writeIndex("Renamed title", time.Now().Add(-30*time.Minute))
	var classified []string
	for _, f := range engine.classifyCodexIndexPath(t.Context(), indexPath) {
		classified = append(classified, f.Path)
	}
	assert.ElementsMatch(t, []string{headPath, page1Path, page2Path}, classified,
		"a rename reaches the thread's pages and nothing else")

	engine.SyncPaths([]string{indexPath})
	for _, id := range threadIDs {
		assert.Equal(t, "Renamed title", sessionName(id))
	}
	for _, id := range otherIDs {
		assert.Equal(t, "Other title", sessionName(id))
	}
}

func TestCodexRevertPageUpgradeReplacesStaleThreadRow(t *testing.T) {
	const goneOrphan = "99999999-9999-4999-8999-999999999999"
	for _, tc := range []struct {
		name                     string
		headMissing, pageMissing bool
	}{
		{"head_present", false, false},
		{"head_missing", true, false},
		{"page_missing", false, true},
		{"both_missing", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var headPath, lastPath string
			for page := range 3 {
				lastPath = writeCodexUsagePage(t, root, page, true, true)
				if page == 0 {
					headPath = lastPath
				}
			}
			database := openTestDB(t)
			cfg := EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
				StagedCodexParseMinBytes: 1,
			}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			engine.Close()

			// Rebuild the pre-fix archive: codex:<T> holds the last-synced page's
			// messages and path, and no page session exists.
			threadID := paginationSessionID(0)
			lastMessages, err := database.GetAllMessages(t.Context(), paginationSessionID(2))
			require.NoError(t, err)
			for i := range lastMessages {
				lastMessages[i].SessionID = threadID
				lastMessages[i].Ordinal = i
			}
			require.NoError(t, database.ReplaceSessionMessages(t.Context(), threadID, lastMessages))
			savedName, note := "Saved thread", "Keep this reply"
			require.NoError(t, database.RenameSession(t.Context(), threadID, &savedName))
			starred, err := database.StarSession(t.Context(), threadID)
			require.NoError(t, err)
			require.True(t, starred)
			pinnedMessages, err := database.GetAllMessages(t.Context(), threadID)
			require.NoError(t, err)
			require.Len(t, pinnedMessages, 2)
			pinID, err := database.PinMessage(t.Context(), threadID, pinnedMessages[1].ID, &note)
			require.NoError(t, err)
			require.NotZero(t, pinID)
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "retained", Agent: "gemini", Project: "sample", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
				{SessionID: "retained", Role: "user", Content: "Retain this archived message"},
			}))
			gonePath := filepath.Join(root, "2026", "09", "20", "rollout-2026-09-20T12-00-00-"+goneOrphan+".jsonl")
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "codex:" + goneOrphan, Agent: "codex", Project: "sample", Machine: "local",
				MessageCount: 1, FilePath: &gonePath,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
				{SessionID: "codex:" + goneOrphan, Role: "user", Content: "Codex orphan"},
			}))
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = raw.Close() })
			_, err = raw.ExecContext(t.Context(), "PRAGMA foreign_keys = ON")
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), `DELETE FROM sessions WHERE id IN (?, ?)`,
				paginationSessionID(1), paginationSessionID(2))
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET file_path=?,message_count=2,user_message_count=1,data_version=129 WHERE id=?`, lastPath, threadID)
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), "PRAGMA user_version = 129")
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			if tc.headMissing {
				require.NoError(t, os.Remove(headPath))
			}
			if tc.pageMissing {
				require.NoError(t, os.Remove(lastPath))
			}

			reopened, err := db.OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			require.True(t, reopened.NeedsResync())
			upgraded := NewEngine(t.Context(), reopened, cfg)
			t.Cleanup(upgraded.Close)
			stats, err := upgraded.SyncThenRun(t.Context(), false, nil, func(full bool) error {
				assert.True(t, full)
				return nil
			})
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.False(t, stats.Aborted)

			metadataID := threadID
			if tc.headMissing {
				metadataID = paginationSessionID(2)
			}
			curated, err := reopened.GetSession(t.Context(), metadataID)
			require.NoError(t, err)
			require.NotNil(t, curated)
			assert.Equal(t, &savedName, curated.DisplayName)
			stars, err := reopened.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{metadataID}, stars)
			pins, err := reopened.ListPinnedMessages(t.Context(), "", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, paginationSessionID(2), pins[0].SessionID)
			assert.Equal(t, new("Response 2"), pins[0].Content)
			assert.Equal(t, &note, pins[0].Note)

			if tc.headMissing {
				stale, err := reopened.GetSession(t.Context(), threadID)
				require.NoError(t, err)
				assert.Nil(t, stale, "the stale thread row whose file is a page is dropped")
				for page := 1; page < 3; page++ {
					assert.Equal(t,
						[]string{fmt.Sprintf("Request %d", page), fmt.Sprintf("Response %d", page)},
						messageContents(t, reopened, paginationSessionID(page)))
				}
				got, _ := dailyOutputTokens(t, reopened)
				assert.Equal(t, map[string]int{"2026-09-25": 20, "2026-09-26": 30}, got)
			} else if tc.pageMissing {
				assert.Equal(t, []string{"Request 2", "Response 2"}, messageContents(t, reopened, paginationSessionID(2)))
				got, _ := dailyOutputTokens(t, reopened)
				assert.Equal(t, map[string]int{"2026-09-24": 10, "2026-09-25": 20, "2026-09-26": 30}, got)
				page, err := reopened.GetSession(t.Context(), paginationSessionID(2))
				require.NoError(t, err)
				require.NotNil(t, page)
				assert.Equal(t, new(threadID), page.ParentSessionID, "missing base falls back to the thread")
				assert.Equal(t, headPath, reopened.GetSessionFilePath(t.Context(), threadID))
			} else {
				assertCodexPaginationUsage(t, reopened, 3, true)
				assert.Equal(t, headPath, reopened.GetSessionFilePath(t.Context(), threadID))
			}
			assert.Equal(t, []string{"Retain this archived message"}, messageContents(t, reopened, "retained"))
			assert.Equal(t, []string{"Codex orphan"}, messageContents(t, reopened, "codex:"+goneOrphan))
		})
	}
}

func TestCodexRevertPageRebuildPreservesDeletionState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		version     int
		trash       bool
		prefix      string
		pagePath    bool
		missingPage bool
	}{
		{"upgrade_deleted", 129, false, "", false, false},
		{"upgrade_trashed", 129, true, "", false, false},
		{"current_deleted", 130, false, "", false, false},
		{"current_trashed", 130, true, "", false, false},
		{"remote_upgrade_deleted", 129, false, "host_a~", false, false},
		{"remote_upgrade_trashed", 129, true, "host_a~", false, false},
		{"upgrade_trashed_page_path", 129, true, "", true, false},
		{"upgrade_deleted_missing_page", 129, false, "", false, true},
		{"current_deleted_missing_page", 130, false, "", false, true},
		{"remote_upgrade_deleted_missing_page", 129, false, "host_a~", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			headPath := writeCodexUsagePage(t, root, 0, true, true)
			var missingPath string
			var missingContent []byte
			for page := 1; page < 3; page++ {
				pagePath := writeCodexUsagePage(t, root, page, true, true)
				if tc.missingPage && page == 2 {
					missingPath = pagePath
					var err error
					missingContent, err = os.ReadFile(pagePath)
					require.NoError(t, err)
					require.NoError(t, os.Remove(pagePath))
				}
				if tc.pagePath {
					headPath = pagePath
				}
			}
			database := openTestDB(t)
			threadID := tc.prefix + paginationSessionID(0)
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: threadID, Agent: "codex", Project: "sample", Machine: "local",
				FilePath: &headPath, MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
				{SessionID: threadID, Role: "user", Content: "Hidden thread"},
			}))
			if tc.trash {
				require.NoError(t, database.SoftDeleteSession(t.Context(), threadID))
			} else {
				require.NoError(t, database.DeleteSession(t.Context(), threadID))
			}
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", tc.version))
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			reopened, err := db.OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			engine := NewEngine(t.Context(), reopened, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "local", IDPrefix: tc.prefix,
			})
			t.Cleanup(engine.Close)
			stats, err := engine.SyncThenRun(t.Context(), true, nil, func(bool) error { return nil })
			require.NoError(t, err)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)

			for page := 1; page < 3; page++ {
				id := tc.prefix + paginationSessionID(page)
				session, err := reopened.GetSession(t.Context(), id)
				require.NoError(t, err)
				if tc.version < 130 {
					assert.Nil(t, session, "an upgrade must keep every page hidden")
					if tc.pagePath && page == 2 {
						// The saved transcript belongs to this page, while the
						// thread row retains the inherited trash scope.
						assert.True(t, reopened.IsSessionTrashed(t.Context(), threadID))
						assert.Equal(t, []string{"Hidden thread"}, messageContents(t, reopened, id))
						continue
					}
					if tc.trash {
						assert.True(t, reopened.IsSessionTrashed(t.Context(), id))
						assert.Equal(t, []string{fmt.Sprintf("Request %d", page), fmt.Sprintf("Response %d", page)},
							messageContents(t, reopened, id), "trash retains the page's content")
					} else {
						assert.True(t, reopened.IsSessionExcluded(t.Context(), id))
					}
				} else if !tc.missingPage || page != 2 {
					assert.NotNil(t, session, "deletion after the upgrade applies only to the selected file")
				}
			}
			if tc.missingPage {
				// A later rebuild must retain the old deletion scope even while
				// the page is still absent and the archive is already upgraded.
				stats, err = engine.SyncThenRun(t.Context(), true, nil, func(bool) error { return nil })
				require.NoError(t, err)
				require.False(t, stats.Aborted)
				require.Zero(t, stats.Failed)
				require.NoError(t, os.WriteFile(missingPath, missingContent, 0o600))
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				page, err := reopened.GetSession(t.Context(), tc.prefix+paginationSessionID(2))
				require.NoError(t, err)
				if tc.version < 130 {
					assert.Nil(t, page, "a returning file must stay deleted after the upgrade")
				} else {
					require.NotNil(t, page, "new per-file deletions must allow sibling pages")
					assert.Equal(t, []string{"Request 2", "Response 2"}, messageContents(t, reopened, page.ID))
				}
			}
		})
	}
}

func TestCodexRevertPageSpawnedSubagentLinksToThePage(t *testing.T) {
	const childID = "88888888-8888-4888-8888-888888888888"
	root := t.TempDir()
	writeCodexUsagePage(t, root, 0, true, false)
	writeCodexRollout(t, root, "25",
		"rollout-2026-09-25T12-00-00-"+paginationThread+"_"+paginationRollouts[1]+".jsonl",
		testjsonl.JoinJSONL(
			codexRevertMeta("2026-09-25T04:00:00Z", paginationThread, 5),
			codexUserEntryJSON("2026-09-25T04:00:01Z", "delegate"),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn",
				map[string]any{"task_name": "helper"}, "2026-09-25T04:00:02Z"),
			testjsonl.CodexSubagentActivityJSON("started", "call_spawn", childID,
				"/root/helper", "2026-09-25T04:00:03Z"),
		))
	writeCodexRollout(t, root, "25", "rollout-2026-09-25T12-01-00-"+childID+".jsonl",
		testjsonl.JoinJSONL(
			testjsonl.CodexSubagentSessionMetaJSON(childID, paginationThread,
				"/work/project", "codex_cli_rs", "2026-09-25T04:00:03Z"),
			codexAssistantEntryJSON("2026-09-25T04:00:04Z", "done"),
		))

	database := openTestDB(t)
	engine := newCodexRevertEngine(t, database, root)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	child, err := database.GetSession(t.Context(), "codex:"+childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, paginationSessionID(1), *child.ParentSessionID,
		"the spawn edge in the page outranks the thread id in the child's meta")
	assert.Equal(t, "subagent", child.RelationshipType)
}
