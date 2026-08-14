package parser

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexCursorStateCheckpointRoundTrip(t *testing.T) {
	seed := codexCursorState{
		model:                    "gpt-5.6-luna",
		cwd:                      "/workspace/project-a",
		agentPath:                "/home/chris/.codex/agents/a",
		firstUserSeen:            true,
		sawUserTurnAfterFirst:    true,
		mayReplayFirstUserPrompt: false,
		lastTokenUsageSeen:       true,
		lastTokenUsageDigest:     [sha256.Size]byte{1, 2, 3},
		forkGate: codexForkGate{
			active:           true,
			createdMs:        123456789,
			subagentParentID: "019f0000-0000-7000-8000-000000000000",
		},
		lastTaskEvent: "task_complete",
	}
	seed.rememberToolCall("call_1", "exec_command")
	seed.rememberToolCall("call_2", "apply_patch")

	blob, err := seed.MarshalBinary()
	require.NoError(t, err)

	var got codexCursorState
	require.NoError(t, got.UnmarshalBinary(blob))
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
