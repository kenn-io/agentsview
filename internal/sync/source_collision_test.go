package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const collisionBaseID = "gemini:shared-session"

// geminiCollisionSession renders a Gemini chat file for sessionID with the
// requested number of alternating user/assistant messages.
func geminiCollisionSession(sessionID string, messages int) string {
	msgs := make([]map[string]any, 0, messages)
	for i := range messages {
		id := "m" + string(rune('a'+i))
		if i%2 == 0 {
			msgs = append(msgs, testjsonl.GeminiUserMsg(id, tsEarly, "question "+id))
		} else {
			msgs = append(msgs, testjsonl.GeminiAssistantMsg(id, tsEarlyS5, "answer "+id, nil))
		}
	}
	return testjsonl.GeminiSessionJSON(sessionID, "collisionhash", tsEarly, tsEarlyS5, msgs)
}

// collisionEnv syncs two Gemini files that record the same session id, the
// first in its own pass, and returns the stored path of each. The second file
// sorts first so a rebuild discovers the files in the other order.
func collisionEnv(t *testing.T, firstMessages, secondMessages int) (env *testEnv, first, second string) {
	t.Helper()
	env = setupTestEnv(t)
	first = env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-05-first.json"),
		geminiCollisionSession("shared-session", firstMessages))
	env.engine.SyncAll(t.Context(), nil)
	second = env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-00-second.json"),
		geminiCollisionSession("shared-session", secondMessages))
	env.engine.SyncAll(t.Context(), nil)
	return env, first, second
}

func requireStoredSession(t *testing.T, database *db.DB, id string) *db.Session {
	t.Helper()
	sess, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess, "session %q", id)
	return sess
}

// assertCollisionPair checks that the base id holds basePath's transcript and
// the other file is a linked continuation under its derived id.
func assertCollisionPair(t *testing.T, database *db.DB, basePath string, baseCount int, altPath string, altCount int) {
	t.Helper()
	base := requireStoredSession(t, database, collisionBaseID)
	require.NotNil(t, base.FilePath)
	assert.Equal(t, basePath, *base.FilePath)
	assertSessionMessageCount(t, database, collisionBaseID, baseCount)

	altID := parser.AltSessionID(collisionBaseID, altPath)
	alt := requireStoredSession(t, database, altID)
	require.NotNil(t, alt.FilePath)
	assert.Equal(t, altPath, *alt.FilePath)
	require.NotNil(t, alt.ParentSessionID)
	assert.Equal(t, collisionBaseID, *alt.ParentSessionID)
	assert.Equal(t, string(parser.RelContinuation), alt.RelationshipType)
	assertSessionMessageCount(t, database, altID, altCount)
}

// Two files that record one session id are both kept. The file stored first
// keeps the id whatever its length; the other becomes a linked session on the
// same pass it appears.
func TestSyncKeepsBothFilesWhenSessionIDsCollide(t *testing.T) {
	tests := []struct {
		name          string
		first, second int
	}{
		{name: "shorter file appears after the longer one", first: 5, second: 1},
		{name: "longer file appears after the shorter one", first: 1, second: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, first, second := collisionEnv(t, tt.first, tt.second)
			assertCollisionPair(t, env.db, first, tt.first, second, tt.second)

			// Later syncs and edits keep each file on its own id.
			env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", filepath.Base(second)),
				geminiCollisionSession("shared-session", tt.second+2))
			env.engine.SyncAll(t.Context(), nil)
			assertCollisionPair(t, env.db, first, tt.first, second, tt.second+2)
		})
	}
}

// A fresh archive that discovers both files in one pass keeps both.
func TestSyncKeepsBothCollidingFilesInOnePass(t *testing.T) {
	env := setupTestEnv(t)
	paths := make([]string, 0, 2)
	for i, name := range []string{"session-2026-01-01T10-00-a.json", "session-2026-01-01T10-05-b.json"} {
		paths = append(paths, env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", name),
			geminiCollisionSession("shared-session", 2*i+1)))
	}
	env.engine.SyncAll(t.Context(), nil)

	base := requireStoredSession(t, env.db, collisionBaseID)
	require.NotNil(t, base.FilePath)
	altPath := paths[0]
	if *base.FilePath == paths[0] {
		altPath = paths[1]
	}
	alt := requireStoredSession(t, env.db, parser.AltSessionID(collisionBaseID, altPath))
	assert.Equal(t, 4, base.MessageCount+alt.MessageCount)
}

