package sync_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// cursorIDESyncBubble is one synthetic cursorDiskKV bubble row.
type cursorIDESyncBubble struct {
	id         string
	bubbleType int
	text       string
	createdAt  string
}

// cursorIDESyncComposer is one synthetic composerData document plus its
// bubbles.
type cursorIDESyncComposer struct {
	id        string
	name      string
	createdAt int64
	updatedAt int64
	bubbles   []cursorIDESyncBubble
}

func cursorIDEComposerJSON(t *testing.T, c cursorIDESyncComposer) []byte {
	t.Helper()
	headers := make([]map[string]any, 0, len(c.bubbles))
	for _, b := range c.bubbles {
		headers = append(headers, map[string]any{
			"bubbleId": b.id, "type": b.bubbleType,
		})
	}
	raw, err := json.Marshal(map[string]any{
		"fullConversationHeadersOnly": headers,
		"name":                        c.name,
		"createdAt":                   c.createdAt,
		"lastUpdatedAt":               c.updatedAt,
		"workspaceIdentifier": map[string]any{
			"uri": map[string]any{"fsPath": "/work/project"},
		},
	})
	require.NoError(t, err)
	return raw
}

func createCursorIDEStateDB(
	t *testing.T, dbPath string, composers []cursorIDESyncComposer,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
	)
	require.NoError(t, err)
	for _, c := range composers {
		_, err = db.Exec(
			`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
			"composerData:"+c.id, cursorIDEComposerJSON(t, c),
		)
		require.NoError(t, err)
		for _, b := range c.bubbles {
			raw, err := json.Marshal(map[string]any{
				"type": b.bubbleType, "text": b.text, "createdAt": b.createdAt,
			})
			require.NoError(t, err)
			_, err = db.Exec(
				`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
				"bubbleId:"+c.id+":"+b.id, raw,
			)
			require.NoError(t, err)
		}
	}
}

func newCursorIDESyncEngine(
	t *testing.T, root string,
) (*sync.Engine, *db.DB) {
	t.Helper()
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCursorIDE: {root},
		},
		Machine: "local",
	})
	return engine, database
}

