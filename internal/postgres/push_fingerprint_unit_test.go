package postgres

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestBatchedDependencyFingerprintMatchesPerSession pins that the chunked
// prefetch path the push loop uses produces the exact fingerprint the
// per-session path produces. A divergence would change every stored session
// fingerprint and re-push the entire archive on the next push.
func TestBatchedDependencyFingerprintMatchesPerSession(t *testing.T) {
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	defer local.Close()
	ctx := t.Context()

	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "dep-a", Project: "alpha", Machine: "m1", Agent: "claude-code",
	}))
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "dep-empty", Project: "alpha", Machine: "m1", Agent: "claude-code",
	}))

	note := "pinned"
	require.NoError(t, local.InsertMessages(ctx, []db.Message{
		{
			SessionID: "dep-a", Ordinal: 0, Role: "user",
			Content: "hello", ContentLength: 5,
			Timestamp: "2026-07-01T10:00:00Z", IsSystem: true,
		},
		{
			SessionID: "dep-a", Ordinal: 1, Role: "assistant",
			Content: "with\x00nul", ContentLength: 8,
			ThinkingText: "thinking", HasThinking: true, HasToolUse: true,
			Model: "claude-test", ContextTokens: 10, OutputTokens: 2,
			HasContextTokens: true, HasOutputTokens: true,
			ToolCalls: []db.ToolCall{
				{
					ToolName: "Bash", Category: "execution",
					ToolUseID: "tu-1", InputJSON: `{"command":"ls"}`,
					ResultContentLength: 2, ResultContent: "ok",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "tu-1", Source: "cli", Status: "ok",
						Content: "ok", ContentLength: 2,
						Timestamp: "2026-07-01T10:00:01Z",
					}},
				},
				{ToolName: "Read", Category: "file"},
			},
		},
	}))
	require.NoError(t, local.ReplaceSessionSecretFindings(ctx, "dep-a",
		[]db.SecretFinding{{
			SessionID: "dep-a", RuleName: "token", Confidence: "high",
			LocationKind: "message", MessageOrdinal: 1,
			MatchStart: 0, MatchEnd: 4, RedactedMatch: "w…",
			RulesVersion: "v1",
		}}, 1, "v1"))
	pinned, err := local.GetMessageByOrdinal(ctx, "dep-a", 1)
	require.NoError(t, err)
	require.NotNil(t, pinned)
	pinID, err := local.PinMessage(ctx, "dep-a", pinned.ID, &note)
	require.NoError(t, err)
	require.NotZero(t, pinID)

	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "dep-a",
		[]db.UsageEvent{
			{
				SessionID: "dep-a", Source: "session", Model: "claude-test",
				InputTokens: 4, OccurredAt: "2026-07-01T12:00:00.000Z",
			},
			{
				SessionID: "dep-a", Source: "session", Model: "claude-test",
				InputTokens: 3, OccurredAt: "2026-07-01T12:00:00.123456789Z",
			},
		}))

	ids := []string{"dep-a", "dep-empty", "dep-missing"}
	usageFPs, err := local.SortedUsageEventFingerprints(
		ctx, ids, pgUsageEventTimestampFingerprintText,
	)
	require.NoError(t, err)

	state, err := readLocalPushDependencyState(ctx, local, ids)
	require.NoError(t, err)

	for _, id := range ids {
		byUsageKnown := map[bool]string{}
		for _, usageKnown := range []bool{true, false} {
			want, err := localSessionDependencyPushFingerprint(
				ctx, local, id, usageFPs[id], usageKnown,
			)
			require.NoError(t, err, id)
			got, err := state.dependencyFingerprint(
				ctx, local, id, usageFPs[id], usageKnown,
			)
			require.NoError(t, err, id)
			assert.Equal(t, want, got,
				"dependency fingerprint %s (usageKnown=%t)", id, usageKnown)
			byUsageKnown[usageKnown] = got
		}
		assert.Equal(t, byUsageKnown[true], byUsageKnown[false],
			"usage fallback must match the prefetched fingerprint for %s", id)
	}

	assert.NotEmpty(t, state.contentHashFP["dep-a"],
		"fixture must exercise the message fingerprint maps")
	assert.NotEmpty(t, usageFPs["dep-a"],
		"fixture must exercise the usage fingerprint")
}
