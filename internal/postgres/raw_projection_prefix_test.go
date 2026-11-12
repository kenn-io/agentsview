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
