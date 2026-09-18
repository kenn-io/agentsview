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
	assert := assert.New(t)
	require := require.New(t)

	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(err)
	defer local.Close()
	ctx := t.Context()

	require.NoError(local.UpsertSession(db.Session{
		ID: "dep-a", Project: "alpha", Machine: "m1", Agent: "claude-code",
	}))
	require.NoError(local.UpsertSession(db.Session{
		ID: "dep-empty", Project: "alpha", Machine: "m1", Agent: "claude-code",
	}))

	note := "pinned"
	require.NoError(local.InsertMessages([]db.Message{
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
	require.NoError(local.ReplaceSessionSecretFindings("dep-a",
		[]db.SecretFinding{{
			SessionID: "dep-a", RuleName: "token", Confidence: "high",
			LocationKind: "message", MessageOrdinal: 1,
			MatchStart: 0, MatchEnd: 4, RedactedMatch: "w…",
			RulesVersion: "v1",
		}}, 1, "v1"))
	pinned, err := local.GetMessageByOrdinal("dep-a", 1)
	require.NoError(err)
	require.NotNil(pinned)
	pinID, err := local.PinMessage("dep-a", pinned.ID, &note)
	require.NoError(err)
	require.NotZero(pinID)

	ids := []string{"dep-a", "dep-empty", "dep-missing"}
	usageFPs, err := local.UsageEventFingerprints(ids)
	require.NoError(err)

	state, err := readLocalPushDependencyState(ctx, local, ids)
	require.NoError(err)

	for _, id := range ids {
		for _, usageKnown := range []bool{true, false} {
			want, err := localSessionDependencyPushFingerprint(
				ctx, local, id, usageFPs[id], usageKnown,
			)
			require.NoError(err, id)
			got, err := state.dependencyFingerprint(
				local, id, usageFPs[id], usageKnown,
			)
			require.NoError(err, id)
			assert.Equal(want, got,
				"dependency fingerprint %s (usageKnown=%t)", id, usageKnown)
		}
	}

	assert.NotEmpty(state.contentHashFP["dep-a"],
		"fixture must exercise the message fingerprint maps")
}
