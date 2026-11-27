package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/money"
)

func TestRawTranscriptPrefixRequiresCompleteRowAgreement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*ingest.PreparedSession)
		matches bool
	}{
		{"append", func(p *ingest.PreparedSession) { p.Session.MessageCount = 2; p.Session.EndedAt = new("later") }, true},
		{"changed message", func(p *ingest.PreparedSession) { p.Messages[0].Content = "different" }, false},
		{"changed usage", func(p *ingest.PreparedSession) { p.Messages[0].OutputTokens = 3; p.Messages[0].HasOutputTokens = true }, false},
		{"changed title", func(p *ingest.PreparedSession) { p.Session.SessionName = new("renamed") }, false},
		{"newer plan usage", func(p *ingest.PreparedSession) {
			p.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(2.5), HasCoverage: true, UncapturedTurns: 1}
		}, true},
		{"newer agent session total", func(p *ingest.PreparedSession) {
			p.SessionTotal = &db.SessionUsageTotal{Source: "claude-cost-state", SubagentScope: "included", Cost: &money.Money{Microdollars: 42}}
		}, true},
		{"changed tool", func(p *ingest.PreparedSession) { p.Messages[0].ToolCalls[0].InputJSON = `{"command":"false"}` }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			short := ingest.PreparedSession{Session: db.Session{MessageCount: 1}, Messages: []db.Message{{Ordinal: 0, Content: "original", ToolCalls: []db.ToolCall{{InputJSON: `{"command":"true"}`}}}}}
			encoded, err := encodeRawPayload(short)
			require.NoError(t, err)
			long, err := decodeRawPayload(encoded)
			require.NoError(t, err)
			long.Messages = append(long.Messages, db.Message{Ordinal: 1, Content: "appended"})
			tc.change(&long)
			matches, err := rawTranscriptPrefix(short, long)
			require.NoError(t, err)
			assert.Equal(t, tc.matches, matches)
		})
	}
}

func TestRawTranscriptPrefixAcceptsAGrownSessionTotalOnTheSameTranscript(t *testing.T) {
	total := func(microdollars int64) *db.SessionUsageTotal {
		return &db.SessionUsageTotal{
			Source: "claude-cost-state", SubagentScope: "included",
			Cost: &money.Money{Microdollars: microdollars},
		}
	}
	// tokens builds a total whose growth comes from cache reads, the counter
	// that grows most between Claude Code snapshots.
	tokens := func(cost *money.Money, cacheRead int64) *db.SessionUsageTotal {
		return &db.SessionUsageTotal{
			Source: "claude-cost-state", SubagentScope: "included", Cost: cost,
			Models: []db.SessionUsageTotalModel{{
				Model: "m", InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: cacheRead,
			}},
		}
	}
	capture := func(change func(*ingest.PreparedSession)) ingest.PreparedSession {
		p := ingest.PreparedSession{
			Session:  db.Session{MessageCount: 1},
			Messages: []db.Message{{Ordinal: 0, Content: "original"}},
		}
		change(&p)
		return p
	}
	for _, tc := range []struct {
		name          string
		before, after func(*ingest.PreparedSession)
		matches       bool
	}{
		{"total appended on exit", func(*ingest.PreparedSession) {}, func(p *ingest.PreparedSession) { p.SessionTotal = total(42) }, true},
		{"total grew", func(p *ingest.PreparedSession) { p.SessionTotal = total(42) }, func(p *ingest.PreparedSession) { p.SessionTotal = total(50) }, true},
		{"total shrank", func(p *ingest.PreparedSession) { p.SessionTotal = total(50) }, func(p *ingest.PreparedSession) { p.SessionTotal = total(42) }, false},
		{"same total", func(p *ingest.PreparedSession) { p.SessionTotal = total(42) }, func(p *ingest.PreparedSession) { p.SessionTotal = total(42) }, false},
		{
			"same cost, more cache tokens", func(p *ingest.PreparedSession) { p.SessionTotal = tokens(&money.Money{Microdollars: 42}, 100) },
			func(p *ingest.PreparedSession) { p.SessionTotal = tokens(&money.Money{Microdollars: 42}, 150) }, true,
		},
		{
			"no cost, more tokens", func(p *ingest.PreparedSession) { p.SessionTotal = tokens(nil, 100) },
			func(p *ingest.PreparedSession) { p.SessionTotal = tokens(nil, 150) }, true,
		},
		{
			"same cost, same tokens", func(p *ingest.PreparedSession) { p.SessionTotal = tokens(&money.Money{Microdollars: 42}, 100) },
			func(p *ingest.PreparedSession) { p.SessionTotal = tokens(&money.Money{Microdollars: 42}, 100) }, false,
		},
		{
			"same cost, fewer tokens", func(p *ingest.PreparedSession) { p.SessionTotal = tokens(&money.Money{Microdollars: 42}, 150) },
			func(p *ingest.PreparedSession) { p.SessionTotal = tokens(&money.Money{Microdollars: 42}, 100) }, false,
		},
		{"changed message", func(*ingest.PreparedSession) {}, func(p *ingest.PreparedSession) {
			p.SessionTotal = total(42)
			p.Messages[0].Content = "different"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := capture(tc.before), capture(tc.after)
			matches, err := rawTranscriptPrefix(before, after)
			require.NoError(t, err)
			assert.Equal(t, tc.matches, matches, "earlier capture extended by the later one")
			reverse, err := rawTranscriptPrefix(after, before)
			require.NoError(t, err)
			assert.False(t, matches && reverse, "two captures never extend each other")
		})
	}
}

