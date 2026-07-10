package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fingerprintBatchFixture builds two populated sessions plus one empty
// session and returns their IDs along with a never-inserted ID, covering
// the missing-key defaults the batched twins must share with the
// per-session methods.
func fingerprintBatchFixture(t *testing.T) (*DB, []string) {
	t.Helper()
	d := testDB(t)

	insertSession(t, d, "fp-a", "alpha")
	insertSession(t, d, "fp-b", "alpha")
	insertSession(t, d, "fp-empty", "alpha")

	sysMsg := userMsgAt("fp-a", 0, "system prompt", "2026-07-01T10:00:00Z")
	sysMsg.IsSystem = true
	asst := asstMsgAt("fp-a", 1, "raw\x00bytes\x80here", "2026-07-01T10:00:05Z")
	asst.ThinkingText = "thinking\x00text"
	asst.HasThinking = true
	asst.HasToolUse = true
	asst.Model = "claude-test"
	asst.ContextTokens = 100
	asst.OutputTokens = 20
	asst.HasContextTokens = true
	asst.HasOutputTokens = true
	asst.ClaudeMessageID = "msg-1"
	asst.SourceUUID = "uuid-1"
	asst.ToolCalls = []ToolCall{
		{
			ToolName:            "Bash",
			Category:            "execution",
			ToolUseID:           "tu-1",
			InputJSON:           `{"command":"ls"}`,
			ResultContentLength: 4,
			ResultContent:       "ok\x00!",
			FilePath:            "/tmp/x",
			ResultEvents: []ToolResultEvent{
				{
					ToolUseID: "tu-1", Source: "cli", Status: "ok",
					Content: "done", ContentLength: 4,
					Timestamp: "2026-07-01T10:00:06Z", EventIndex: 0,
				},
				{
					ToolUseID: "tu-1", Source: "cli", Status: "ok",
					Content: "more", ContentLength: 4, EventIndex: 1,
				},
			},
		},
		{
			ToolName:  "Read",
			Category:  "file",
			ToolUseID: "tu-2",
			InputJSON: `{"path":"a.go"}`,
		},
	}
	tail := userMsg("fp-a", 2, "follow-up")
	tail.IsSystem = true
	insertMessages(t, d, sysMsg, asst, tail)

	other := asstMsg("fp-b", 0, "short")
	other.ToolCalls = []ToolCall{{ToolName: "Grep", Category: "search"}}
	insertMessages(t, d, other, userMsg("fp-b", 1, "longer message body"))

	// A NULL timestamp exercises the COALESCE both paths must share.
	_, err := d.getWriter().Exec(
		"UPDATE messages SET timestamp = NULL" +
			" WHERE session_id = 'fp-b' AND ordinal = 1",
	)
	require.NoError(t, err)

	require.NoError(t, d.ReplaceSessionSecretFindings(
		"fp-a",
		[]SecretFinding{
			{
				SessionID: "fp-a", RuleName: "aws-key",
				Confidence: "high", LocationKind: "message",
				MessageOrdinal: 1, MatchStart: 3, MatchEnd: 9,
				RedactedMatch: "AKIA…", RulesVersion: "v1",
			},
			{
				SessionID: "fp-a", RuleName: "token",
				Confidence: "low", LocationKind: "tool_result",
				MessageOrdinal: 1, CallIndex: Ptr(0),
				MatchStart: 1, MatchEnd: 2, MatchIndex: 1,
				RedactedMatch: "t…", RulesVersion: "v1",
			},
		},
		2, "v1",
	))

	var msgID int64
	require.NoError(t, d.getReader().QueryRow(
		"SELECT id FROM messages WHERE session_id = 'fp-a' AND ordinal = 1",
	).Scan(&msgID))
	pinID, err := d.PinMessage("fp-a", msgID, Ptr("note"))
	require.NoError(t, err)
	require.NotZero(t, pinID)

	return d, []string{"fp-a", "fp-b", "fp-empty", "fp-missing"}
}