func TestSyncPathsCursorIDEDeletedComposerTombstonesSession(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{
		{
			id: "deleted-composer", name: "Deleted chat",
			createdAt: 1782026756842, updatedAt: 1782026791522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "gone",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
		{
			id: "surviving-composer", name: "Surviving chat",
			createdAt: 1782026756842, updatedAt: 1782026801522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "kept",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
	})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	beforeDelete, err := database.GetSessionFull(
		t.Context(), "cursor-ide:deleted-composer",
	)
	require.NoError(t, err)
	require.NotNil(t, beforeDelete)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, key := range []string{
		"composerData:deleted-composer", "bubbleId:deleted-composer:b1",
	} {
		_, err = writer.Exec(`DELETE FROM cursorDiskKV WHERE key = ?`, key)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	engine.SyncPaths([]string{dbPath})

	archived, err := database.GetSessionFull(
		t.Context(), "cursor-ide:deleted-composer",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, beforeDelete.MessageCount, archived.MessageCount,
		"source loss must retain the archived transcript")
	surviving, err := database.GetSessionFull(
		t.Context(), "cursor-ide:surviving-composer",
	)
	require.NoError(t, err)
	require.NotNil(t, surviving)
	assert.Nil(t, surviving.SourceMissingAt)
}

func TestSyncPathsCursorIDEDeletedPhysicalDBPreservesSessions(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "archived-composer", name: "Archived chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.Remove(dbPath))

	engine.SyncPaths([]string{dbPath})

	sess, err := database.GetSession(t.Context(), "cursor-ide:archived-composer")
	require.NoError(t, err)
	assert.NotNil(t, sess,
		"removing state.vscdb must not delete already-synced sessions")
}

func TestReconcileWatchRootsCursorIDEDeletedMemberTombstonesSession(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{
		{
			id: "deleted-composer", name: "Deleted chat",
			createdAt: 1782026756842, updatedAt: 1782026791522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "gone",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
		{
			id: "surviving-composer", name: "Surviving chat",
			createdAt: 1782026756842, updatedAt: 1782026801522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "kept",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
	})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, key := range []string{
		"composerData:deleted-composer", "bubbleId:deleted-composer:b1",
	} {
		_, err = writer.Exec(`DELETE FROM cursorDiskKV WHERE key = ?`, key)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))

	archived, err := database.GetSessionFull(
		t.Context(), "cursor-ide:deleted-composer",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	surviving, err := database.GetSessionFull(
		t.Context(), "cursor-ide:surviving-composer",
	)
	require.NoError(t, err)
	require.NotNil(t, surviving)
	assert.Nil(t, surviving.SourceMissingAt)
}

func TestSyncAllCursorIDEAddedBubbleWithUnchangedTimestampReparses(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	composer := cursorIDESyncComposer{
		id: "edited-composer", name: "Edited chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{composer})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	sess, err := database.GetSession(t.Context(), "cursor-ide:edited-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 1, sess.MessageCount)

	// A new turn whose write leaves lastUpdatedAt untouched: the composer
	// document changes only in its header list.
	composer.bubbles = append(composer.bubbles, cursorIDESyncBubble{
		id: "b2", bubbleType: 2, text: "world",
		createdAt: "2026-06-21T07:27:31.522Z",
	})
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		cursorIDEComposerJSON(t, composer), "composerData:edited-composer",
	)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"type": 2, "text": "world", "createdAt": "2026-06-21T07:27:31.522Z",
	})
	require.NoError(t, err)
	_, err = writer.Exec(
		`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
		"bubbleId:edited-composer:b2", raw,
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err = database.GetSession(t.Context(), "cursor-ide:edited-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount,
		"an added turn must not be dropped as unchanged when lastUpdatedAt is stale")
}

func TestSyncAllCursorIDEBubbleContentEditWithUnchangedComposerDocReparses(
	t *testing.T,
) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "edited-composer", name: "Edited chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	sess, err := database.GetSession(t.Context(), "cursor-ide:edited-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FirstMessage)
	require.Equal(t, "hello", *sess.FirstMessage)

	// Rewrite the bubble in place without touching composerData at all:
	// lastUpdatedAt and the header list stay identical, so only the stored
	// bubble bytes reveal the edit.
	raw, err := json.Marshal(map[string]any{
		"type": 1, "text": "hello, but rewritten in place",
		"createdAt": "2026-06-21T07:27:29.606Z",
	})
	require.NoError(t, err)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		raw, "bubbleId:edited-composer:b1",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err = database.GetSession(t.Context(), "cursor-ide:edited-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, "hello, but rewritten in place", *sess.FirstMessage,
		"an in-place bubble rewrite must not be dropped as unchanged")
}

func TestSyncAllCursorIDEEmptiedContainerRetiresAllMembers(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "only-composer", name: "Only chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`DELETE FROM cursorDiskKV`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	archived, err := database.GetSessionFull(t.Context(), "cursor-ide:only-composer")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
}

func TestSyncAllCursorIDERenamedComposerWithUnchangedTimestampReparses(
	t *testing.T,
) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	composer := cursorIDESyncComposer{
		id: "renamed-composer", name: "Old name",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{composer})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	// Rename the chat without touching lastUpdatedAt or any bubble: only the
	// composer document's name field changes.
	composer.name = "New name"
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		cursorIDEComposerJSON(t, composer), "composerData:renamed-composer",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err := database.GetSession(t.Context(), "cursor-ide:renamed-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.DisplayName)
	assert.Equal(t, "New name", *sess.DisplayName,
		"a rename that leaves lastUpdatedAt untouched must not be dropped as unchanged")
}

func TestSyncAllCursorIDESameSizeSameMtimeRewriteReparses(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "rewritten-composer", name: "Rewritten chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	// A second unchanged pass lets the engine record the container's
	// skip-cache entry, so the rewrite below must actually defeat the cached
	// skip rather than just the unchanged-result filter.
	engine.SyncAll(t.Context(), nil)
	before, err := os.Stat(dbPath)
	require.NoError(t, err)

	// Rewrite the bubble to an equal-length value and restore the database
	// file's mtime, so size, mtime, and the composer document are all
	// byte-identical to the synced state. Only the SQLite change counter and
	// the bubble's content differ.
	raw, err := json.Marshal(map[string]any{
		"type": 1, "text": "howdy", "createdAt": "2026-06-21T07:27:29.606Z",
	})
	require.NoError(t, err)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		raw, "bubbleId:rewritten-composer:b1",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	after, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size(),
		"fixture must reproduce a same-size rewrite")
	require.NoError(t, os.Chtimes(dbPath, before.ModTime(), before.ModTime()))

	engine.SyncAll(t.Context(), nil)

	sess, err := database.GetSession(
		t.Context(), "cursor-ide:rewritten-composer",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, "howdy", *sess.FirstMessage,
		"a same-size, same-mtime rewrite must miss the skip cache and reparse")
}

func TestSyncAllCursorIDEWipedBubblesMarkSourceMissingAndPreserveTranscript(
	t *testing.T,
) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "wiped-composer", name: "Wiped chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), "cursor-ide:wiped-composer")
	require.NoError(t, err)
	require.NotNil(t, before)

	// A Cursor update wiping bubble rows while the composer document and its
	// headers survive: the transcript's source material is locally gone, but
	// the archived session must stay browsable and intact.
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM cursorDiskKV WHERE key = ?`, "bubbleId:wiped-composer:b1",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	archived, err := database.GetSessionFull(t.Context(), "cursor-ide:wiped-composer")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, before.MessageCount, archived.MessageCount,
		"wiped bubble rows must not truncate the archived transcript")
}

