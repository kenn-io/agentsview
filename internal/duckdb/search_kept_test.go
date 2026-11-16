//go:build !(windows && arm64)

package duckdb

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/docbank/document/ocr"
)

func TestSearchKeptMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	buildMirrorFixture(t, path, "session-a", "answer")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	loads := 0
	store.searchReads.Size = func(db.SearchPage) int64 { loads++; return 1 }
	filter := db.SearchFilter{Query: "answer"}
	first, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, first.Results, 1)
	require.Equal(t, "session-a", first.Results[0].SessionID)
	repeated, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	require.Equal(t, 1, loads)
	next := filepath.Join(dir, "next.duckdb")
	buildMirrorFixture(t, next, "session-b", "answer")
	require.NoError(t, swapMirrorFile(next, path))
	changed, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, changed.Results, 1)
	require.Equal(t, "session-b", changed.Results[0].SessionID)
	require.Equal(t, 2, loads)
}

func TestSearchKeptReplacementDuringRead(t *testing.T) {
	skipReopenTestOnWindows(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	next := filepath.Join(dir, "next.duckdb")
	original := filepath.Join(dir, "original.duckdb")
	buildMirrorFixture(t, path, "session-a", "answer")
	buildMirrorFixture(t, next, "session-b", "answer")
	info, err := os.Stat(path)
	require.NoError(t, err)
	nextInfo, err := os.Stat(next)
	require.NoError(t, err)
	require.Equal(t, info.Size(), nextInfo.Size())
	require.NoError(t, os.Chtimes(next, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Link(path, original))
	store := openUsagePartsStore(t, path)
	store.afterSearchRead = func() {
		store.afterSearchRead = nil
		require.NoError(t, swapMirrorFile(next, path))
		store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	}
	filter := db.SearchFilter{Query: "answer"}
	page, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "session-b", page.Results[0].SessionID)
	require.NoError(t, store.Close())
	require.NoError(t, swapMirrorFile(original, path))
	restarted := openUsagePartsStore(t, path)
	page, err = restarted.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "session-a", page.Results[0].SessionID)
	require.NoError(t, restarted.Close())
	restarted = openUsagePartsStore(t, path)
	defer restarted.Close()
	reads := 0
	restarted.afterSearchRead = func() { reads++ }
	page, err = restarted.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, "session-a", page.Results[0].SessionID)
	require.Zero(t, reads)
}

func TestSearchKeptUnversionedAndBorrowed(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session-a", "answer")
	writeUsageMirror(t, path, `UPDATE sessions SET agentsview_push_fingerprint = ''`)
	filter := db.SearchFilter{Query: "answer"}
	for range 2 {
		store := openUsagePartsStore(t, path)
		reads := 0
		store.afterSearchRead = func() { reads++ }
		page, err := store.Search(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Equal(t, 1, reads)
		borrowed := NewStoreFromDB(store.DB())
		borrowed.afterSearchRead = func() { reads++ }
		for range 2 {
			_, err = borrowed.Search(t.Context(), filter)
			require.NoError(t, err)
		}
		require.Equal(t, 3, reads)
		require.NoError(t, store.Close())
	}
}

func TestSearchKeptSelectionsAcrossRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, path := newPushFixture(t, 3)
	messages, err := local.GetAllMessages(t.Context(), "sess-3")
	require.NoError(t, err)
	messages[0].Content = "prefix first sess-3"
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "sess-3", messages))
	result, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	filters := []db.SearchFilter{
		{Query: "first"}, {Query: "missing"}, {Query: "first", Project: "other"},
		{Query: "first", DateFrom: "2030-01-01"}, {Query: "first", Sort: "recency"},
		{Query: "first", Cursor: 2, Limit: 2}, {Query: "first", Limit: 2},
	}
	first := openUsagePartsStore(t, path)
	pages := make([]db.SearchPage, len(filters))
	for i, f := range filters {
		pages[i], err = first.Search(t.Context(), f)
		require.NoError(t, err)
	}
	require.Len(t, pages[0].Results, 3)
	for i, id := range []string{"sess-2", "sess-1", "sess-3"} {
		require.Equal(t, id, pages[0].Results[i].SessionID)
		snippet := "first " + id
		if id == "sess-3" {
			snippet = "prefix " + snippet
		}
		require.Equal(t, snippet, pages[0].Results[i].Snippet)
	}
	require.Empty(t, pages[1].Results)
	require.Empty(t, pages[2].Results)
	require.Empty(t, pages[3].Results)
	require.Len(t, pages[4].Results, 3)
	for i, id := range []string{"sess-3", "sess-2", "sess-1"} {
		require.Equal(t, id, pages[4].Results[i].SessionID)
	}
	require.Len(t, pages[5].Results, 1)
	require.Equal(t, "sess-3", pages[5].Results[0].SessionID)
	require.Zero(t, pages[5].NextCursor)
	require.Len(t, pages[6].Results, 2)
	require.Equal(t, "sess-2", pages[6].Results[0].SessionID)
	require.Equal(t, "sess-1", pages[6].Results[1].SessionID)
	require.Equal(t, 2, pages[6].NextCursor)
	require.NoError(t, first.Close())
	restarted := openUsagePartsStore(t, path)
	defer restarted.Close()
	reads := 0
	restarted.afterSearchRead = func() { reads++ }
	for i, f := range filters {
		page, err := restarted.Search(t.Context(), f)
		require.NoError(t, err)
		require.Equal(t, pages[i], page)
	}
	require.Zero(t, reads)
}

