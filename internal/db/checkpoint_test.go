package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserCheckpointRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(d.UpsertParserCheckpoint(cp, blobs))

	got, ok, err := d.GetParserCheckpoint(cp.SessionID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(cp.SessionID, got.SessionID)
	assert.Equal(cp.Agent, got.Agent)
	assert.Equal(cp.FilePath, got.FilePath)
	assert.Equal(cp.FileInode, got.FileInode)
	assert.Equal(cp.FileDevice, got.FileDevice)
	assert.Equal(cp.FileMTime, got.FileMTime)
	assert.Equal(cp.Offset, got.Offset)
	assert.Equal(cp.TailAnchorDigest, got.TailAnchorDigest)
	assert.Equal(cp.Hash, got.Hash)
	assert.Equal(cp.NextOrdinal, got.NextOrdinal)
	assert.Equal(cp.Version, got.Version)
	assert.NotEmpty(got.UpdatedAt)

	gotBlobs, ok, err := d.GetParserCheckpointBlobs(cp.SessionID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(blobs.Cursor, gotBlobs.Cursor)
	assert.Equal(blobs.HashState, gotBlobs.HashState)

	require.NoError(d.DeleteParserCheckpoint(cp.SessionID))
	_, ok, err = d.GetParserCheckpoint(cp.SessionID)
	require.NoError(err)
	assert.False(ok)
	_, ok, err = d.GetParserCheckpointBlobs(cp.SessionID)
	require.NoError(err)
	assert.False(ok, "delete must remove the blob payload too")
}

func TestReplaceSessionContentWithCheckpointUsesPrefixedSessionID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	const storedID = "host:codex:native"
	insertSession(t, d, storedID, "proj")
	msgs := []Message{{
		SessionID:  storedID,
		Ordinal:    0,
		Role:       "assistant",
		Content:    "running",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			SessionID: storedID,
			ToolName:  "exec_command",
			Category:  "Bash",
			ToolUseID: "call_1",
		}},
	}}
	cp := &ParserCheckpoint{
		SessionID:        "codex:native",
		Agent:            "codex",
		FilePath:         "/sessions/rollout.jsonl",
		FileInode:        1,
		FileDevice:       1,
		FileMTime:        1,
		FileChangeTime:   1,
		Offset:           8,
		TailAnchorDigest: "anchor",
		Hash:             "hash",
		NextOrdinal:      0,
		Version:          ParserCheckpointVersion,
	}
	blobs := &ParserCheckpointBlobs{
		SessionID: "codex:native",
		Cursor:    []byte("cursor"),
		HashState: []byte("state"),
	}
	err := d.ReplaceSessionContentWithCheckpoint(
		storedID, msgs, SessionSignalUpdate{}, nil, cp, blobs,
	)
	require.NoError(err)

	var nativeCount, prefixedCount int
	require.NoError(d.Reader().QueryRow(
		`SELECT COUNT(*) FROM parser_checkpoints WHERE session_id = ?`,
		"codex:native",
	).Scan(&nativeCount))
	require.NoError(d.Reader().QueryRow(
		`SELECT COUNT(*) FROM parser_checkpoints WHERE session_id = ?`,
		storedID,
	).Scan(&prefixedCount))
	assert.Zero(nativeCount,
		"the checkpoint must not be stored under the parser-native id")
	assert.Equal(1, prefixedCount,
		"the checkpoint must be stored under the rewritten session id")

	var blobCount int
	require.NoError(d.Reader().QueryRow(
		`SELECT COUNT(*) FROM parser_checkpoint_blobs WHERE session_id = ?`,
		storedID,
	).Scan(&blobCount))
	assert.Equal(1, blobCount)
}

func TestWriteSessionIncrementalPersistsCheckpointInSameTx(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	_, werr := d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount:        1,
		NextOrdinal:     1,
		Checkpoint:      &cp,
		CheckpointBlobs: &blobs,
	})
	require.NoError(werr)

	got, ok, err := d.GetParserCheckpoint("s1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(1024), got.Offset)
	gotBlobs, ok, err := d.GetParserCheckpointBlobs("s1")
	require.NoError(err)
	require.True(ok)
	assert.Equal([]byte("c"), gotBlobs.Cursor)

	// A delta without a checkpoint must not disturb the stored one.
	_, werr = d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount:    1,
		NextOrdinal: 2,
	})
	require.NoError(werr)
	got, ok, err = d.GetParserCheckpoint("s1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(1024), got.Offset)
}

func TestParserCheckpointRollsBackWithTransaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	tx, err := d.getWriter().Begin()
	require.NoError(err)
	require.NoError(upsertParserCheckpointTx(tx, ParserCheckpoint{
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
	require.NoError(tx.Rollback())

	_, ok, err := d.GetParserCheckpoint("s-rollback")
	require.NoError(err)
	assert.False(ok,
		"an aborted transaction must not leave a checkpoint behind")
	_, ok, err = d.GetParserCheckpointBlobs("s-rollback")
	require.NoError(err)
	assert.False(ok,
		"an aborted transaction must not leave checkpoint blobs behind")
}