func TestSyncAllCursorIDEPartialBubbleWipeKeepsArchivedTranscript(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "partial-composer", name: "Partially wiped chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{
			{id: "b1", bubbleType: 1, text: "ask", createdAt: "2026-06-21T07:27:29.606Z"},
			{id: "b2", bubbleType: 2, text: "answer", createdAt: "2026-06-21T07:27:31.522Z"},
		},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	sess, err := database.GetSession(t.Context(), "cursor-ide:partial-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, sess.MessageCount)

	// A partial wipe: one bubble row vanishes while the composer document
	// still references it. The truncated remainder must not replace the
	// archived fuller transcript.
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM cursorDiskKV WHERE key = ?`, "bubbleId:partial-composer:b2",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err = database.GetSession(t.Context(), "cursor-ide:partial-composer")
	require.NoError(t, err)
	require.NotNil(t, sess, "the preserved session must stay active")
	assert.Equal(t, 2, sess.MessageCount,
		"a partial bubble wipe must not shrink the archived transcript")
}

func TestSyncAllCursorIDEGappedComposerSurfacesAndKeepsGrowing(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	composer := cursorIDESyncComposer{
		id: "gappy-composer", name: "Gappy chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{
			{id: "missing", bubbleType: 1, text: "never written"},
			{id: "b1", bubbleType: 1, text: "hello", createdAt: "2026-06-21T07:27:29.606Z"},
		},
	}
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{composer})
	// Remove the first bubble's row so the composer starts out gapped, as a
	// database wiped before agentsview ever saw it would be.
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM cursorDiskKV WHERE key = ?`, "bubbleId:gappy-composer:missing",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced,
		"a gapped composer never seen before must still surface its remaining content")
	sess, err := database.GetSession(t.Context(), "cursor-ide:gappy-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 1, sess.MessageCount)
	assert.True(t, sess.IsTruncated)

	// The user keeps chatting in the gapped conversation: growth must not be
	// frozen by the shrink guard.
	composer.bubbles = append(composer.bubbles, cursorIDESyncBubble{
		id: "b2", bubbleType: 2, text: "reply", createdAt: "2026-06-21T07:27:31.522Z",
	})
	writer, err = sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		cursorIDEComposerJSON(t, composer), "composerData:gappy-composer",
	)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"type": 2, "text": "reply", "createdAt": "2026-06-21T07:27:31.522Z",
	})
	require.NoError(t, err)
	_, err = writer.Exec(
		`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
		"bubbleId:gappy-composer:b2", raw,
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err = database.GetSession(t.Context(), "cursor-ide:gappy-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount,
		"new turns in a gapped conversation must keep syncing")
}

func TestSyncAllCursorIDEEarlierBubbleWipeWithGrowthKeepsArchive(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	composer := cursorIDESyncComposer{
		id: "masked-composer", name: "Masked wipe chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{
			{id: "b1", bubbleType: 1, text: "first ask", createdAt: "2026-06-21T07:27:29.606Z"},
			{id: "b2", bubbleType: 2, text: "first answer", createdAt: "2026-06-21T07:27:31.522Z"},
		},
	}
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{composer})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	sess, err := database.GetSession(t.Context(), "cursor-ide:masked-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, sess.MessageCount)

	// Wipe the first bubble while the conversation keeps growing: the new
	// transcript has as many messages as the archive, but no longer contains
	// the archived first turn. A message-count guard alone would admit it.
	composer.bubbles = append(composer.bubbles,
		cursorIDESyncBubble{id: "b3", bubbleType: 1, text: "second ask", createdAt: "2026-06-21T07:28:01.000Z"},
		cursorIDESyncBubble{id: "b4", bubbleType: 2, text: "second answer", createdAt: "2026-06-21T07:28:05.000Z"},
	)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		cursorIDEComposerJSON(t, composer), "composerData:masked-composer",
	)
	require.NoError(t, err)
	for _, b := range composer.bubbles[2:] {
		raw, err := json.Marshal(map[string]any{
			"type": b.bubbleType, "text": b.text, "createdAt": b.createdAt,
		})
		require.NoError(t, err)
		_, err = writer.Exec(
			`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
			"bubbleId:masked-composer:"+b.id, raw,
		)
		require.NoError(t, err)
	}
	_, err = writer.Exec(
		`DELETE FROM cursorDiskKV WHERE key = ?`, "bubbleId:masked-composer:b1",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err = database.GetSession(t.Context(), "cursor-ide:masked-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount,
		"a wiped earlier bubble masked by new turns must not replace the archive")
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, "first ask", *sess.FirstMessage,
		"the archived first turn must survive the masked wipe")
}