// A shorter file cannot take over a missing owner's archived transcript,
// before or after the row is marked source-missing.
func TestMissingOwnerKeepsSessionID(t *testing.T) {
	for _, tombstoned := range []bool{false, true} {
		t.Run(map[bool]string{false: "not yet marked", true: "marked missing"}[tombstoned], func(t *testing.T) {
			env := setupTestEnv(t)
			dir := filepath.Join("tmp", "collisionhash", "chats")
			owner := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-owner.json"),
				geminiCollisionSession("shared-session", 5))
			env.engine.SyncAll(t.Context(), nil)
			require.NoError(t, os.Remove(owner))
			if tombstoned {
				require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{owner}))
				require.NotNil(t, requireStoredSession(t, env.db, collisionBaseID).SourceMissingAt)
			}
			paths := []string{
				env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-00-a.json"), geminiCollisionSession("shared-session", 1)),
				env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-05-b.json"), geminiCollisionSession("shared-session", 3)),
			}
			env.engine.SyncAll(t.Context(), nil)

			base := requireStoredSession(t, env.db, collisionBaseID)
			assert.Equal(t, owner, *base.FilePath)
			assertSessionMessageCount(t, env.db, collisionBaseID, 5)
			assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, paths[0]), 1)
			assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, paths[1]), 3)
		})
	}
}

// Changing the configured root after moving the agent's data keeps one session
// when the new transcript is at least as long as the archive. A shorter source
// stays separate, including when a rebuild reads ownership from the old DB.
func TestMovedSourceRootKeepsSessionID(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentGemini, parser.AgentCursor} {
		for _, mode := range []string{"sync", "resync", "watcher"} {
			for _, length := range []string{"equal", "longer", "shorter", "copy"} {
				t.Run(string(agent)+"/"+mode+"/"+length, func(t *testing.T) {
					root := t.TempDir()
					oldRoot, newRoot := filepath.Join(root, "old"), filepath.Join(root, "new")
					count := 2
					switch length {
					case "longer":
						count = 3
					case "shorter":
						count = 1
					}
					rel := filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-00-shared.json")
					original, replacement := geminiCollisionSession("shared", 2), geminiCollisionSession("shared", count)
					if agent == parser.AgentCursor {
						rel = filepath.Join("project-a", "agent-transcripts", "shared.txt")
						original = "user:\nHello\nassistant:\nHi\n"
						replacement = original
						switch length {
						case "longer":
							replacement += "user:\nMore\n"
						case "shorter":
							replacement = "user:\nHello\n"
						}
					}
					oldPath, newPath := filepath.Join(oldRoot, rel), filepath.Join(newRoot, rel)
					require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o755))
					require.NoError(t, os.WriteFile(oldPath, []byte(original), 0o644))
					database := dbtest.OpenTestDB(t)
					initial := sync.NewEngine(t.Context(), database, sync.EngineConfig{
						AgentDirs: map[parser.AgentType][]string{agent: {oldRoot}}, Machine: "local",
					})
					initial.SyncAll(t.Context(), nil)
					initial.Close()
					baseID := string(agent) + ":shared"
					assertSessionMessageCount(t, database, baseID, 2)
					_, err := database.StarSession(t.Context(), baseID)
					require.NoError(t, err)
					if length == "copy" {
						require.NoError(t, os.MkdirAll(filepath.Dir(newPath), 0o755))
					} else {
						require.NoError(t, os.Rename(oldRoot, newRoot))
					}
					require.NoError(t, os.WriteFile(newPath, []byte(replacement), 0o644))
					moved := sync.NewEngine(t.Context(), database, sync.EngineConfig{
						AgentDirs: map[parser.AgentType][]string{agent: {newRoot}}, Machine: "local",
					})
					t.Cleanup(moved.Close)
					switch mode {
					case "resync":
						stats := moved.ResyncAll(t.Context(), nil)
						require.False(t, stats.Aborted, "%v", stats.Warnings)
					case "watcher":
						require.NoError(t, moved.SyncPathsContext(t.Context(), []string{newPath}))
					default:
						moved.SyncAll(t.Context(), nil)
					}
					base := requireStoredSession(t, database, baseID)
					if length == "shorter" || length == "copy" {
						assert.Equal(t, oldPath, *base.FilePath)
						assertSessionMessageCount(t, database, baseID, 2)
						assertSessionMessageCount(t, database, parser.AltSessionID(baseID, newPath), count)
					} else {
						assert.Equal(t, newPath, *base.FilePath)
						assertSessionMessageCount(t, database, baseID, count)
						records, err := database.ListSessionPathRecords(t.Context(), baseID)
						require.NoError(t, err)
						assert.Len(t, records, 1, "moving the root must not duplicate the session")
					}
					starred, err := database.ListStarredSessionIDs(t.Context())
					require.NoError(t, err)
					assert.Contains(t, starred, baseID)
				})
			}
		}
	}
}

