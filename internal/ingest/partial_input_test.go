package ingest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

// One result carrier holds a result for a call that is not in the
// transcript (a truncated or resumed file). That orphan must not stop the
// remaining results from pairing, and a blocked category keeps only its
// length.
func TestPrepareCandidatePairsAroundOrphanAndBlockedResults(t *testing.T) {
	parsed := parser.ParseResult{
		Session: parser.ParsedSession{ID: "session-1", Agent: parser.AgentClaude},
		Messages: []parser.ParsedMessage{
			{
				Ordinal: 0, Role: parser.RoleAssistant, Content: "running two tools",
				ToolCalls: []parser.ParsedToolCall{
					{ToolUseID: "call-1", ToolName: "Bash", Category: "Bash"},
					{ToolUseID: "call-2", ToolName: "Read", Category: "Read"},
				},
			},
			{
				Ordinal: 1, Role: parser.RoleUser,
				ToolResults: []parser.ParsedToolResult{
					{ToolUseID: "call-1", ContentRaw: `"ok output"`},
					{ToolUseID: "call-missing", ContentRaw: `"from a dropped turn"`},
					{ToolUseID: "call-2", ContentLength: 11, ContentRaw: `"secret file"`},
				},
			},
			{
				Ordinal: 2, Role: parser.RoleUser, Content: "thanks, now commit",
				ToolResults: []parser.ParsedToolResult{
					{ToolUseID: "call-missing-2", ContentRaw: `"late"`},
				},
			},
		},
	}

	candidate, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{
		BlockedResultCategories: map[string]bool{"Read": true},
	})
	require.NoError(t, err)

	require.Len(t, candidate.Messages, 2, "only the empty carrier is dropped")
	assert.Equal(t, 0, candidate.Messages[0].Ordinal)
	assert.Equal(t, 2, candidate.Messages[1].Ordinal)
	assert.Equal(t, "thanks, now commit", candidate.Messages[1].Content,
		"a user turn with text survives even when it also carries a result")

	calls := candidate.Messages[0].ToolCalls
	require.Len(t, calls, 2)
	assert.Equal(t, "ok output", calls[0].ResultContent)
	assert.Equal(t, 9, calls[0].ResultContentLength)
	assert.Empty(t, calls[1].ResultContent, "blocked category content is not stored")
	assert.Equal(t, 11, calls[1].ResultContentLength)
	assert.Equal(t, 2, candidate.Session.MessageCount)
	assert.Equal(t, 1, candidate.Session.UserMessageCount)
}

func TestPrepareCandidateStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ingest.PrepareCandidate(ctx, parser.ParseResult{
		Session:  parser.ParsedSession{ID: "session-1", Agent: parser.AgentClaude},
		Messages: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "hi"}},
	}, ingest.ContentOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestSummarizeToolResultEvents(t *testing.T) {
	no, yes := false, true
	tests := []struct {
		name   string
		events []db.ToolResultEvent
		want   string
	}{
		{name: "no events", want: ""},
		{
			name: "latest anonymous update wins",
			events: []db.ToolResultEvent{
				{Content: "running"}, {Content: "done"},
			},
			want: "done",
		},
		{
			name: "blank updates do not erase content",
			events: []db.ToolResultEvent{
				{Content: "done"}, {Content: "  \n"},
			},
			want: "done",
		},
		{
			name: "one agent plus anonymous tail",
			events: []db.ToolResultEvent{
				{AgentID: "worker", Content: "first"},
				{AgentID: " worker ", Content: "second"},
				{Content: "wrapped up"},
			},
			want: "second\n\nwrapped up",
		},
		{
			name: "agents keep first-seen order with latest content",
			events: []db.ToolResultEvent{
				{AgentID: "b", Content: "b1"},
				{AgentID: "a", Content: "a1"},
				{AgentID: "b", Content: "b2"},
				{Content: "anon"},
			},
			want: "b:\nb2\n\na:\na1\n\nanon",
		},
		{
			name: "event that did not participate is ignored",
			events: []db.ToolResultEvent{
				{Content: "kept", SummaryParticipates: &yes},
				{Content: "excluded", SummaryParticipates: &no},
			},
			want: "kept",
		},
		{
			// Participation is captured from raw bytes; an update whose
			// content was later sanitized to blank still replaces the
			// earlier one.
			name: "participating update blanked by sanitizing",
			events: []db.ToolResultEvent{
				{Content: "old"},
				{Content: "", SummaryParticipates: &yes},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ingest.SummarizeToolResultEventsContext(t.Context(), tt.events)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func oversizedTranscript(sessionID string) []db.Message {
	messages := make([]db.Message, 2001)
	for i := range messages {
		messages[i] = db.Message{SessionID: sessionID, Ordinal: i, Role: "user", Content: "x"}
	}
	return messages
}

func TestComputeSessionFrictionOverBudgetIsSkippedWithoutReview(t *testing.T) {
	reviewed := false
	update, err := ingest.ComputeSessionFriction(
		db.Session{ID: "big", Agent: "claude"}, oversizedTranscript("big"), nil,
		ingest.FrictionOptions{Review: func(friction.SessionInput) []friction.Signal {
			reviewed = true
			return nil
		}},
	)
	require.NoError(t, err)
	assert.False(t, reviewed, "an over-budget session must not be reviewed")
	assert.Equal(t, "friction-v2:skipped", update.RulesVersion)
	assert.Empty(t, update.Findings)
	assert.NotEmpty(t, update.Hash)
}

func TestComputeSessionFrictionRecoversDetectorPanic(t *testing.T) {
	update, err := ingest.ComputeSessionFriction(
		db.Session{ID: "claude:s1", Agent: "claude"},
		[]db.Message{{SessionID: "claude:s1", Ordinal: 0, Role: "user", Content: "hi"}},
		nil,
		ingest.FrictionOptions{Review: func(friction.SessionInput) []friction.Signal {
			panic("detector exploded")
		}},
	)
	require.EqualError(t, err, "friction compute claude:s1: panic: detector exploded")
	assert.Empty(t, update.Findings)
}

func TestRefreshFriction(t *testing.T) {
	t.Run("nil prepared session", func(t *testing.T) {
		require.EqualError(t, ingest.RefreshFriction(nil, ingest.ContentOptions{}),
			"refresh friction: nil prepared session")
	})
	t.Run("usage-only archive settles and clears stale counts", func(t *testing.T) {
		prepared := &ingest.PreparedSession{
			Session: db.Session{
				ID: "claude:s1", Agent: "claude",
				FrictionCount: 4, FrictionRulesVersion: "friction-v1", FrictionHash: "stale",
			},
			Messages: []db.Message{{SessionID: "claude:s1", Ordinal: 0, Role: "user", Content: "hi"}},
		}
		require.NoError(t, ingest.RefreshFriction(prepared, ingest.ContentOptions{
			ArchiveContent: config.ArchiveContentUsage,
		}))
		assert.Zero(t, prepared.Session.FrictionCount)
		assert.Equal(t, "friction-v2", prepared.Session.FrictionRulesVersion)
		assert.NotEqual(t, "stale", prepared.Session.FrictionHash)
		assert.Equal(t, prepared.Friction.Hash, prepared.Session.FrictionHash)
	})
	t.Run("over-budget session is stamped skipped", func(t *testing.T) {
		prepared := &ingest.PreparedSession{
			Session:  db.Session{ID: "big", Agent: "claude", FrictionCount: 2},
			Messages: oversizedTranscript("big"),
		}
		require.NoError(t, ingest.RefreshFriction(prepared, ingest.ContentOptions{}))
		assert.Zero(t, prepared.Session.FrictionCount)
		assert.Equal(t, "friction-v2:skipped", prepared.Session.FrictionRulesVersion)
	})
}

func TestCodexCloudKeepReason(t *testing.T) {
	tests := []struct {
		name     string
		messages int
		saved    int
		current  bool
		want     string
	}{
		{
			name: "shorter mirror, current parser", messages: 3, saved: 5, current: true,
			want: "the mirror file has fewer messages than the saved copy (5 saved, 3 found)",
		},
		{name: "shorter mirror, older parser reparses", messages: 3, saved: 5},
		{
			name: "empty mirror, older parser", messages: 0, saved: 5,
			want: "the mirror file has fewer messages than the saved copy (5 saved, 0 found)",
		},
		{name: "same length", messages: 5, saved: 5, current: true},
		{name: "longer mirror", messages: 6, saved: 5, current: true},
		{name: "nothing saved", messages: 0, saved: 0, current: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ingest.CodexCloudKeepReason(tt.messages, ingest.CodexCloudPrior{
				Session: db.Session{MessageCount: tt.saved}, Current: tt.current,
			})
			assert.Equal(t, tt.want, got)
		})
	}
}

// A mirror cut short keeps the keys of its earlier usage events; a rebuilt
// mirror starts over under new keys. Only the cut file takes the saved
// token counts back.
func TestCarryCodexCloudUsageCutVersusRebuiltMirror(t *testing.T) {
	prior := ingest.CodexCloudPrior{
		Current: true,
		Session: db.Session{
			HasTotalOutputTokens: true, TotalOutputTokens: 500,
			HasPeakContextTokens: true, PeakContextTokens: 9000,
		},
		UsageEvents: []db.UsageEvent{{DedupKey: "k1"}, {DedupKey: "k2"}, {DedupKey: "k3"}},
	}
	t.Run("cut file takes saved tokens and events", func(t *testing.T) {
		prepared := &ingest.PreparedSession{
			Session: db.Session{
				HasTotalOutputTokens: true, TotalOutputTokens: 500,
				HasPeakContextTokens: true, PeakContextTokens: 4000,
			},
			UsageEvents: []db.UsageEvent{{DedupKey: "k1"}, {DedupKey: "k2"}},
		}
		assert.True(t, ingest.CarryCodexCloudUsage(prepared, prior))
		assert.Equal(t, []db.UsageEvent{{DedupKey: "k1"}, {DedupKey: "k2"}, {DedupKey: "k3"}}, prepared.UsageEvents)
		assert.Equal(t, 500, prepared.Session.TotalOutputTokens)
		assert.Equal(t, 9000, prepared.Session.PeakContextTokens)
		assert.Nil(t, prepared.PlanUsage)
	})
	t.Run("rebuilt file keeps its own tokens and the larger peak", func(t *testing.T) {
		prepared := &ingest.PreparedSession{
			Session: db.Session{
				HasTotalOutputTokens: true, TotalOutputTokens: 600,
				HasPeakContextTokens: true, PeakContextTokens: 4000,
			},
			UsageEvents: []db.UsageEvent{{DedupKey: "new-1"}},
		}
		assert.True(t, ingest.CarryCodexCloudUsage(prepared, prior))
		assert.Equal(t, []db.UsageEvent{{DedupKey: "new-1"}}, prepared.UsageEvents)
		assert.Equal(t, 600, prepared.Session.TotalOutputTokens)
		assert.Equal(t, 9000, prepared.Session.PeakContextTokens)
	})
	t.Run("older parser copy is not treated as cut", func(t *testing.T) {
		older := prior
		older.Current = false
		prepared := &ingest.PreparedSession{
			Session: db.Session{
				HasTotalOutputTokens: true, TotalOutputTokens: 100,
				HasPeakContextTokens: true, PeakContextTokens: 4000,
			},
			UsageEvents: []db.UsageEvent{{DedupKey: "k1"}},
		}
		assert.False(t, ingest.CarryCodexCloudUsage(prepared, older))
		assert.Equal(t, []db.UsageEvent{{DedupKey: "k1"}}, prepared.UsageEvents)
		assert.Equal(t, 100, prepared.Session.TotalOutputTokens)
		assert.Equal(t, 4000, prepared.Session.PeakContextTokens)
	})
}
