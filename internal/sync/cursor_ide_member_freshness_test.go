package sync_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func cursorIDEFreshnessComposer(i int) cursorIDESyncComposer {
	return cursorIDESyncComposer{
		id:        fmt.Sprintf("composer-%04d", i),
		name:      fmt.Sprintf("Chat %d", i),
		createdAt: 1782026756842,
		updatedAt: 1782026791522,
		bubbles: []cursorIDESyncBubble{{
			id: "b1", bubbleType: 1, text: fmt.Sprintf("hello %d", i),
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}
}

func seedCursorIDEFreshness(
	t *testing.T, n int,
) (string, string, []cursorIDESyncComposer) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.vscdb")
	composers := make([]cursorIDESyncComposer, 0, n)
	for i := range n {
		composers = append(composers, cursorIDEFreshnessComposer(i))
	}
	createCursorIDEStateDB(t, dbPath, composers)
	return root, dbPath, composers
}

func cursorIDEExec(t *testing.T, dbPath string, stmts func(tx *sql.Tx) error) {
	t.Helper()
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer writer.Close()
	tx, err := writer.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, stmts(tx))
	require.NoError(t, tx.Commit())
}

func createCursorIDEItemTable(t *testing.T, dbPath string) {
	t.Helper()
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`)
		return err
	})
}

func writeCursorIDEItem(t *testing.T, dbPath, key string) {
	t.Helper()
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, key, []byte("v"))
		return err
	})
}

func putCursorIDEComposer(t *testing.T, tx *sql.Tx, c cursorIDESyncComposer) error {
	t.Helper()
	if _, err := tx.ExecContext(t.Context(),
		`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
		"composerData:"+c.id, cursorIDEComposerJSON(t, c),
	); err != nil {
		return err
	}
	for _, b := range c.bubbles {
		raw, err := json.Marshal(map[string]any{
			"type": b.bubbleType, "text": b.text, "createdAt": b.createdAt,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
			"bubbleId:"+c.id+":"+b.id, raw,
		); err != nil {
			return err
		}
	}
	return nil
}

func appendCursorIDEReply(
	c cursorIDESyncComposer, updatedAt int64,
) cursorIDESyncComposer {
	c.bubbles = append(append([]cursorIDESyncBubble(nil), c.bubbles...),
		cursorIDESyncBubble{
			id: "b2", bubbleType: 2, text: "reply",
			createdAt: "2026-06-21T08:00:00.000Z",
		})
	c.updatedAt = updatedAt
	return c
}

type cursorIDECounters struct{ parses, digests int64 }

func readCursorIDECounters() cursorIDECounters {
	return cursorIDECounters{
		parses:  parser.CursorIDEComposerParses(),
		digests: parser.CursorIDEComposerDigests(),
	}
}

func (c cursorIDECounters) since() cursorIDECounters {
	now := readCursorIDECounters()
	return cursorIDECounters{parses: now.parses - c.parses, digests: now.digests - c.digests}
}

func cursorIDESessionID(c cursorIDESyncComposer) string {
	return "cursor-ide:" + c.id
}

func TestCursorIDEWatcherEventParsesOnlyChangedComposer(t *testing.T) {
	digestDeltas := map[int]int64{}
	for _, n := range []int{20, 200} {
		t.Run("composers_"+strconv.Itoa(n), func(t *testing.T) {
			root, dbPath, composers := seedCursorIDEFreshness(t, n)
			engine, database := newCursorIDESyncEngine(t, root)
			require.Equal(t, n, engine.SyncAll(t.Context(), nil).Synced)

			edited := appendCursorIDEReply(composers[0], 1782029000000)
			cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
				return putCursorIDEComposer(t, tx, edited)
			})

			before := readCursorIDECounters()
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
			delta := before.since()

			assert.Equal(t, int64(1), delta.parses, "parse count")
			assert.Equal(t, 1, engine.LastSyncStats().Synced)
			messages, err := database.GetAllMessages(t.Context(), cursorIDESessionID(edited))
			require.NoError(t, err)
			assert.Len(t, messages, 2)
			digestDeltas[n] = delta.digests
		})
	}
	assert.Equal(t, digestDeltas[20], digestDeltas[200], "digest work must not scale")
}