// A stored file that now holds a different session still owns the id it was
// stored under; another file with that id is not treated as its move.
func TestReusedOwnerPathKeepsSessionID(t *testing.T) {
	env := setupTestEnv(t)
	dir := filepath.Join("tmp", "collisionhash", "chats")
	owner := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-owner.json"),
		geminiCollisionSession("shared-session", 5))
	env.engine.SyncAll(t.Context(), nil)
	env.writeGeminiSession(t, filepath.Join(dir, filepath.Base(owner)), geminiCollisionSession("other-session", 2))
	other := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-00-new.json"),
		geminiCollisionSession("shared-session", 1))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{other}))

	assertSessionMessageCount(t, env.db, collisionBaseID, 5)
	assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, other), 1)
}

// A rebuild keeps the previous owner on the base id even when it discovers
// the other file first, so curation on the base stays with its transcript.
func TestResyncKeepsCollisionOwner(t *testing.T) {
	env, first, second := collisionEnv(t, 1, 5)
	starred, err := env.db.StarSession(t.Context(), collisionBaseID)
	require.NoError(t, err)
	require.True(t, starred)
	// A third file the rebuild sees first must not take the base id either.
	third := env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T09-00-third.json"),
		geminiCollisionSession("shared-session", 3))

	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)

	assertCollisionPair(t, env.db, first, 1, second, 5)
	assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, third), 3)
	ids, err := env.db.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, ids, collisionBaseID)
}

// A collision first seen during a rebuild keeps the stored file on the base
// id, with its curation, even when the rebuild discovers the new file first.
func TestResyncMeetingNewFileKeepsStoredOwner(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored", true: "deleted"}[deleted], func(t *testing.T) {
			env := setupTestEnv(t)
			dir := filepath.Join("tmp", "collisionhash", "chats")
			owner := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-05-owner.json"),
				geminiCollisionSession("shared-session", 5))
			env.engine.SyncAll(t.Context(), nil)
			if deleted {
				require.NoError(t, env.db.DeleteSession(t.Context(), collisionBaseID))
			} else {
				starred, err := env.db.StarSession(t.Context(), collisionBaseID)
				require.NoError(t, err)
				require.True(t, starred)
			}
			newcomer := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-new.json"),
				geminiCollisionSession("shared-session", 1))

			stats := env.engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)

			assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, newcomer), 1)
			base, err := env.db.GetSessionFull(t.Context(), collisionBaseID)
			require.NoError(t, err)
			alt, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(collisionBaseID, owner))
			require.NoError(t, err)
			assert.Nil(t, alt, "the stored file never moves to a derived id")
			if deleted {
				assert.Nil(t, base, "the deleted transcript stays deleted")
				return
			}
			require.NotNil(t, base)
			assert.Equal(t, owner, *base.FilePath)
			ids, err := env.db.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, ids, collisionBaseID)
		})
	}
}

// Each file's row follows its own file: deleting one marks only that row
// source-missing, and the survivor keeps its id.
func TestCollidingFileDeletionMarksOnlyItsRow(t *testing.T) {
	t.Run("derived file deleted", func(t *testing.T) {
		env, first, second := collisionEnv(t, 5, 1)
		require.NoError(t, os.Remove(second))
		require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{second}))

		alt := requireStoredSession(t, env.db, parser.AltSessionID(collisionBaseID, second))
		assert.NotNil(t, alt.SourceMissingAt)
		base := requireStoredSession(t, env.db, collisionBaseID)
		assert.Nil(t, base.SourceMissingAt)
		assert.Equal(t, first, *base.FilePath)
	})
	t.Run("base file deleted", func(t *testing.T) {
		env, first, second := collisionEnv(t, 3, 5)
		require.NoError(t, os.Remove(first))
		require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{first}))
		env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", filepath.Base(second)),
			geminiCollisionSession("shared-session", 7))
		env.engine.SyncAll(t.Context(), nil)

		base := requireStoredSession(t, env.db, collisionBaseID)
		assert.NotNil(t, base.SourceMissingAt)
		assertCollisionPair(t, env.db, first, 3, second, 7)
	})
}