func TestRawTranscriptPrefixMatchesAutomatedHeadlessContinuation(t *testing.T) {
	short := ingest.PreparedSession{
		Session: db.Session{
			ID: "panel-1", Agent: "claude", Entrypoint: "sdk-cli",
			RelationshipType: "subagent", IsAutomated: true,
			MessageCount: 2, UserMessageCount: 1,
		},
		Messages: []db.Message{
			{Ordinal: 0, Role: "user", Content: "You are a code reviewer. Review this change."},
			{Ordinal: 1, Role: "assistant", Content: "No findings."},
		},
	}
	short.Session.RecordParserRelationship()
	short.Session.RelationshipType = db.SessionRelationship(short.Session)
	require.Empty(t, short.Session.RelationshipType)

	long := ingest.PreparedSession{
		Session: db.Session{
			ID: "panel-1", Agent: "claude", Entrypoint: "sdk-cli",
			RelationshipType: "subagent", IsAutomated: false,
			MessageCount: 4, UserMessageCount: 2,
		},
		Messages: []db.Message{
			{Ordinal: 0, Role: "user", Content: "You are a code reviewer. Review this change."},
			{Ordinal: 1, Role: "assistant", Content: "No findings."},
			{Ordinal: 2, Role: "user", Content: "Check error paths too."},
			{Ordinal: 3, Role: "assistant", Content: "Checked."},
		},
	}
	long.Session.RecordParserRelationship()
	long.Session.RelationshipType = db.SessionRelationship(long.Session)
	require.Equal(t, "subagent", long.Session.RelationshipType)

	matches, err := rawTranscriptPrefix(short, long)
	require.NoError(t, err)
	assert.True(t, matches, "2-turn continuation of an automated headless run must match its 1-turn prefix")

	conflict := long
	conflict.Session.RelationshipType = "fork"
	conflict.Session.RestoreRelationshipProvenance("fork", false)
	matches, err = rawTranscriptPrefix(short, conflict)
	require.NoError(t, err)
	assert.False(t, matches, "genuine parser relationship conflicts must still reject prefix matching")
}