func TestSearchKeptPushInvalidation(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	push := func() {
		t.Helper()
		result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	check := func(query string, ids ...string) {
		t.Helper()
		filter := db.SearchFilter{Query: query}
		store := openUsagePartsStore(t, path)
		reads := 0
		store.afterSearchRead = func() { reads++ }
		page, err := store.Search(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, 1, reads)
		require.Len(t, page.Results, len(ids))
		for i, id := range ids {
			require.Equal(t, id, page.Results[i].SessionID)
		}
		require.NoError(t, store.Close())
		store = openUsagePartsStore(t, path)
		store.afterSearchRead = func() { reads++ }
		got, err := store.Search(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, page, got)
		require.Equal(t, 1, reads)
		require.NoError(t, store.Close())
	}
	push()
	check("first", "sess-1")
	check("fresh")
	appendSessionMessage(t, local, "sess-1", "user", "fresh content", db.SessionSignalUpdate{})
	push()
	check("first", "sess-1")
	check("fresh", "sess-1")
	check("renamed")
	name := "renamed unique"
	require.NoError(t, local.RenameSession(ctx, "sess-1", &name))
	push()
	check("fresh", "sess-1")
	check("renamed", "sess-1")
	check("newmatch")
	const ts = "2026-02-02T00:00:00.000Z"
	require.NoError(t, local.UpsertSession(ctx, syncSession("sess-2", "alpha", "newmatch", ts, 1)))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "sess-2", []db.Message{syncMessage("sess-2", 0, "user", "newmatch", ts)}))
	push()
	check("newmatch", "sess-2")
	require.NoError(t, local.DeleteSession(ctx, "sess-1"))
	push()
	check("fresh")
}