// Deleting or trashing the base never deletes the derived transcript or refills
// the base from it. Trashing the base hides the linked session from the sidebar;
// permanent deletion promotes it to a top-level entry.
func TestUserDeletedBaseKeepsCollidingFileSeparate(t *testing.T) {
	tests := []struct {
		name   string
		remove func(*db.DB) error
	}{
		{name: "deleted", remove: func(d *db.DB) error { return d.DeleteSession(t.Context(), collisionBaseID) }},
		{name: "trashed", remove: func(d *db.DB) error { return d.SoftDeleteSession(t.Context(), collisionBaseID) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _, second := collisionEnv(t, 5, 1)
			require.NoError(t, tt.remove(env.db))
			altID := parser.AltSessionID(collisionBaseID, second)
			for _, sync := range []func(){
				func() { env.engine.SyncAll(t.Context(), nil) },
				func() { env.engine.ResyncAll(t.Context(), nil) },
			} {
				sync()
				alt := requireStoredSession(t, env.db, altID)
				assert.Nil(t, alt.DeletedAt)
				assertSessionMessageCount(t, env.db, altID, 1)
				index, err := env.db.GetSidebarSessionIndex(t.Context(), db.SessionFilter{})
				require.NoError(t, err)
				if tt.name == "trashed" {
					assert.Empty(t, index.Sessions)
				} else {
					require.Len(t, index.Sessions, 1)
					assert.Equal(t, altID, index.Sessions[0].ID)
				}
				base, err := env.db.GetSessionFull(t.Context(), collisionBaseID)
				require.NoError(t, err)
				if base != nil {
					assert.NotNil(t, base.DeletedAt)
					assert.Equal(t, 5, base.MessageCount)
				}
			}
		})
	}
}

// Deleting the derived session excludes only that file; it does not take
// the base id once the base file is gone.
func TestUserDeletedDerivedSessionStaysExcluded(t *testing.T) {
	env, first, second := collisionEnv(t, 5, 1)
	altID := parser.AltSessionID(collisionBaseID, second)
	require.NoError(t, env.db.DeleteSession(t.Context(), altID))
	require.NoError(t, os.Remove(first))
	env.engine.SyncAll(t.Context(), nil)

	alt, err := env.db.GetSessionFull(t.Context(), altID)
	require.NoError(t, err)
	assert.Nil(t, alt)
	base := requireStoredSession(t, env.db, collisionBaseID)
	assert.Equal(t, first, *base.FilePath)
	assertSessionMessageCount(t, env.db, collisionBaseID, 5)
}

// A file that shrinks in place still replaces its own transcript.
func TestSyncAcceptsSameSourceShrinking(t *testing.T) {
	rel := filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-00-same.json")
	env := setupTestEnv(t)
	env.writeGeminiSession(t, rel, geminiCollisionSession("shrinking-session", 5))
	env.engine.SyncAll(t.Context(), nil)
	assertSessionMessageCount(t, env.db, "gemini:shrinking-session", 5)

	env.writeGeminiSession(t, rel, geminiCollisionSession("shrinking-session", 1))
	env.engine.SyncAll(t.Context(), nil)
	assertSessionMessageCount(t, env.db, "gemini:shrinking-session", 1)
}

