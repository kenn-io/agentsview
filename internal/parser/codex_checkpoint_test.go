package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
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
	require := require.New(t)

	var state codexCursorState

	// Wrong version.
	blob, err := state.MarshalBinary()
	require.NoError(err)
	blob[0] = 99
	require.Error(state.UnmarshalBinary(blob))

	// Truncated payload.
	blob, err = state.MarshalBinary()
	require.NoError(err)
	require.Error(state.UnmarshalBinary(blob[:len(blob)-3]))

	// Oversized pending-call count.
	blob, err = state.MarshalBinary()
	require.NoError(err)
	blob[len(blob)-1] = 200
	require.Error(state.UnmarshalBinary(blob))
}

func TestCodexProviderIncrementalResumesFromCheckpointSeed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	checkpoint := outcome.Results[0].Result.Checkpoint
	require.NotEmpty(checkpoint,
		"a full parse of a safe-offset transcript must produce a checkpoint")

	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		callID, "done", "2026-08-02T09:00:03Z",
	))
	appendCodexProviderContent(t, path, tail)

	fingerprint, err = provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	incOutcome, status, err := provider.ParseIncremental(
		t.Context(), IncrementalRequest{
			Source:       source,
			Fingerprint:  fingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 2,
			Seed:         checkpoint,
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	assert.Empty(incOutcome.Messages)
	require.Len(incOutcome.ToolCallUpdates, 1)
	assert.Equal(callID, incOutcome.ToolCallUpdates[0].ToolUseID)
	assert.NotEmpty(incOutcome.NextCursor,
		"an applied incremental parse must advance the cursor")
}

// TestCodexParseCarriesSinglePassHashState verifies the full parse captures
// the resumable SHA-256 state and tail-anchor digest on its own read pass:
// the state digest must equal the snapshot hash and the anchor digest must
// equal the hash of the trailing window, so checkpoint persistence never
// needs a second source read.
func TestCodexParseCarriesSinglePassHashState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0].Result
	require.NotEmpty(result.CheckpointHashState,
		"the parse must capture the resumable hash state")

	// The state digest must equal the snapshot's full hash.
	stateHash := sha256.New()
	require.NoError(stateHash.(interface{ UnmarshalBinary([]byte) error }).
		UnmarshalBinary(result.CheckpointHashState))
	assert.Equal(fingerprint.Hash, result.Session.File.Hash)
	wantDigest := sha256.Sum256([]byte(prefix))
	assert.Equal(fingerprint.Hash,
		hex.EncodeToString(wantDigest[:]),
		"sanity: the provider fingerprint is the snapshot hash")
	stateSum := stateHash.Sum(nil)
	assert.Equal(wantDigest[:], stateSum,
		"the captured state must hash exactly the parsed snapshot")

	// The anchor digest must equal the trailing window's hash.
	window := prefix[max(0, len(prefix)-codexCheckpointAnchorSize):]
	wantAnchor := sha256.Sum256([]byte(window))
	assert.Equal(hex.EncodeToString(wantAnchor[:]),
		result.CheckpointAnchorDigest)

	// Resuming the state over an appended tail must reproduce the real
	// full-file hash — the same property the engine's resume path relies
	// on.
	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_tee", "done", "2026-08-02T09:00:03Z",
	))
	appendCodexProviderContent(t, path, tail)
	resumed := sha256.New()
	require.NoError(resumed.(interface{ UnmarshalBinary([]byte) error }).
		UnmarshalBinary(result.CheckpointHashState))
	_, err = resumed.Write([]byte(tail))
	require.NoError(err)
	full := append([]byte(prefix), []byte(tail)...)
	wantFull := sha256.Sum256(full)
	assert.Equal(wantFull[:], resumed.Sum(nil),
		"resuming the captured state must reproduce the full-file hash")
}