func TestRawTranscriptPrefixMergesEqualLengthAutomatedUpgrade(t *testing.T) {
	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	base := func(isAutomated bool, rel string, recordProvenance bool) ingest.PreparedSession {
		p := ingest.PreparedSession{
			Session: db.Session{
				ID: "review-1", Agent: "claude", Entrypoint: "sdk-cli",
				FirstMessage: &prompt, RelationshipType: rel, IsAutomated: isAutomated,
				EndedAt:      new("2026-11-27T01:00:00Z"),
				MessageCount: 2, UserMessageCount: 1,
			},
			Messages: []db.Message{
				{Ordinal: 0, Role: "user", Content: prompt},
				{Ordinal: 1, Role: "assistant", Content: "No findings."},
			},
			SessionTotal: &db.SessionUsageTotal{
				Source: "claude-cost-state", SubagentScope: "included",
				Cost: &money.Money{Microdollars: 42},
				Models: []db.SessionUsageTotalModel{{
					Model: "claude-sonnet", InputTokens: 100, OutputTokens: 20,
				}},
			},
		}
		if recordProvenance {
			p.Session.RecordParserRelationship()
			p.Session.RelationshipType = db.SessionRelationship(p.Session)
		}
		return p
	}

	legacyBytes, err := encodeRawPayload(base(false, "subagent", false))
	require.NoError(t, err)
	legacy, err := decodeRawPayload(legacyBytes)
	require.NoError(t, err)

	legacyAutomatedBytes, err := encodeRawPayload(base(true, "subagent", false))
	require.NoError(t, err)
	legacyAutomated, err := decodeRawPayload(legacyAutomatedBytes)
	require.NoError(t, err)

	upgraded := base(true, "subagent", true)
	require.Empty(t, upgraded.Session.RelationshipType)

	// Both pre-upgrade forms merge into the upgraded normalized capture in one direction only.
	for _, tc := range []struct {
		name string
		old  ingest.PreparedSession
	}{
		{"legacy non-automated subagent", legacy},
		{"legacy automated subagent", legacyAutomated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matches, err := rawTranscriptPrefix(tc.old, upgraded)
			require.NoError(t, err)
			assert.True(t, matches)
			reverse, err := rawTranscriptPrefix(upgraded, tc.old)
			require.NoError(t, err)
			assert.False(t, reverse)
		})
	}

	// Genuine equal-length conflicts stay separate even when automation rank increases.
	for _, tc := range []struct {
		name   string
		mutate func(*ingest.PreparedSession)
	}{
		{"changed message", func(p *ingest.PreparedSession) { p.Messages[1].Content = "Different reply." }},
		{"changed ended_at", func(p *ingest.PreparedSession) { p.Session.EndedAt = new("2026-11-27T02:00:00Z") }},
		{"different total model with same cost and tokens", func(p *ingest.PreparedSession) {
			p.SessionTotal.Models[0].Model = "claude-opus"
		}},
		{"different plan usage", func(p *ingest.PreparedSession) {
			p.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(5.0)}
		}},
		{"parser relationship conflict", func(p *ingest.PreparedSession) {
			p.Session.RestoreRelationshipProvenance("fork", false)
		}},
		{"explicit vs inferred subagent conflict", func(p *ingest.PreparedSession) {
			p.Session.RestoreRelationshipProvenance("subagent", false)
		}},
	} {
		t.Run("conflict/"+tc.name, func(t *testing.T) {
			cand := base(true, "subagent", true)
			tc.mutate(&cand)
			matches, err := rawTranscriptPrefix(legacy, cand)
			require.NoError(t, err)
			assert.False(t, matches)
		})
	}

	t.Run("conflict/explicit grok subagent vs inferred worker", func(t *testing.T) {
		explicit := base(true, "subagent", false)
		explicit.Session.Agent = "grok"
		explicit.Session.Entrypoint = ""
		explicit.Session.SessionKind = "non-interactive"
		explicit.Session.RestoreRelationshipProvenance("subagent", false)

		inferred := base(true, "subagent", false)
		inferred.Session.Agent = "grok"
		inferred.Session.Entrypoint = ""
		inferred.Session.SessionKind = "non-interactive"
		inferred.Session.RestoreRelationshipProvenance("subagent", true)
		inferred.Session.RelationshipType = db.SessionRelationship(inferred.Session)
		require.Empty(t, inferred.Session.RelationshipType)

		matches, err := rawTranscriptPrefix(explicit, inferred)
		require.NoError(t, err)
		assert.False(t, matches)
	})
}