// A derived session follows its file when the provider moves it: a second
// project's Cursor .txt transcript replaced by a .jsonl beside it keeps one
// row under the same id, and a deleted one stays deleted.
func TestDerivedSessionFollowsProviderMove(t *testing.T) {
	const baseID = "cursor:shared"
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept", true: "deleted"}[deleted], func(t *testing.T) {
			cursorDir := t.TempDir()
			env := setupTestEnv(t, WithCursorDirs([]string{cursorDir}))
			txt := "user:\nHello\nassistant:\nHi\n"
			env.writeCursorSession(t, cursorDir, "Users-alice-code-one", "shared.txt", txt)
			env.engine.SyncAll(t.Context(), nil)
			second := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.txt", txt)
			env.engine.SyncAll(t.Context(), nil)
			altID := parser.AltSessionID(baseID, second)
			requireStoredSession(t, env.db, altID)
			if deleted {
				require.NoError(t, env.db.DeleteSession(t.Context(), altID))
			}

			jsonl := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.jsonl",
				`{"role":"user","message":{"content":"Hello"}}`+"\n"+
					`{"role":"assistant","message":{"content":"Hi"}}`+"\n"+
					`{"role":"user","message":{"content":"More"}}`+"\n")
			env.engine.SyncAll(t.Context(), nil)

			moved, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(baseID, jsonl))
			require.NoError(t, err)
			assert.Nil(t, moved, "no second derived id for the moved file")
			alt, err := env.db.GetSessionFull(t.Context(), altID)
			require.NoError(t, err)
			if deleted {
				assert.Nil(t, alt, "the deleted session stays deleted")
				return
			}
			require.NotNil(t, alt)
			assert.Equal(t, jsonl, *alt.FilePath)
			assert.Equal(t, 3, alt.MessageCount)
		})
	}
}

// A rebuild resolves a derived session's move the same way an ordinary sync
// does, even after its old file was marked missing.
func TestResyncFollowsMoveOfMissingDerivedSession(t *testing.T) {
	const baseID = "cursor:shared"
	cursorDir := t.TempDir()
	env := setupTestEnv(t, WithCursorDirs([]string{cursorDir}))
	txt := "user:\nHello\nassistant:\nHi\n"
	env.writeCursorSession(t, cursorDir, "Users-alice-code-one", "shared.txt", txt)
	env.engine.SyncAll(t.Context(), nil)
	second := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.txt", txt)
	env.engine.SyncAll(t.Context(), nil)
	altID := parser.AltSessionID(baseID, second)
	starred, err := env.db.StarSession(t.Context(), altID)
	require.NoError(t, err)
	require.True(t, starred)
	require.NoError(t, os.Remove(second))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{second}))
	require.NotNil(t, requireStoredSession(t, env.db, altID).SourceMissingAt)

	jsonl := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.jsonl",
		`{"role":"user","message":{"content":"Hello"}}`+"\n"+
			`{"role":"assistant","message":{"content":"Hi"}}`+"\n"+
			`{"role":"user","message":{"content":"More"}}`+"\n")
	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)

	moved, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(baseID, jsonl))
	require.NoError(t, err)
	assert.Nil(t, moved, "no second derived id for the moved file")
	alt := requireStoredSession(t, env.db, altID)
	assert.Equal(t, jsonl, *alt.FilePath)
	assert.Equal(t, 3, alt.MessageCount)
	ids, err := env.db.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, ids, altID)
}

// A file that reuses a permanently deleted session's id shows up under its
// own id once the deleted session's file is gone; the deleted one stays hidden.
func TestNewFileReusingDeletedSessionIDStaysVisible(t *testing.T) {
	env := setupTestEnv(t)
	dir := filepath.Join("tmp", "collisionhash", "chats")
	owner := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-owner.json"),
		geminiCollisionSession("shared-session", 5))
	env.engine.SyncAll(t.Context(), nil)
	require.NoError(t, env.db.DeleteSession(t.Context(), collisionBaseID))
	require.NoError(t, os.Remove(owner))
	newcomer := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-00-new.json"),
		geminiCollisionSession("shared-session", 1))

	for _, sync := range []func(){
		func() { env.engine.SyncAll(t.Context(), nil) },
		func() { env.engine.ResyncAll(t.Context(), nil) },
	} {
		sync()
		assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, newcomer), 1)
		base, err := env.db.GetSessionFull(t.Context(), collisionBaseID)
		require.NoError(t, err)
		assert.Nil(t, base)
	}
}

// A deletion recorded before deletions kept their file covers every file
// with that id, so nothing deleted before upgrading comes back.
func TestDeletionWithoutRecordedFileHidesEveryFile(t *testing.T) {
	env := setupTestEnv(t)
	dir := filepath.Join("tmp", "collisionhash", "chats")
	env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-owner.json"),
		geminiCollisionSession("shared-session", 5))
	env.engine.SyncAll(t.Context(), nil)
	require.NoError(t, env.db.DeleteSession(t.Context(), collisionBaseID))
	require.NoError(t, env.db.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "UPDATE excluded_sessions SET file_path = NULL WHERE id = ?", collisionBaseID)
		return err
	}))
	newcomer := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-00-new.json"),
		geminiCollisionSession("shared-session", 1))

	for _, sync := range []func(){
		func() { env.engine.SyncAll(t.Context(), nil) },
		func() { env.engine.ResyncAll(t.Context(), nil) },
	} {
		sync()
		alt, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(collisionBaseID, newcomer))
		require.NoError(t, err)
		assert.Nil(t, alt)
		base, err := env.db.GetSessionFull(t.Context(), collisionBaseID)
		require.NoError(t, err)
		assert.Nil(t, base)
	}
}