// TestBatchedFingerprintsMatchPerSession pins the byte-identity contract
// between each batched fingerprint method and its per-session twin: push
// fingerprints hash these values and compare them against fingerprints
// stored by earlier pushes, so any divergence re-pushes every session.
func TestBatchedFingerprintsMatchPerSession(t *testing.T) {
	d, ids := fingerprintBatchFixture(t)
	normalize := func(ts string) string {
		if ts == "" {
			return ""
		}
		return "norm:" + ts
	}

	content, err := d.MessageContentFingerprints(ids)
	require.NoError(t, err)
	tokens, err := d.MessageTokenFingerprints(ids)
	require.NoError(t, err)
	contentHash, err := d.MessageContentHashFingerprints(ids)
	require.NoError(t, err)
	roleTime, err := d.MessageRoleTimeFingerprintsWithTimestampNormalizer(
		ids, normalize,
	)
	require.NoError(t, err)
	flags, err := d.MessageFlagsFingerprints(ids)
	require.NoError(t, err)
	system, err := d.SystemMessageFingerprints(ids)
	require.NoError(t, err)
	callCounts, err := d.ToolCallCounts(ids)
	require.NoError(t, err)
	callSums, err := d.ToolCallContentFingerprints(ids)
	require.NoError(t, err)
	callFPs, err := d.ToolCallFingerprints(ids)
	require.NoError(t, err)
	resultFPs, err := d.ToolResultEventFingerprintsWithTimestampNormalizer(
		ids, normalize,
	)
	require.NoError(t, err)

	for _, id := range ids {
		sum, maxLen, minLen, err := d.MessageContentFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t,
			MessageContentAggregate{Sum: sum, Max: maxLen, Min: minLen},
			content[id], "content aggregate %s", id)

		wantToken, err := d.MessageTokenFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantToken, tokens[id], "token fp %s", id)

		wantHash, err := d.MessageContentHashFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantHash, contentHash[id], "content hash fp %s", id)

		wantRoleTime, err := d.MessageRoleTimeFingerprintWithTimestampNormalizer(
			id, normalize,
		)
		require.NoError(t, err, id)
		assert.Equal(t, wantRoleTime, roleTime[id], "role/time fp %s", id)

		wantFlags, err := d.MessageFlagsFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantFlags, flags[id], "flags fp %s", id)

		wantSystem, err := d.SystemMessageFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantSystem, system[id], "system fp %s", id)

		wantCount, err := d.ToolCallCount(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantCount, callCounts[id], "tool call count %s", id)

		wantSum, err := d.ToolCallContentFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantSum, callSums[id], "tool call sum %s", id)

		wantCallFP, err := d.ToolCallFingerprint(id)
		require.NoError(t, err, id)
		assert.Equal(t, wantCallFP, callFPs[id], "tool call fp %s", id)

		wantResultFP, err := d.ToolResultEventFingerprintWithTimestampNormalizer(
			id, normalize,
		)
		require.NoError(t, err, id)
		assert.Equal(t, wantResultFP, resultFPs[id], "tool result fp %s", id)
	}

	assert.NotEmpty(t, tokens["fp-a"], "fixture must produce a token fp")
	assert.NotEmpty(t, callFPs["fp-a"], "fixture must produce a tool call fp")
	assert.Equal(t, "0,2", system["fp-a"], "system ordinals")
}

// TestBatchedFindingsAndPinsMatchPerSession pins nil-vs-empty slice
// semantics: the push dependency fingerprint JSON-encodes both slices, so
// a session without findings must stay [] and a session without pins must
// stay null, exactly as the per-session methods return them.
func TestBatchedFindingsAndPinsMatchPerSession(t *testing.T) {
	d, ids := fingerprintBatchFixture(t)
	ctx := context.Background()

	findings, err := d.SessionSecretFindingsBySession(ctx, ids)
	require.NoError(t, err)
	pins, err := d.PinnedMessagesBySession(ctx, ids)
	require.NoError(t, err)

	for _, id := range ids {
		wantFindings, err := d.SessionSecretFindings(ctx, id)
		require.NoError(t, err, id)
		got, ok := findings[id]
		require.True(t, ok, "findings entry %s", id)
		assert.Equal(t, wantFindings, got, "findings %s", id)
		assert.NotNil(t, got, "findings must be non-nil for %s", id)

		wantPins, err := d.ListPinnedMessages(ctx, id, "")
		require.NoError(t, err, id)
		assert.Equal(t, wantPins, pins[id], "pins %s", id)
	}

	require.Len(t, findings["fp-a"], 2)
	assert.Equal(t, "token", findings["fp-a"][0].RuleName,
		"findings keep natural position order (match_start ascending)")
	require.Len(t, pins["fp-a"], 1)
	_, hasEmptyPins := pins["fp-empty"]
	assert.False(t, hasEmptyPins, "pin map omits sessions without pins")
}
