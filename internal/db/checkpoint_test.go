package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserCheckpointRoundTrip(t *testing.T) {
	d := testDB(t)
	cp := ParserCheckpoint{
		SessionID:   "codex:019eb791-cf7d-75c1-8439-9ed74c122b02",
		Agent:       "codex",
		FilePath:    "/sessions/rollout-x.jsonl",
		FileInode:   42,
		FileDevice:  7,
		FileMTime:   1234567890,
		Offset:      4096,
		TailAnchor:  []byte("anchor-bytes"),
		Cursor:      []byte("cursor-bytes"),
		HashState:   []byte("hash-state"),
		Hash:        "deadbeef",
		NextOrdinal: 10,
		Version:     ParserCheckpointVersion,
	}
	require.NoError(t, d.UpsertParserCheckpoint(cp))

	got, ok, err := d.GetParserCheckpoint(cp.SessionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, cp.SessionID, got.SessionID)
	assert.Equal(t, cp.Agent, got.Agent)
	assert.Equal(t, cp.FilePath, got.FilePath)
	assert.Equal(t, cp.FileInode, got.FileInode)
	assert.Equal(t, cp.FileDevice, got.FileDevice)
	assert.Equal(t, cp.FileMTime, got.FileMTime)
	assert.Equal(t, cp.Offset, got.Offset)
	assert.Equal(t, cp.TailAnchor, got.TailAnchor)
	assert.Equal(t, cp.Cursor, got.Cursor)
	assert.Equal(t, cp.HashState, got.HashState)
	assert.Equal(t, cp.Hash, got.Hash)
	assert.Equal(t, cp.NextOrdinal, got.NextOrdinal)
	assert.Equal(t, cp.Version, got.Version)
	assert.NotEmpty(t, got.UpdatedAt)

	require.NoError(t, d.DeleteParserCheckpoint(cp.SessionID))
	_, ok, err = d.GetParserCheckpoint(cp.SessionID)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestWriteSessionIncrementalPersistsCheckpointInSameTx(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    0,
		Role:       "assistant",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			SessionID: "s1",
			ToolName:  "exec_command",
			Category:  "Bash",
			ToolUseID: "call_cmd",
		}},
	})
	cp := ParserCheckpoint{
		SessionID:   "s1",
		Agent:       "codex",
		FilePath:    "/sessions/rollout-s1.jsonl",
		FileInode:   1,
		FileDevice:  2,
		FileMTime:   100,
		Offset:      1024,
		TailAnchor:  []byte("a"),
		Cursor:      []byte("c"),
		HashState:   []byte("h"),
		Hash:        "abc",
		NextOrdinal: 2,
		Version:     ParserCheckpointVersion,
	}
	require.NoError(t, d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount:    1,
		NextOrdinal: 1,
		Checkpoint:  &cp,
	}))

	got, ok, err := d.GetParserCheckpoint("s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1024), got.Offset)
	assert.Equal(t, []byte("c"), got.Cursor)

	// A delta without a checkpoint must not disturb the stored one.
	require.NoError(t, d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount:    1,
		NextOrdinal: 2,
	}))
	got, ok, err = d.GetParserCheckpoint("s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1024), got.Offset)
}

func TestParserCheckpointRollsBackWithTransaction(t *testing.T) {
	d := testDB(t)
	tx, err := d.getWriter().Begin()
	require.NoError(t, err)
	require.NoError(t, upsertParserCheckpointTx(tx, ParserCheckpoint{
		SessionID:   "s-rollback",
		Agent:       "codex",
		FilePath:    "/sessions/rollout-s-rollback.jsonl",
		Offset:      512,
		TailAnchor:  []byte("a"),
		Cursor:      []byte("c"),
		Hash:        "h",
		NextOrdinal: 1,
		Version:     ParserCheckpointVersion,
	}))
	require.NoError(t, tx.Rollback())

	_, ok, err := d.GetParserCheckpoint("s-rollback")
	require.NoError(t, err)
	assert.False(t, ok,
		"an aborted transaction must not leave a checkpoint behind")
}

// TestParserCheckpointSchemaMigratesFromPreSplitShape simulates an archive
// written by the pre-split schema (raw tail_anchor/cursor/hash_state
// columns): reopening must add the digest column, drop the dead columns,
// and leave a version-1 row readable (the engine rebuilds it
// authoritatively) while new version-2 upserts succeed.
func TestParserCheckpointSchemaMigratesFromPreSplitShape(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().Exec(`DROP TABLE parser_checkpoints`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		CREATE TABLE parser_checkpoints (
		    session_id         TEXT PRIMARY KEY,
		    agent              TEXT NOT NULL,
		    file_path          TEXT NOT NULL,
		    file_inode         INTEGER NOT NULL,
		    file_device        INTEGER NOT NULL,
		    file_mtime         INTEGER NOT NULL,
		    offset             INTEGER NOT NULL,
		    tail_anchor        BLOB NOT NULL,
		    cursor             BLOB NOT NULL,
		    hash_state         BLOB,
		    hash               TEXT NOT NULL,
		    next_ordinal       INTEGER NOT NULL,
		    checkpoint_version INTEGER NOT NULL,
		    updated_at         TEXT NOT NULL
		)`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		INSERT INTO parser_checkpoints (
		    session_id, agent, file_path, file_inode, file_device,
		    file_mtime, offset, tail_anchor, cursor, hash_state, hash,
		    next_ordinal, checkpoint_version, updated_at
		) VALUES (
		    'legacy-session', 'codex', '/sessions/legacy.jsonl', 1, 2,
		    100, 512, X'616E', X'637572', X'6861', 'h',
		    1, 1, '2026-08-13T00:00:00Z'
		)`)
	require.NoError(t, err)
	path := d.path
	require.NoError(t, d.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	cp, ok, err := reopened.GetParserCheckpoint("legacy-session")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, cp.Version,
		"a pre-split row keeps its version so the engine rebuilds it")
	assert.Empty(t, cp.TailAnchorDigest,
		"the migration leaves the digest empty; the gate treats it as invalid")

	// The new shape must accept a version-2 upsert.
	require.NoError(t, reopened.UpsertParserCheckpoint(
		ParserCheckpoint{
			SessionID:        "new-session",
			Agent:            "codex",
			FilePath:         "/sessions/new.jsonl",
			Offset:           64,
			TailAnchorDigest: "digest",
			Hash:             "hash",
			NextOrdinal:      1,
			Version:          ParserCheckpointVersion,
		},
		ParserCheckpointBlobs{
			SessionID: "new-session",
			Cursor:    []byte("c"),
			HashState: []byte("h"),
		},
	))
	got, ok, err := reopened.GetParserCheckpoint("new-session")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ParserCheckpointVersion, got.Version)
}