// A stored session with no recorded file still owns its id; a file that
// arrives with the same id gets its own id.
func TestPathlessOwnerKeepsSessionID(t *testing.T) {
	env := setupTestEnv(t)
	require.NoError(t, env.db.UpsertSession(t.Context(), db.Session{
		ID: collisionBaseID, Project: "p", Machine: "local", Agent: string(parser.AgentGemini), MessageCount: 7,
	}))
	other := env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-00-new.json"),
		geminiCollisionSession("shared-session", 1))
	env.engine.SyncAll(t.Context(), nil)

	base := requireStoredSession(t, env.db, collisionBaseID)
	assert.Nil(t, base.FilePath)
	assert.Equal(t, 7, base.MessageCount)
	assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, other), 1)
}

// A Cursor file outside the cwd allow-list never takes part in ownership: an
// allowed file with the same id gets the base id, with no dangling parent, on
// every pass.
func TestCwdFilteredFileDoesNotClaimSessionID(t *testing.T) {
	root := t.TempDir()
	workspaces := cursorWorkspaceTempDir(t)
	keep, drop := filepath.Join(workspaces, "keep"), filepath.Join(workspaces, "drop")
	const sessionID = "12121212-3434-4565-8787-abababababab"
	paths := make(map[string]string)
	for _, workspace := range []string{keep, drop} {
		require.NoError(t, os.MkdirAll(workspace, 0o755))
		path := filepath.Join(root, encodeCursorProjectDir(workspace), "agent-transcripts", sessionID+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(`{"role":"user","message":{"content":"hello"}}`+"\n"), 0o644))
		paths[workspace] = path
	}
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{keep},
	})
	t.Cleanup(engine.Close)

	const baseID = "cursor:" + sessionID
	for range 2 {
		engine.SyncAll(t.Context(), nil)
		base := requireStoredSession(t, database, baseID)
		assert.Equal(t, paths[keep], *base.FilePath)
		assert.Nil(t, base.ParentSessionID)
		for _, path := range paths {
			alt, err := database.GetSessionFull(t.Context(), parser.AltSessionID(baseID, path))
			require.NoError(t, err)
			assert.Nil(t, alt)
		}
	}
}

// A derived Cursor session whose workspace leaves the cwd allow-list keeps its
// derived id, so cwd reconciliation updates its own row.
func TestCwdFilteredDerivedSessionKeepsItsID(t *testing.T) {
	root := t.TempDir()
	workspaces := cursorWorkspaceTempDir(t)
	keep, drop := filepath.Join(workspaces, "keep"), filepath.Join(workspaces, "drop")
	const sessionID = "56565656-7878-4989-8a8a-cdcdcdcdcdcd"
	paths := make(map[string]string)
	database := dbtest.OpenTestDB(t)
	for _, workspace := range []string{keep, drop} {
		require.NoError(t, os.MkdirAll(workspace, 0o755))
		path := filepath.Join(root, encodeCursorProjectDir(workspace), "agent-transcripts", sessionID+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(`{"role":"user","message":{"content":"hello"}}`+"\n"), 0o644))
		paths[workspace] = path
		initial := sync.NewEngine(t.Context(), database, sync.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}}, Machine: "local",
		})
		initial.SyncAll(t.Context(), nil)
		initial.Close()
	}
	const baseID = "cursor:" + sessionID
	altID := parser.AltSessionID(baseID, paths[drop])
	require.Equal(t, drop, requireStoredSession(t, database, altID).Cwd)

	require.NoError(t, os.RemoveAll(drop))
	filtered := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{keep},
	})
	t.Cleanup(filtered.Close)
	filtered.SyncAll(t.Context(), nil)

	assert.Empty(t, requireStoredSession(t, database, altID).Cwd, "the derived row's cwd follows its unresolved workspace")
	assert.Equal(t, paths[keep], *requireStoredSession(t, database, baseID).FilePath)
}