func TestCursorIDEWatcherEventWithNoComposerChangeParsesNothing(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
			"composerData:empty-draft", []byte(`{"fullConversationHeadersOnly":[]}`))
		return err
	})
	createCursorIDEItemTable(t, dbPath)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)
	beforeRows := map[string]db.Session{}
	for _, c := range composers {
		sess, err := database.GetSessionFull(t.Context(), cursorIDESessionID(c))
		require.NoError(t, err)
		require.NotNil(t, sess)
		beforeRows[c.id] = *sess
	}

	writeCursorIDEItem(t, dbPath, "workbench.state")
	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	delta := before.since()

	assert.Zero(t, delta.parses)
	assert.Zero(t, delta.digests)
	for _, c := range composers {
		sess, err := database.GetSessionFull(t.Context(), cursorIDESessionID(c))
		require.NoError(t, err)
		require.NotNil(t, sess)
		want := beforeRows[c.id]
		assert.Equal(t, want.MessageCount, sess.MessageCount, c.id)
		assert.Equal(t, want.FileHash, sess.FileHash, c.id)
		assert.Equal(t, want.TranscriptRevision, sess.TranscriptRevision, c.id)
		assert.Nil(t, sess.SourceMissingAt, c.id)
	}
}

func TestCursorIDEWatcherEventCatchesNewMessageWithStaleTimestamp(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)

	edited := appendCursorIDEReply(composers[3], composers[3].updatedAt)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		return putCursorIDEComposer(t, tx, edited)
	})
	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))

	assert.Equal(t, int64(1), before.since().parses)
	sess, err := database.GetSession(t.Context(), cursorIDESessionID(edited))
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount)
}

func TestCursorIDEBubbleOnlyEditWaitsForScheduledReconcile(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)
	target := composers[5]
	id := cursorIDESessionID(target)

	raw, err := json.Marshal(map[string]any{
		"type": 1, "text": "rewritten in place",
		"createdAt": "2026-06-21T07:27:29.606Z",
	})
	require.NoError(t, err)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
			raw, "bubbleId:"+target.id+":b1")
		return err
	})

	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Zero(t, before.since().parses)
	sess, err := database.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, target.bubbles[0].text, *sess.FirstMessage,
		"the watcher pass defers bubble-only edits")

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentCursorIDE, []string{root},
	))
	sess, err = database.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, "rewritten in place", *sess.FirstMessage)
}

func TestCursorIDEWatcherEventArchivesNewComposer(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)

	added := cursorIDEFreshnessComposer(len(composers))
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		return putCursorIDEComposer(t, tx, added)
	})
	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))

	assert.Equal(t, int64(1), before.since().parses)
	sess, err := database.GetSession(t.Context(), cursorIDESessionID(added))
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.MessageCount)
}

func TestCursorIDEWatcherEventCommitDuringListingKeepsMemberSources(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 200)
	createCursorIDEItemTable(t, dbPath)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)

	edited := appendCursorIDEReply(composers[0], 1782029000000)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		return putCursorIDEComposer(t, tx, edited)
	})
	// A composer committed after the listing's snapshot reaches the archive on its own event.
	during := appendCursorIDEReply(composers[1], 1782029000000)
	calls := 0
	sync.SetChangedPathListedHook(engine, func(string) {
		calls++
		if calls == 1 {
			writeCursorIDEItem(t, dbPath, "during.listing")
			cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
				return putCursorIDEComposer(t, tx, during)
			})
		}
	})

	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Equal(t, int64(1), before.since().parses)
	require.Positive(t, calls)
	sess, err := database.GetSession(t.Context(), cursorIDESessionID(edited))
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount)

	before = readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Equal(t, int64(1), before.since().parses)
	messages, err := database.GetAllMessages(t.Context(), cursorIDESessionID(during))
	require.NoError(t, err)
	assert.Len(t, messages, 2)
}

