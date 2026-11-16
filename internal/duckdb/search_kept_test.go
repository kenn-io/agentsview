//go:build !(windows && arm64)

package duckdb

import (
	"encoding/base64"
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
}

func TestSearchKeptUnversioned(t *testing.T) {
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
		require.NoError(t, store.Close())
	}
}

func TestSearchKeptSelectionsAcrossRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, path := newPushFixture(t, 3)
	result, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	cases := []struct {
		filter db.SearchFilter
		ids    []string
		cursor int
	}{
		{db.SearchFilter{Query: "first"}, []string{"sess-3", "sess-2", "sess-1"}, 0},
		{db.SearchFilter{Query: "missing"}, nil, 0},
		{db.SearchFilter{Query: "first", Project: "other"}, nil, 0},
		{db.SearchFilter{Query: "first", DateFrom: "2030-01-01"}, nil, 0},
		{db.SearchFilter{Query: "first", Sort: "recency"}, []string{"sess-3", "sess-2", "sess-1"}, 0},
		{db.SearchFilter{Query: "first", Cursor: 2, Limit: 2}, []string{"sess-1"}, 0},
		{db.SearchFilter{Query: "first", Limit: 2}, []string{"sess-3", "sess-2"}, 2},
	}
	first := openUsagePartsStore(t, path)
	pages := make([]db.SearchPage, len(cases))
	for i, tc := range cases {
		pages[i], err = first.Search(t.Context(), tc.filter)
		require.NoError(t, err)
		require.Len(t, pages[i].Results, len(tc.ids))
		for j, id := range tc.ids {
			require.Equal(t, id, pages[i].Results[j].SessionID)
		}
		require.Equal(t, tc.cursor, pages[i].NextCursor)
	}
	require.NoError(t, first.Close())
	restarted := openUsagePartsStore(t, path)
	defer restarted.Close()
	reads := 0
	restarted.afterSearchRead = func() { reads++ }
	for i, tc := range cases {
		page, err := restarted.Search(t.Context(), tc.filter)
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

func TestSearchKeptContentChangedDuringPush(t *testing.T) {
	for _, kind := range []string{"image", "message", "is_system", "is_system_prefixed"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			ctx := t.Context()
			local := newLocalDB(t)
			const id, ts = "screenshot", "2026-01-02T03:04:05.000Z"
			content := `[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
			text := "apple"
			if kind == "is_system_prefixed" {
				text = "<task-notification>apple"
			}
			require.NoError(t, local.UpsertSession(ctx, syncSession(id, "shots", "look", ts, 1)))
			message := syncMessage(id, 0, "assistant", text, ts, db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "shot", ResultContent: content})
			require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{message}))
			require.NoError(t, local.BackfillImageTextOccurrences(ctx))
			hash := db.RetainedToolResultImages(content)[0].SHA256
			identity := ocr.Identity{Fingerprint: "proc-a", Provider: "tesseract", Model: "eng", Revision: "r1"}
			require.NoError(t, local.SaveImageText(ctx, hash, identity, "apple", false))
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
			switch kind {
			case "image":
				require.NoError(t, local.SaveImageText(ctx, hash, identity, "banana", false))
			case "message":
				message.Content = "banana"
			case "is_system":
				message.IsSystem = true
			case "is_system_prefixed":
				message.Role = "user"
			}
			if kind != "image" {
				require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{message}))
			}
			_, err = syncer.pushSingleSession(ctx, sessions[0], fingerprints[id])
			require.NoError(t, err)
			require.NoError(t, syncer.Close())
			filter := db.SearchFilter{Query: "banana"}
			visibility := kind == "is_system" || kind == "is_system_prefixed"
			if visibility {
				filter.Query = "apple"
			}
			store := openUsagePartsStore(t, path)
			page, err := store.Search(ctx, filter)
			require.NoError(t, err)
			if visibility {
				require.Empty(t, page.Results)
			} else {
				require.Len(t, page.Results, 1)
			}
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
			if visibility {
				require.Len(t, page.Results, 1)
				require.Equal(t, id, page.Results[0].SessionID)
			} else {
				require.Empty(t, page.Results)
			}
		})
	}
}
