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