func TestRawTranscriptPrefixDirectionalGrokProvenance(t *testing.T) {
	makeGrok := func(msgs []db.Message, inferred bool) ingest.PreparedSession {
		p := ingest.PreparedSession{
			Session: db.Session{
				ID: "grok:run-1", Agent: "grok", SessionKind: "non-interactive",
				RelationshipType: "subagent", IsAutomated: false,
				MessageCount: len(msgs), UserMessageCount: len(msgs) / 2,
			},
			Messages: msgs,
		}
		p.Session.RestoreRelationshipProvenance("subagent", inferred)
		return p
	}
	shortMsgs := []db.Message{
		{Ordinal: 0, Role: "user", Content: "Review this change."},
		{Ordinal: 1, Role: "assistant", Content: "First pass."},
	}
	longMsgs := append(append([]db.Message(nil), shortMsgs...),
		db.Message{Ordinal: 2, Role: "user", Content: "Check error handling."},
		db.Message{Ordinal: 3, Role: "assistant", Content: "Done."},
	)

	inferredShort := makeGrok(shortMsgs, true)
	explicitLong := makeGrok(longMsgs, false)
	matches, err := rawTranscriptPrefix(inferredShort, explicitLong)
	require.NoError(t, err)
	assert.True(t, matches, "inferred shorter copy should merge into explicit longer continuation")

	explicitShort := makeGrok(shortMsgs, false)
	inferredLong := makeGrok(longMsgs, true)
	matches, err = rawTranscriptPrefix(explicitShort, inferredLong)
	require.NoError(t, err)
	assert.False(t, matches, "explicit child must never merge into inferred longer worker")
}

func TestRefreshRawSessionAutomation(t *testing.T) {
	reviewPrompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	humanPrompt := "Fix the nil check in raw_projection_rows.go."
	for _, tc := range []struct {
		name      string
		session   db.Session
		messages  []db.Message
		usageOnly bool
		want      bool
	}{
		{
			name: "reclassifies legacy one-turn review prompt",
			session: db.Session{
				Entrypoint: "sdk-cli", FirstMessage: &reviewPrompt,
				UserMessageCount: 1, IsAutomated: false,
			},
			messages: []db.Message{{Ordinal: 0, Role: "user", Content: reviewPrompt}},
			want:     true,
		},
		{
			name: "keeps interactive prompt non-automated",
			session: db.Session{
				Entrypoint: "sdk-cli", FirstMessage: &humanPrompt,
				UserMessageCount: 1, IsAutomated: false,
			},
			messages: []db.Message{{Ordinal: 0, Role: "user", Content: humanPrompt}},
			want:     false,
		},
		{
			name: "preserves prompt-based automation in usage-only mode even when message text is present",
			session: db.Session{
				Entrypoint: "sdk-cli", UserMessageCount: 1, IsAutomated: true,
			},
			messages:  []db.Message{{Ordinal: 0, Role: "user", Content: humanPrompt}},
			usageOnly: true,
			want:      true,
		},
		{
			name: "preserves prompt-based automation when restored usage-only payload has emptied assistant rows",
			session: db.Session{
				Entrypoint: "sdk-cli", UserMessageCount: 1, IsAutomated: true,
			},
			messages: []db.Message{{Ordinal: 0, Role: "assistant", Content: ""}},
			want:     true,
		},
		{
			name: "preserves prompt-based automation when saved payload has no messages",
			session: db.Session{
				Entrypoint: "sdk-cli", UserMessageCount: 1, IsAutomated: true,
			},
			want: true,
		},
		{
			name: "detects metadata automation even in usage-only mode",
			session: db.Session{
				SessionKind: "roborev", UserMessageCount: 1, IsAutomated: false,
			},
			usageOnly: true,
			want:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, refreshRawSessionAutomation(tc.session, tc.messages, tc.usageOnly))
		})
	}
}