func TestCursorIDEWatcherEventTombstonesAndParsesInOneBatch(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "clean"
		if malformed {
			name = "malformed_sibling"
		}
		t.Run(name, func(t *testing.T) {
			root, dbPath, composers := seedCursorIDEFreshness(t, 20)
			engine, database := newCursorIDESyncEngine(t, root)
			require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)
			deleted, advanced, broken := composers[1], composers[2], composers[3]
			beforeDelete, err := database.GetSessionFull(t.Context(), cursorIDESessionID(deleted))
			require.NoError(t, err)
			require.NotNil(t, beforeDelete)

			edited := appendCursorIDEReply(advanced, 1782029000000)
			cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(),
					`DELETE FROM cursorDiskKV WHERE key IN (?, ?)`,
					"composerData:"+deleted.id, "bubbleId:"+deleted.id+":b1",
				); err != nil {
					return err
				}
				if malformed {
					if _, err := tx.ExecContext(t.Context(),
						`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
						"{not json", "composerData:"+broken.id,
					); err != nil {
						return err
					}
				}
				return putCursorIDEComposer(t, tx, edited)
			})

			before := readCursorIDECounters()
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
			assert.Equal(t, int64(1), before.since().parses)

			archived, err := database.GetSessionFull(t.Context(), cursorIDESessionID(deleted))
			require.NoError(t, err)
			assertSourceMissingState(t, archived)
			assert.Equal(t, beforeDelete.MessageCount, archived.MessageCount,
				"source loss must retain the archived transcript")
			sess, err := database.GetSession(t.Context(), cursorIDESessionID(edited))
			require.NoError(t, err)
			require.NotNil(t, sess)
			assert.Equal(t, 2, sess.MessageCount)
		})
	}
}

func TestCursorIDELegacyRowsFallBackToContainerParse(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	createCursorIDEItemTable(t, dbPath)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)
	// Suppressed rows alone must not stand in for stored authority.
	trashed, deleted := composers[9], composers[11]
	require.NoError(t, database.SoftDeleteSession(t.Context(), cursorIDESessionID(trashed)))
	require.NoError(t, database.DeleteSession(t.Context(), cursorIDESessionID(deleted)))

	current := strconv.Itoa(db.CurrentDataVersion())
	previous := strconv.Itoa(db.CurrentDataVersion() - 1)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE sessions SET data_version = ? WHERE agent = ?`,
			db.CurrentDataVersion()-1, string(parser.AgentCursorIDE),
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`UPDATE skipped_files SET file_path = replace(file_path, ?, ?)
			WHERE file_path LIKE ?`,
			"data_version="+current, "data_version="+previous,
			"%data_version="+current+"%",
		)
		return err
	}))

	edited := appendCursorIDEReply(composers[0], 1782029000000)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		return putCursorIDEComposer(t, tx, edited)
	})
	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	delta := before.since()
	assert.Equal(t, int64(len(composers)), delta.parses)
	// The container parse digests each composer once; member sources would digest twice (fingerprint and parse).
	assert.Equal(t, int64(len(composers)), delta.digests, "the pass must take the whole-container source")
	for _, c := range composers {
		if c.id == trashed.id || c.id == deleted.id {
			continue
		}
		id := cursorIDESessionID(c)
		assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), id), c.id)
		sess, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess)
		require.NotNil(t, sess.FileHash, c.id)
		assert.True(t, strings.HasPrefix(*sess.FileHash, "cide1:"), c.id)
	}

	bare := composers[7]
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE sessions SET file_hash = ? WHERE id = ?`,
			"0123456789abcdef", cursorIDESessionID(bare),
		)
		return err
	}))
	writeCursorIDEItem(t, dbPath, "unrelated")
	before = readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Equal(t, int64(1), before.since().parses)
}

func TestCursorIDEWatcherEventSkipsTrashedAndDeletedComposers(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	createCursorIDEItemTable(t, dbPath)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)
	trashed, deleted := composers[2], composers[4]
	require.NoError(t, database.SoftDeleteSession(t.Context(), cursorIDESessionID(trashed)))
	require.NoError(t, database.DeleteSession(t.Context(), cursorIDESessionID(deleted)))
	// A trashed row is never rewritten, so it keeps an older version and a pre-cide1 hash.
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE sessions SET data_version = ?, file_hash = ? WHERE id = ?`,
			db.CurrentDataVersion()-1, "0123456789abcdef", cursorIDESessionID(trashed),
		)
		return err
	}))

	writeCursorIDEItem(t, dbPath, "workbench.state")
	before := readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Zero(t, before.since().parses, "trashed and permanently deleted composers must not be parsed")

	edited := appendCursorIDEReply(trashed, 1782029000000)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		return putCursorIDEComposer(t, tx, edited)
	})
	before = readCursorIDECounters()
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Zero(t, before.since().parses, "a trashed composer must not be parsed")
	sess, err := database.GetSessionFull(t.Context(), cursorIDESessionID(trashed))
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.NotNil(t, sess.DeletedAt, "the composer stays trashed")
	gone, err := database.GetSessionFull(t.Context(), cursorIDESessionID(deleted))
	require.NoError(t, err)
	assert.Nil(t, gone)
}
