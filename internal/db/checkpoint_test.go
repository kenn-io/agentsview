package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserCheckpointRoundTrip(t *testing.T) {
	d := testDB(t)
	cp := ParserCheckpoint{
		SessionID:        "codex:019eb791-cf7d-75c1-8439-9ed74c122b02",
		Agent:            "codex",
		FilePath:         "/sessions/rollout-x.jsonl",
		FileInode:        42,
		FileDevice:       7,
		FileMTime:        1234567890,
		Offset:           4096,
		TailAnchorDigest: "anchor-digest",
		Hash:             "deadbeef",
		NextOrdinal:      10,
		Version:          ParserCheckpointVersion,
	}
	blobs := ParserCheckpointBlobs{
		SessionID: cp.SessionID,
		Cursor:    []byte("cursor-bytes"),
		HashState: []byte("hash-state"),
	}
	require.NoError(t, d.UpsertParserCheckpoint(cp, blobs))

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
	assert.Equal(t, cp.TailAnchorDigest, got.TailAnchorDigest)
	assert.Equal(t, cp.Hash, got.Hash)
	assert.Equal(t, cp.NextOrdinal, got.NextOrdinal)
	assert.Equal(t, cp.Version, got.Version)
	assert.NotEmpty(t, got.UpdatedAt)

	gotBlobs, ok, err := d.GetParserCheckpointBlobs(cp.SessionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, blobs.Cursor, gotBlobs.Cursor)
	assert.Equal(t, blobs.HashState, gotBlobs.HashState)

	require.NoError(t, d.DeleteParserCheckpoint(cp.SessionID))
	_, ok, err = d.GetParserCheckpoint(cp.SessionID)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = d.GetParserCheckpointBlobs(cp.SessionID)
	require.NoError(t, err)
	assert.False(t, ok, "delete must remove the blob payload too")
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
		SessionID:        "s1",
		Agent:            "codex",
		FilePath:         "/sessions/rollout-s1.jsonl",
		FileInode:        1,
		FileDevice:       2,
		FileMTime:        100,
		Offset:           1024,
		TailAnchorDigest: "anchor-a",
		Hash:             "abc",
		NextOrdinal:      2,
		Version:          ParserCheckpointVersion,
	}
	blobs := ParserCheckpointBlobs{
		SessionID: "s1",
		Cursor:    []byte("c"),
		HashState: []byte("h"),
	}
	require.NoError(t, d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount:        1,
		NextOrdinal:     1,
		Checkpoint:      &cp,
		CheckpointBlobs: &blobs,
	}))

	got, ok, err := d.GetParserCheckpoint("s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1024), got.Offset)
	gotBlobs, ok, err := d.GetParserCheckpointBlobs("s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []byte("c"), gotBlobs.Cursor)

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
		SessionID:        "s-rollback",
		Agent:            "codex",
		FilePath:         "/sessions/rollout-s-rollback.jsonl",
		Offset:           512,
		TailAnchorDigest: "anchor-a",
		Hash:             "h",
		NextOrdinal:      1,
		Version:          ParserCheckpointVersion,
	}, ParserCheckpointBlobs{
		SessionID: "s-rollback",
		Cursor:    []byte("c"),
		HashState: []byte("h"),
	}))
	require.NoError(t, tx.Rollback())

	_, ok, err := d.GetParserCheckpoint("s-rollback")
	require.NoError(t, err)
	assert.False(t, ok,
		"an aborted transaction must not leave a checkpoint behind")
	_, ok, err = d.GetParserCheckpointBlobs("s-rollback")
	require.NoError(t, err)
	assert.False(t, ok,
		"an aborted transaction must not leave checkpoint blobs behind")
}

// TestParserCheckpointSchemaMigratesFromPreSplitShape simulates an archive
// written by the pre-split schema (raw tail_anchor/cursor/hash_state
// columns): reopening must add the digest column, drop the dead columns,
// and leave a version-1 row readable (the engine rebuilds it
// authoritatively) while new version-2 upserts succeed.
