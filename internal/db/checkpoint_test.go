package db

import (
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
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
	require.NoError(t, d.UpsertParserCheckpoint(t.Context(), cp, blobs))

	got, ok, err := d.GetParserCheckpoint(t.Context(), cp.SessionID)
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

	gotBlobs, ok, err := d.GetParserCheckpointBlobs(t.Context(), cp.SessionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, blobs.Cursor, gotBlobs.Cursor)
	assert.Equal(t, blobs.HashState, gotBlobs.HashState)

	require.NoError(t, d.DeleteParserCheckpoint(t.Context(), cp.SessionID))
	_, ok, err = d.GetParserCheckpoint(t.Context(), cp.SessionID)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = d.GetParserCheckpointBlobs(t.Context(), cp.SessionID)
	require.NoError(t, err)
	assert.False(t, ok, "delete must remove the blob payload too")
}

func TestReplaceSessionContentWithCheckpointUsesPrefixedSessionID(t *testing.T) {
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
	err := d.ReplaceSessionContentWithCheckpoint(t.Context(),
		storedID, msgs, SessionSignalUpdate{}, nil, cp, blobs,
	)
	require.NoError(t, err)

	var nativeCount, prefixedCount int
	require.NoError(t, d.Reader().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM parser_checkpoints WHERE session_id = ?`,
		"codex:native",
	).Scan(&nativeCount))
	require.NoError(t, d.Reader().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM parser_checkpoints WHERE session_id = ?`,
		storedID,
	).Scan(&prefixedCount))
	assert.Zero(t, nativeCount,
		"the checkpoint must not be stored under the parser-native id")
	assert.Equal(t, 1, prefixedCount,
		"the checkpoint must be stored under the rewritten session id")

	var blobCount int
	require.NoError(t, d.Reader().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM parser_checkpoint_blobs WHERE session_id = ?`,
		storedID,
	).Scan(&blobCount))
	assert.Equal(t, 1, blobCount)
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
	_, werr := d.WriteSessionIncremental(t.Context(), "s1", nil, IncrementalSessionUpdate{
		MsgCount:        1,
		NextOrdinal:     1,
		Checkpoint:      &cp,
		CheckpointBlobs: &blobs,
	})
	require.NoError(t, werr)

	got, ok, err := d.GetParserCheckpoint(t.Context(), "s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1024), got.Offset)
	gotBlobs, ok, err := d.GetParserCheckpointBlobs(t.Context(), "s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []byte("c"), gotBlobs.Cursor)

	// A delta without a checkpoint must not disturb the stored one.
	_, werr = d.WriteSessionIncremental(t.Context(), "s1", nil, IncrementalSessionUpdate{
		MsgCount:    1,
		NextOrdinal: 2,
	})
	require.NoError(t, werr)
	got, ok, err = d.GetParserCheckpoint(t.Context(), "s1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1024), got.Offset)
}

func TestParserCheckpointRollsBackWithTransaction(t *testing.T) {
	d := testDB(t)
	tx, err := d.getWriter().Begin(t.Context())
	require.NoError(t, err)
	require.NoError(t, d.upsertParserCheckpointTx(t.Context(), tx, ParserCheckpoint{
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

	_, ok, err := d.GetParserCheckpoint(t.Context(), "s-rollback")
	require.NoError(t, err)
	assert.False(t, ok,
		"an aborted transaction must not leave a checkpoint behind")
	_, ok, err = d.GetParserCheckpointBlobs(t.Context(), "s-rollback")
	require.NoError(t, err)
	assert.False(t, ok,
		"an aborted transaction must not leave checkpoint blobs behind")
}

func sha256StateAfter(t *testing.T, n int) []byte {
	t.Helper()
	h := sha256.New()
	_, _ = h.Write([]byte(strings.Repeat("s", n)))
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	require.NoError(t, err)
	return state
}

func TestRestrictedArchiveKeepsOnlySourceFreeCheckpoints(t *testing.T) {
	writes := map[string]func(t *testing.T, d *DB, cp ParserCheckpoint, blobs ParserCheckpointBlobs){
		"upsert": func(t *testing.T, d *DB, cp ParserCheckpoint, blobs ParserCheckpointBlobs) {
			t.Helper()
			require.NoError(t, d.UpsertParserCheckpoint(t.Context(), cp, blobs))
		},
		"incremental": func(t *testing.T, d *DB, cp ParserCheckpoint, blobs ParserCheckpointBlobs) {
			t.Helper()
			_, err := d.WriteSessionIncremental(t.Context(), cp.SessionID, nil, IncrementalSessionUpdate{
				MsgCount: 1, NextOrdinal: 1, Checkpoint: &cp, CheckpointBlobs: &blobs,
			})
			require.NoError(t, err)
		},
	}
	for _, content := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for name, write := range writes {
			for _, tc := range []struct {
				name  string
				blobs ParserCheckpointBlobs
				want  []byte // stored hash state; nil when the checkpoint is dropped
			}{
				{"aligned state", ParserCheckpointBlobs{HashState: sha256StateAfter(t, 128)}, sha256StateAfter(t, 128)},
				{"unaligned state", ParserCheckpointBlobs{HashState: sha256StateAfter(t, 100)}, sha256StateAfter(t, 64)},
				{"unknown state encoding", ParserCheckpointBlobs{HashState: []byte("state")}, nil},
				{"unknown cursor", ParserCheckpointBlobs{Cursor: []byte{1}, HashState: sha256StateAfter(t, 128)}, nil},
			} {
				t.Run(string(content)+"/"+name+"/"+tc.name, func(t *testing.T) {
					d := testDB(t)
					d.SetArchiveContent(content)
					insertSession(t, d, "s1", "proj")
					// Start from a stored checkpoint so a refused write must delete it.
					require.NoError(t, d.UpsertParserCheckpoint(t.Context(), ParserCheckpoint{
						SessionID: "s1", Agent: "claude", FilePath: "/sessions/s1.jsonl",
						Offset: 64, TailAnchorDigest: "anchor", Hash: "h", NextOrdinal: 1,
					}, ParserCheckpointBlobs{Cursor: []byte{}, HashState: sha256StateAfter(t, 64)}))
					blobs := tc.blobs
					blobs.SessionID = "s1"
					if blobs.Cursor == nil {
						blobs.Cursor = []byte{}
					}
					write(t, d, ParserCheckpoint{
						SessionID: "s1", Agent: "claude", FilePath: "/sessions/s1.jsonl",
						Offset: 128, TailAnchorDigest: "anchor", Hash: "h",
						NextOrdinal: 1, Version: ParserCheckpointVersion,
					}, blobs)
					_, ok, err := d.GetParserCheckpoint(t.Context(), "s1")
					require.NoError(t, err)
					assert.Equal(t, tc.want != nil, ok)
					stored, ok, err := d.GetParserCheckpointBlobs(t.Context(), "s1")
					require.NoError(t, err)
					require.Equal(t, tc.want != nil, ok)
					if ok {
						assert.Equal(t, tc.want, stored.HashState)
					}
				})
			}
		}
	}
}

// codexCursorBytes encodes a Codex cursor (codexCursorState.MarshalBinary,
// version 6) with only a model and no pending tool calls.
func codexCursorBytes(t *testing.T) []byte {
	t.Helper()
	var b []byte
	b = append(b, 6)
	for _, str := range []string{"gpt-5.4", "", "", "", ""} {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(str)))
		b = append(b, str...)
	}
	b = append(b, make([]byte, sha256.Size)...) // first user digest
	b = append(b, 0)                            // flags
	b = append(b, make([]byte, sha256.Size)...) // last token usage digest
	b = binary.LittleEndian.AppendUint32(b, 0)  // last task event
	b = append(b, 0)                            // pending call count
	require.True(t, parser.IsCodexCursor(b), "update to the current cursor encoding")
	return b
}

func TestParserCheckpointBlobsKeptUnderCodexCursor(t *testing.T) {
	blobs := ParserCheckpointBlobs{Cursor: codexCursorBytes(t), HashState: sha256StateAfter(t, 128)}
	assert.True(t, blobs.keptUnder(config.ArchiveContentTranscripts))
	assert.False(t, blobs.keptUnder(config.ArchiveContentUsage))
	blobs.HashState = sha256StateAfter(t, 100)
	assert.False(t, blobs.keptUnder(config.ArchiveContentTranscripts))
}

func TestParserCheckpointBlobsKeptUnderRestrictedArchives(t *testing.T) {
	stateAfter := func(n int) []byte { return sha256StateAfter(t, n) }
	for _, tc := range []struct {
		name  string
		blobs ParserCheckpointBlobs
		want  bool
	}{
		{"state at block boundary", ParserCheckpointBlobs{HashState: stateAfter(128)}, true},
		{"empty input state", ParserCheckpointBlobs{HashState: stateAfter(0)}, true},
		{"state inside a block", ParserCheckpointBlobs{HashState: stateAfter(100)}, false},
		{"unknown cursor", ParserCheckpointBlobs{Cursor: []byte{1}, HashState: stateAfter(128)}, false},
		{"missing state", ParserCheckpointBlobs{}, false},
		{"unknown encoding", ParserCheckpointBlobs{HashState: make([]byte, 108)}, false},
		{"aligned count with buffered bytes", ParserCheckpointBlobs{HashState: func() []byte {
			state := stateAfter(128)
			state[40] = 'x'
			return state
		}()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.blobs.keptUnder(config.ArchiveContentTranscripts))
			assert.Equal(t, tc.want, tc.blobs.keptUnder(config.ArchiveContentUsage))
			assert.True(t, tc.blobs.keptUnder(config.ArchiveContentFull))
		})
	}
}