func TestSourceMtimeCursorIDEResolvesVirtualMemberPath(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "watched-composer", name: "Watched chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, _ := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	// The session watcher polls SourceMtime as an equality-only change
	// token; a "state.vscdb#<composer>" virtual path cannot be stat'ed, so
	// it must resolve through the member fingerprint instead of returning
	// zero and disabling change detection.
	token := engine.SourceMtime("cursor-ide:watched-composer")
	require.NotZero(t, token,
		"the watcher token must resolve through the member fingerprint")

	// An in-place bubble rewrite leaves lastUpdatedAt untouched; the token
	// must still move so the polling fallback sees the edit.
	raw, err := json.Marshal(map[string]any{
		"type": 1, "text": "hello, edited in place",
		"createdAt": "2026-06-21T07:27:29.606Z",
	})
	require.NoError(t, err)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		raw, "bubbleId:watched-composer:b1",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	edited := engine.SourceMtime("cursor-ide:watched-composer")
	require.NotZero(t, edited)
	assert.NotEqual(t, token, edited,
		"an edit that leaves lastUpdatedAt untouched must still move the token")
}

func TestResyncAllCursorIDEKeepsArchivedTranscriptOverGapResult(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{
		{
			id: "resync-composer", name: "Resynced chat",
			createdAt: 1782026756842, updatedAt: 1782026791522,
			bubbles: []cursorIDESyncBubble{
				{id: "b1", bubbleType: 1, text: "ask", createdAt: "2026-06-21T07:27:29.606Z"},
				{id: "b2", bubbleType: 2, text: "answer", createdAt: "2026-06-21T07:27:31.522Z"},
			},
		},
		{
			id: "healthy-composer", name: "Healthy chat",
			createdAt: 1782026756842, updatedAt: 1782026801522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "fine",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
	})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	sess, err := database.GetSession(t.Context(), "cursor-ide:resync-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, sess.MessageCount)

	// Wipe one bubble, then rebuild the archive. During the rebuild e.db is
	// the fresh database, so the truncation guard must verify against the
	// original archive (archiveStore); admitting the gap transcript there
	// would put the session into the rebuild and the orphan copy would
	// never rescue the fuller original.
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM cursorDiskKV WHERE key = ?`, "bubbleId:resync-composer:b2",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "resync aborted: %+v", stats)

	sess, err = database.GetSession(t.Context(), "cursor-ide:resync-composer")
	require.NoError(t, err)
	require.NotNil(t, sess,
		"the archived session must survive the rebuild")
	assert.Equal(t, 2, sess.MessageCount,
		"a full resync must not replace the archive with a gap transcript")
}

func TestSyncAllCursorIDEEmptiedBubbleKeepsArchivedTranscript(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "emptied-composer", name: "Emptied chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{
			{id: "b1", bubbleType: 1, text: "ask", createdAt: "2026-06-21T07:27:29.606Z"},
			{id: "b2", bubbleType: 2, text: "answer", createdAt: "2026-06-21T07:27:31.522Z"},
		},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	sess, err := database.GetSession(t.Context(), "cursor-ide:emptied-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, sess.MessageCount)

	// The bubble row survives but its content is wiped in place: the header
	// still references it, so the transcript is incomplete, not edited.
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{}`), "bubbleId:emptied-composer:b2",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(t.Context(), nil)

	sess, err = database.GetSession(t.Context(), "cursor-ide:emptied-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount,
		"a bubble emptied in place must not erase the archived turn")
}