func TestSearchKeptImageTextPush(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	local := newLocalDB(t)
	const id, ts = "screenshot", "2026-01-02T03:04:05.000Z"
	content := `[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
	require.NoError(t, local.UpsertSession(ctx, syncSession(id, "shots", "look", ts, 1)))
	require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{
		syncMessage(id, 0, "assistant", "reading", ts, db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "shot", ResultContent: content}),
	}))
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	push := func() {
		t.Helper()
		result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	push()
	filter := db.SearchFilter{Query: "invoice"}
	store := openUsagePartsStore(t, path)
	page, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, page.Results)
	require.NoError(t, store.Close())
	hash := db.RetainedToolResultImages(content)[0].SHA256
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, hash, ocr.Identity{Fingerprint: "proc-a", Provider: "tesseract", Model: "eng", Revision: "r1"}, "invoice 4827", false))
	push()
	store = openUsagePartsStore(t, path)
	reads := 0
	store.afterSearchRead = func() { reads++ }
	page, err = store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, id, page.Results[0].SessionID)
	require.Equal(t, "invoice 4827", page.Results[0].Snippet)
	require.Equal(t, 1, reads)
	require.NoError(t, store.Close())
	store = openUsagePartsStore(t, path)
	defer store.Close()
	store.afterSearchRead = func() { reads++ }
	got, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, page, got)
	require.Equal(t, 1, reads)
}

func TestSearchKeptContentChangedDuringPush(t *testing.T) {
	for _, kind := range []string{"image", "message"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			ctx := t.Context()
			local := newLocalDB(t)
			const id, ts = "screenshot", "2026-01-02T03:04:05.000Z"
			content := `[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
			require.NoError(t, local.UpsertSession(ctx, syncSession(id, "shots", "look", ts, 1)))
			message := syncMessage(id, 0, "assistant", "apple", ts, db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "shot", ResultContent: content})
			require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{message}))
			require.NoError(t, local.BackfillImageTextOccurrences(ctx))
			hash := db.RetainedToolResultImages(content)[0].SHA256
			identity := ocr.Identity{Fingerprint: "proc-a", Provider: "tesseract", Model: "eng", Revision: "r1"}
			require.NoError(t, local.SaveImageText(ctx, hash, identity, "apple", false))
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			syncer := newTestSync(t, path, local, storage.MirrorPushOptions{})
			sessions, err := local.ListSessionsForMirrorWindow(ctx, "", nil, nil)
			require.NoError(t, err)
			fingerprints, err := syncer.sessionFingerprints(ctx, sessions)
			require.NoError(t, err)
			setText := func(text string) {
				t.Helper()
				if kind == "image" {
					require.NoError(t, local.SaveImageText(ctx, hash, identity, text, false))
				} else {
					message.Content = text
					require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{message}))
				}
			}
			setText("banana")
			_, err = syncer.pushSingleSession(ctx, sessions[0], fingerprints[id])
			require.NoError(t, err)
			require.NoError(t, syncer.Close())
			filter := db.SearchFilter{Query: "banana"}
			store := openUsagePartsStore(t, path)
			page, err := store.Search(ctx, filter)
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			require.NoError(t, store.Close())
			setText("apple")
			result, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			// Restoring content can reproduce the earlier fingerprint even though
			// the normal local writers also advance other session metadata.
			writeUsageMirror(t, path, fmt.Sprintf("UPDATE sessions SET agentsview_push_fingerprint = '%s'", fingerprints[id]))
			store = openUsagePartsStore(t, path)
			defer store.Close()
			page, err = store.Search(ctx, filter)
			require.NoError(t, err)
			require.Empty(t, page.Results)
		})
	}
}

func TestSearchKeptVisibilityChangedDuringPush(t *testing.T) {
	for _, flag := range []string{"is_system", "is_system_prefixed"} {
		t.Run(flag, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			ctx := t.Context()
			local := newLocalDB(t)
			const id, ts = "visibility", "2026-01-02T03:04:05.000Z"
			message := syncMessage(id, 0, "assistant", "<task-notification>uniqueterm", ts)
			require.NoError(t, local.UpsertSession(ctx, syncSession(id, "project", "session", ts, 1)))
			require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{message}))
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			result, err := Push(ctx, path, local, "test-machine", storage.MirrorPushOptions{}, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			backup := filepath.Join(t.TempDir(), "backup.db")
			require.NoError(t, local.SnapshotTo(ctx, backup))
			syncer := newTestSync(t, path, local, storage.MirrorPushOptions{})
			sessions, err := local.ListSessionsForMirrorWindow(ctx, "", nil, nil)
			require.NoError(t, err)
			fingerprints, err := syncer.sessionFingerprints(ctx, sessions)
			require.NoError(t, err)
			if flag == "is_system" {
				message.IsSystem = true
			} else {
				message.Role = "user"
			}
			require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{message}))
			_, err = syncer.pushSingleSession(ctx, sessions[0], fingerprints[id])
			require.NoError(t, err)
			require.NoError(t, syncer.Close())
			filter := db.SearchFilter{Query: "uniqueterm"}
			store := openUsagePartsStore(t, path)
			page, err := store.Search(ctx, filter)
			require.NoError(t, err)
			require.Empty(t, page.Results)
			require.NoError(t, store.Close())
			restored, err := db.OpenIsolated(ctx, backup)
			require.NoError(t, err)
			defer restored.Close()
			result, err = Push(ctx, path, restored, "test-machine", storage.MirrorPushOptions{}, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store = openUsagePartsStore(t, path)
			defer store.Close()
			page, err = store.Search(ctx, filter)
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			require.Equal(t, id, page.Results[0].SessionID)
		})
	}
}
