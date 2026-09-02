package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexCursorStateCheckpointRoundTrip(t *testing.T) {
	seed := codexCursorState{
		model:                    "gpt-5.6-luna",
		cwd:                      "/workspace/project-a",
		agentPath:                "codex/agents/a",
		firstUserSeen:            true,
		sawUserTurnAfterFirst:    true,
		mayReplayFirstUserPrompt: false,
		lastTokenUsageSeen:       true,
		lastTokenUsageDigest:     [sha256.Size]byte{1, 2, 3},
		forkGate: codexForkGate{
			active:          true,
			parentSessionID: "019f0000-0000-7000-8000-000000000000",
			parentResolved:  true,
		},
		lastTaskEvent: "task_complete",
	}
	seed.rememberToolCall("call_1", "exec_command", &ParsedToolCallPosition{MessageOrdinal: 1, CallIndex: 0})
	seed.rememberToolCall("call_2", "apply_patch", &ParsedToolCallPosition{MessageOrdinal: 2, CallIndex: 0})

	blob, err := seed.MarshalBinary()
	require.NoError(t, err)

	var got codexCursorState
	require.NoError(t, got.UnmarshalBinary(blob))
	// The fork replay gate is process-only state: it is re-armed from the
	// transcript on every parse and is not part of the persisted cursor.
	got.forkGate = seed.forkGate
	assert.Equal(t, seed, got)
}

func TestCodexCursorStateCheckpointRejectsBadPayloads(t *testing.T) {
	var state codexCursorState

	// Wrong version.
	blob, err := state.MarshalBinary()
	require.NoError(t, err)
	blob[0] = 99
	require.Error(t, state.UnmarshalBinary(blob))

	// Truncated payload.
	blob, err = state.MarshalBinary()
	require.NoError(t, err)
	require.Error(t, state.UnmarshalBinary(blob[:len(blob)-3]))

	// Oversized pending-call count.
	blob, err = state.MarshalBinary()
	require.NoError(t, err)
	blob[len(blob)-1] = 200
	require.Error(t, state.UnmarshalBinary(blob))
}

func TestCodexProviderIncrementalResumesFromCheckpointSeed(t *testing.T) {
	const (
		uuid   = "019eb791-cf7d-75c1-8439-9ed74c122a01"
		callID = "call_checkpoint"
	)
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "run the command", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, tsEarlyS5,
		),
	)
	root := t.TempDir()
	path := writeCodexProviderSessionContent(
		t, root, uuid, prefix,
	)
	provider, ok := NewProvider(
		AgentCodex, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	checkpoint := outcome.Results[0].Result.Checkpoint
	require.NotEmpty(t, checkpoint,
		"a full parse of a safe-offset transcript must produce a checkpoint")

	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		callID, "done", "2026-08-02T09:00:03Z",
	))
	appendCodexProviderContent(t, path, tail)

	fingerprint, err = provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	incOutcome, status, err := provider.ParseIncremental(
		context.Background(), IncrementalRequest{
			Source:       source,
			Fingerprint:  fingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 2,
			Seed:         checkpoint,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	assert.Empty(t, incOutcome.Messages)
	require.Len(t, incOutcome.ToolCallUpdates, 1)
	assert.Equal(t, callID, incOutcome.ToolCallUpdates[0].ToolUseID)
	assert.NotEmpty(t, incOutcome.NextCursor,
		"an applied incremental parse must advance the cursor")
}

// TestCodexParseCarriesSinglePassHashState verifies the full parse captures
// the resumable SHA-256 state and tail-anchor digest on its own read pass:
// the state digest must equal the snapshot hash and the anchor digest must
// equal the hash of the trailing window, so checkpoint persistence never
// needs a second source read.
func TestCodexParseCarriesSinglePassHashState(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122a02"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "run the command", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_tee", nil, tsEarlyS5,
		),
	)
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(
		AgentCodex, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	require.NotEmpty(t, result.CheckpointHashState,
		"the parse must capture the resumable hash state")

	// The state digest must equal the snapshot's full hash.
	stateHash := sha256.New()
	require.NoError(t, stateHash.(interface{ UnmarshalBinary([]byte) error }).
		UnmarshalBinary(result.CheckpointHashState))
	assert.Equal(t, fingerprint.Hash, result.Session.File.Hash)
	wantDigest := sha256.Sum256([]byte(prefix))
	assert.Equal(t, fingerprint.Hash,
		fmt.Sprintf("%x", wantDigest[:]),
		"sanity: the provider fingerprint is the snapshot hash")
	stateSum := stateHash.Sum(nil)
	assert.Equal(t, wantDigest[:], stateSum,
		"the captured state must hash exactly the parsed snapshot")

	// The anchor digest must equal the trailing window's hash.
	window := prefix[max(0, len(prefix)-codexCheckpointAnchorSize):]
	wantAnchor := sha256.Sum256([]byte(window))
	assert.Equal(t, fmt.Sprintf("%x", wantAnchor[:]),
		result.CheckpointAnchorDigest)

	// Resuming the state over an appended tail must reproduce the real
	// full-file hash — the same property the engine's resume path relies
	// on.
	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_tee", "done", "2026-08-02T09:00:03Z",
	))
	appendCodexProviderContent(t, path, tail)
	resumed := sha256.New()
	require.NoError(t, resumed.(interface{ UnmarshalBinary([]byte) error }).
		UnmarshalBinary(result.CheckpointHashState))
	_, err = resumed.Write([]byte(tail))
	require.NoError(t, err)
	full := append([]byte(prefix), []byte(tail)...)
	wantFull := sha256.Sum256(full)
	assert.Equal(t, wantFull[:], resumed.Sum(nil),
		"resuming the captured state must reproduce the full-file hash")
}