func TestSyncAllCursorIDEReplacedDatabaseFileReparses(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	composer := func(text string) cursorIDESyncComposer {
		return cursorIDESyncComposer{
			id: "replaced-composer", name: "Replaced chat",
			createdAt: 1782026756842, updatedAt: 1782026791522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: text,
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		}
	}
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{composer("hello")})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	// A second unchanged pass records the container's skip-cache entry.
	engine.SyncAll(t.Context(), nil)
	before, err := os.Stat(dbPath)
	require.NoError(t, err)

	// Rename a different database over state.vscdb, built by an identical
	// statement sequence so its size and 100-byte header match, and restore
	// the mtime -- the shape of a backup restore or profile switch. Only the
	// file identity distinguishes it.
	otherPath := filepath.Join(t.TempDir(), "state.vscdb")
	createCursorIDEStateDB(t, otherPath, []cursorIDESyncComposer{composer("howdy")})
	require.NoError(t, os.Rename(otherPath, dbPath))
	after, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size(),
		"fixture must reproduce a same-size replacement")
	require.NoError(t, os.Chtimes(dbPath, before.ModTime(), before.ModTime()))

	engine.SyncAll(t.Context(), nil)

	sess, err := database.GetSession(t.Context(), "cursor-ide:replaced-composer")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, "howdy", *sess.FirstMessage,
		"a replaced database file must miss the skip cache and reparse")
}

// TestSyncAllCursorIDENullValueRowsDoNotFailThePass reproduces
// kenn-io/agentsview#1676: a cursorDiskKV row with a NULL value must not fail
// the whole cursor-ide pass. It loads the reporter's captured artifact
// verbatim, applied on top of two healthy sibling composers built by the
// existing test helper.
func TestSyncAllCursorIDENullValueRowsDoNotFailThePass(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{
		{
			id: "sibling-a-composer", name: "Sibling A",
			createdAt: 1782026756842, updatedAt: 1782026791522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "sibling a content",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
		{
			id: "sibling-b-composer", name: "Sibling B",
			createdAt: 1782026756842, updatedAt: 1782026791522,
			bubbles: []cursorIDESyncBubble{{
				id: "b1", bubbleType: 1, text: "sibling b content",
				createdAt: "2026-06-21T07:27:29.606Z",
			}},
		},
	})

	artifact, err := os.ReadFile(
		filepath.Join("..", "parser", "testdata", "cursor-ide-null-values.sql"),
	)
	require.NoError(t, err)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(string(artifact))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine, database := newCursorIDESyncEngine(t, root)
	stats := engine.SyncAll(t.Context(), nil)
	require.True(t, stats.ProcessingComplete(),
		"the reporter's NULL cursorDiskKV rows must not fail the sync pass: %+v", stats)
	assert.Zero(t, stats.Failed)
	assert.Equal(t, 2, stats.Synced,
		"both healthy sibling composers must be synced past the husk rows")

	a, err := database.GetSessionFull(t.Context(), "cursor-ide:sibling-a-composer")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, 1, a.MessageCount)
	assert.Zero(t, a.ParserMalformedLines)
	b, err := database.GetSessionFull(t.Context(), "cursor-ide:sibling-b-composer")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Equal(t, 1, b.MessageCount)
	assert.Zero(t, b.ParserMalformedLines)

	_, hasCursorIDE := stats.Anomalies.MalformedLinesByAgent["cursor-ide"]
	assert.False(t, hasCursorIDE,
		"the husk rows must be published through source-missing and truncation, not a counter")
}

// TestSyncAllCursorIDENullComposerKeepsArchivedTranscript covers P5: a live
// composer whose value later goes NULL must preserve the archived transcript
// through the recoverable source-missing seam, not fail the pass or drop the
// session.
func TestSyncAllCursorIDENullComposerKeepsArchivedTranscript(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	createCursorIDEStateDB(t, dbPath, []cursorIDESyncComposer{{
		id: "goes-null-composer", name: "Goes null chat",
		createdAt: 1782026756842, updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: "hello",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), "cursor-ide:goes-null-composer")
	require.NoError(t, err)
	require.NotNil(t, before)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = NULL WHERE key = ?`,
		"composerData:goes-null-composer",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	stats := engine.SyncAll(t.Context(), nil)
	require.True(t, stats.ProcessingComplete(),
		"a composer value going NULL must not fail the pass: %+v", stats)

	archived, err := database.GetSessionFull(t.Context(), "cursor-ide:goes-null-composer")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, before.MessageCount, archived.MessageCount,
		"a NULL composer value must not truncate the archived transcript")
	assert.False(t, database.IsSessionExcluded("cursor-ide:goes-null-composer"),
		"the archived session must not be permanently deleted")
}
