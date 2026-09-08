package sync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestProviderAuthoritativeUnchangedSessionSkipsOnResync verifies that a
// provider-authoritative agent whose source file is unchanged is skipped on a
// second full sync rather than reparsed and rewritten. Before the generic
// providerSourceUnchangedInDB freshness check, only Claude and Cowork had a
// pre-parse DB skip in processProviderFile, so the other migrated agents
// (OpenHands, Cursor, Hermes, Vibe) fell through to provider.Parse + writeBatch
// and rewrote unchanged sessions on every full/periodic sync. Vibe is used as a
// representative of that group.
func TestProviderAuthoritativeUnchangedSessionSkipsOnResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	vibeDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVibe: {vibeDir},
		},
		Machine: "local",
	})

	sessionID := "abc123def-0000-0000-0000-000000000000"
	writeVibeSyncFixture(
		t, vibeDir, "session_20260616_083518_abc123", sessionID, "Title",
	)

	ctx := context.Background()
	first := engine.SyncAll(ctx, nil)
	require.Equal(t, 1, first.Synced, "first sync parses and stores the session")

	// Source files are untouched, so the second full sync must skip the session
	// at the DB-freshness check instead of reparsing and rewriting it.
	second := engine.SyncAll(ctx, nil)
	assert.Equal(t, 0, second.Synced,
		"an unchanged provider-authoritative session must not be re-synced")
	assert.GreaterOrEqual(t, second.Skipped, 1,
		"the unchanged session must be counted as skipped")
}

func TestCursorSameMtimeHashChangeReparses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	const sessionID = "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(
		root, "Users-demo-Code-app", "agent-transcripts", sessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	writeTranscript := func(first string) {
		require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(
			`{"role":"user","message":{"content":"<user_query>%s</user_query>"}}`+"\n"+
				`{"role":"assistant","message":{"content":"Done."}}`+"\n",
			first,
		)), 0o644))
	}
	writeTranscript("one!")
	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, mtime, mtime))

	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCursor: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	ctx := context.Background()
	first := engine.SyncAll(ctx, nil)
	require.Equal(t, 1, first.Synced)
	before, err := testDB.GetSession(ctx, "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.FirstMessage)
	assert.Equal(t, "one!", *before.FirstMessage)

	writeTranscript("two!")
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	engine.SyncPaths([]string{path})

	after, err := testDB.GetSession(ctx, "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.FirstMessage)
	assert.Equal(t, "two!", *after.FirstMessage,
		"a same-mtime content change must bypass Cursor freshness")
}

func TestCursorStoreEnrichmentFailureStillArchivesTranscriptUpdates(t *testing.T) {
	for _, failure := range []string{"chats is a file", "unsupported metadata"} {
		t.Run(failure, func(t *testing.T) {
			cursorDir := filepath.Join(t.TempDir(), ".cursor")
			root := filepath.Join(cursorDir, "projects")
			const sessionID = "11111111-2222-4333-8444-555555555555"
			path := filepath.Join(root, "Users-demo-Code-app", "agent-transcripts", sessionID+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			transcript := `{"role":"user","message":{"content":"<user_query>First prompt</user_query>"}}` + "\n" +
				`{"role":"assistant","message":{"content":"First answer"}}` + "\n"
			require.NoError(t, os.WriteFile(path, []byte(transcript), 0o644))
			chats := filepath.Join(cursorDir, "chats")
			if failure == "chats is a file" {
				require.NoError(t, os.WriteFile(chats, []byte("file"), 0o644))
			} else {
				storePath := filepath.Join(chats, "workspace", sessionID, "store.db")
				require.NoError(t, os.MkdirAll(filepath.Dir(storePath), 0o755))
				store, err := sql.Open("sqlite3", storePath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				_, err = store.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
					CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB);
					INSERT INTO meta VALUES ('0', 'not-hex');`)
				require.NoError(t, err)
				require.NoError(t, store.Close())
			}

			archive := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			first := engine.SyncAll(t.Context(), nil)
			require.Equal(t, 1, first.Synced)
			messages, err := archive.GetMessages(t.Context(), "cursor:"+sessionID, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "First answer", messages[1].Content)

			transcript += `{"role":"user","message":{"content":"<user_query>Next prompt</user_query>"}}` + "\n" +
				`{"role":"assistant","message":{"content":"Next answer"}}` + "\n"
			require.NoError(t, os.WriteFile(path, []byte(transcript), 0o644))
			engine.SyncPaths([]string{path})
			messages, err = archive.GetMessages(t.Context(), "cursor:"+sessionID, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 4)
			assert.Equal(t, "Next answer", messages[3].Content)
		})
	}
}