func TestCodexCursorPendingDuplicateIDsAreFIFO(t *testing.T) {
	var state codexCursorState
	first := &ParsedToolCallPosition{MessageOrdinal: 1, CallIndex: 0}
	second := &ParsedToolCallPosition{MessageOrdinal: 2, CallIndex: 0}
	require.True(t, state.rememberToolCall("reused", "exec_command", first))
	require.True(t, state.rememberToolCall("reused", "apply_patch", second))

	name, ok := state.toolCallName("reused")
	require.True(t, ok)
	assert.Equal(t, "exec_command", name)
	position, ok := state.toolCallPosition("reused")
	require.True(t, ok)
	assert.Equal(t, first, position)

	blob, err := state.MarshalBinary()
	require.NoError(t, err)
	var restored codexCursorState
	require.NoError(t, restored.UnmarshalBinary(blob))

	restored.forgetToolCall("reused")
	name, ok = restored.toolCallName("reused")
	require.True(t, ok)
	assert.Equal(t, "apply_patch", name)
	position, ok = restored.toolCallPosition("reused")
	require.True(t, ok)
	assert.Equal(t, second, position)
}

func TestCodexProviderIncrementalTargetsPendingDuplicateCallIDOccurrence(t *testing.T) {
	const (
		uuid   = "019eb791-cf7d-75c1-8439-9ed74c122d01"
		callID = "reused-call"
	)
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "run twice", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, tsEarlyS5,
		),
		testjsonl.CodexFunctionCallOutputJSON(
			callID, "first result", tsLate,
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, tsLateS5,
		),
	)
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(
		AgentCodex, ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	checkpoint := outcome.Results[0].Result.Checkpoint
	require.NotEmpty(t, checkpoint)

	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		callID, "second result", "2026-08-02T09:00:06Z",
	))
	appendCodexProviderContent(t, path, tail)
	fingerprint, err = provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	incOutcome, status, err := provider.ParseIncremental(
		context.Background(), IncrementalRequest{
			Source:       source,
			Fingerprint:  fingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 3,
			Seed:         checkpoint,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, incOutcome.ToolCallUpdates, 1)
	update := incOutcome.ToolCallUpdates[0]
	assert.Equal(t, callID, update.ToolUseID)
	assert.True(t, update.TargetKnown)
	assert.Equal(t, 2, update.MessageOrdinal)
	assert.Equal(t, 0, update.CallIndex)
	require.Len(t, update.ResultEvents, 1)
	assert.Equal(t, "second result", update.ResultEvents[0].Content)
}