func TestCodexProviderIncrementalTargetsLatestDuplicateCallIDOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, tsLateS5,
		),
	)
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(
		AgentCodex, ProviderConfig{Roots: []string{root}},
	)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	checkpoint := outcome.Results[0].Result.Checkpoint
	require.NotEmpty(checkpoint)

	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		callID, "second result", "2026-08-02T09:00:06Z",
	))
	appendCodexProviderContent(t, path, tail)
	fingerprint, err = provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	incOutcome, status, err := provider.ParseIncremental(
		t.Context(), IncrementalRequest{
			Source:       source,
			Fingerprint:  fingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 3,
			Seed:         checkpoint,
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(incOutcome.ToolCallUpdates, 1)
	update := incOutcome.ToolCallUpdates[0]
	assert.Equal(callID, update.ToolUseID)
	assert.True(update.TargetKnown)
	assert.Equal(2, update.MessageOrdinal)
	assert.Equal(0, update.CallIndex)
	require.Len(update.ResultEvents, 1)
	assert.Equal("second result", update.ResultEvents[0].Content)
}

func TestCodexCheckpointRecoversAfterPendingCallBurst(t *testing.T) {
	for _, outputs := range []int{0, 1, 8, 9} {
		t.Run(strconv.Itoa(outputs), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122a09"
			lines := []string{
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project-a", "codex_cli_rs", tsEarly),
				testjsonl.CodexMsgJSON("user", "run the commands", tsEarlyS1),
			}
			for i := range 9 {
				lines = append(lines, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", strconv.Itoa(i), nil, tsEarlyS5))
			}
			for i := range outputs {
				lines = append(lines, testjsonl.CodexFunctionCallOutputJSON(strconv.Itoa(i), "done", tsLate))
			}
			content := testjsonl.JoinJSONL(lines...)
			root := t.TempDir()
			path := writeCodexProviderSessionContent(t, root, uuid, content)
			provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
			require.True(ok)
			source := requireCodexProviderSource(t, provider, uuid)
			ctx := WithoutFilesystemProjectDiscovery(t.Context())
			outcome, err := provider.Parse(ctx, ParseRequest{Source: source})
			require.NoError(err)
			require.Len(outcome.Results, 1)
			result := outcome.Results[0].Result
			sum := sha256.Sum256([]byte(content))
			assert.Equal(hex.EncodeToString(sum[:]), result.Session.File.Hash)
			require.Len(result.Messages, 10)
			for _, msg := range result.Messages[1 : outputs+1] {
				require.Len(msg.ToolCalls, 1)
				require.Len(msg.ToolCalls[0].ResultEvents, 1)
				assert.Equal("done", msg.ToolCalls[0].ResultEvents[0].Content)
			}
			if outputs == 0 {
				assert.Empty(result.Checkpoint)
				return
			}
			require.NotEmpty(result.Checkpoint)
			if outputs == 8 {
				appendCodexProviderContent(t, path, testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("8", "last result", tsLateS5)))
				fingerprint, err := provider.Fingerprint(ctx, source)
				require.NoError(err)
				tail, status, err := provider.ParseIncremental(ctx, IncrementalRequest{
					Source: source, Fingerprint: fingerprint, SessionID: "codex:" + uuid,
					Offset: int64(len(content)), StartOrdinal: 10, Seed: result.Checkpoint,
				})
				require.NoError(err)
				require.Equal(IncrementalApplied, status)
				require.Len(tail.ToolCallUpdates, 1)
				assert.Equal(9, tail.ToolCallUpdates[0].MessageOrdinal)
				require.Len(tail.ToolCallUpdates[0].ResultEvents, 1)
				assert.Equal("last result", tail.ToolCallUpdates[0].ResultEvents[0].Content)
			}
		})
	}
}

func TestCodexSeedlessAppendWithUnresolvedCallOverflow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122a10"
	lines := []string{
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project-a", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "run the commands", tsEarlyS1),
	}
	for i := range 10 {
		lines = append(lines, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", strconv.Itoa(i), nil, tsEarlyS5))
	}
	lines = append(lines, `{"type":"event_msg","payload":{"type":"turn_aborted"}}`)
	prefix := testjsonl.JoinJSONL(lines...)
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	ctx := WithoutFilesystemProjectDiscovery(t.Context())
	appendCodexProviderContent(t, path, testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("user", "continue", tsLate),
		testjsonl.CodexMsgJSON("assistant", "reading the result", tsLateS5),
		testjsonl.CodexFunctionCallOutputJSON("9", "late result", "2024-01-01T10:01:06Z"),
	))
	fingerprint, err := provider.Fingerprint(ctx, source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(ctx, IncrementalRequest{
		Source: source, Fingerprint: fingerprint, SessionID: "codex:" + uuid,
		Offset: int64(len(prefix)), StartOrdinal: 11,
	})
	require.NoError(err)
	require.Equal(IncrementalApplied, status)
	assert.Empty(outcome.NextCursor)
	require.Len(outcome.Messages, 2)
	assert.Equal("continue", outcome.Messages[0].Content)
	assert.Equal("reading the result", outcome.Messages[1].Content)
	require.Len(outcome.ToolCallUpdates, 1)
	assert.True(outcome.ToolCallUpdates[0].TargetKnown)
	assert.Equal(10, outcome.ToolCallUpdates[0].MessageOrdinal)
	require.Len(outcome.ToolCallUpdates[0].ResultEvents, 1)
	assert.Equal("late result", outcome.ToolCallUpdates[0].ResultEvents[0].Content)
	require.NotNil(outcome.TerminationStatus)
	assert.Equal(TerminationToolCallPending, *outcome.TerminationStatus)
}
