package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// usageBenchRange generates a 31-week range in progress with about
// 1.5 million usage rows over 90,000 sessions: about half the rows carry a
// Claude pair, an eighth of those shared with another session, a few
// percent carry a source UUID or usage key, and the rest carry no token.
// Another 15,000 candidate sessions have no usage.
type usageBenchRange struct {
	rng      *rand.Rand
	q        activity.Query
	ids      []string
	sessions []activity.SessionMeta
	pairs    []string
}

const (
	usageBenchSessions   = 90_000
	usageBenchCandidates = 105_000
)

func newUsageBenchRange() *usageBenchRange {
	start := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	g := &usageBenchRange{
		rng: rand.New(rand.NewPCG(1, 2)),
		q: activity.Query{
			Loc: time.UTC, RangeStart: start, RangeEnd: start.Add(31 * 7 * 24 * time.Hour),
			EffectiveEnd: start.Add(30 * 7 * 24 * time.Hour), Partial: true,
			Bucket: activity.BucketSpec{Unit: activity.BucketWeek, NominalSeconds: 7 * 86400},
		},
	}
	for i := range usageBenchCandidates {
		id := fmt.Sprintf("bench-session-%06d", i)
		g.ids = append(g.ids, id)
		g.sessions = append(g.sessions, activity.SessionMeta{
			SessionID: id, Project: fmt.Sprintf("project-%d", i%40), Agent: "claude", IsSubagent: i%5 == 0,
		})
	}
	return g
}

func (g *usageBenchRange) sessionRows(session int) []clickSessionUsageOrderedRow {
	id := g.ids[session]
	began := g.q.RangeStart.Add(time.Duration(session) * (30 * 7 * 24 * time.Hour / usageBenchSessions))
	n := 1 + g.rng.IntN(32)
	rows := make([]clickSessionUsageOrderedRow, 0, n)
	for i := range n {
		ts := began.Add(time.Duration(i*20+g.rng.IntN(20)) * time.Second)
		text := ts.Format(time.RFC3339Nano)
		row := clickActivityReportUsageRow{
			sessionID: id, source: "message", model: []string{"model-a", "model-a", "model-b", "model-c"}[g.rng.IntN(4)],
			providerID: "anthropic", ts: text, pricingTS: text, agent: "claude",
			messageOrdinal: sql.NullInt64{Int64: int64(i), Valid: true},
			inputTok:       g.rng.IntN(5000), outputTok: g.rng.IntN(2000), cacheRd: g.rng.IntN(100_000),
		}
		switch k := g.rng.IntN(100); {
		case k < 54:
			if len(g.pairs) > 0 && g.rng.IntN(8) == 0 {
				pair := g.pairs[g.rng.IntN(len(g.pairs))]
				row.claudeMessageID, row.claudeRequestID = "msg_01"+pair, "req_011"+pair
			} else {
				pair := fmt.Sprintf("%024d", g.rng.Uint64())
				g.pairs = append(g.pairs, pair)
				if len(g.pairs) > 4096 {
					g.pairs = g.pairs[1:]
				}
				row.claudeMessageID, row.claudeRequestID = "msg_01"+pair, "req_011"+pair
			}
		case k < 58:
			row.sourceUUID = fmt.Sprintf("%08x-0000-4000-8000-%012x", g.rng.Uint32(), g.rng.Uint64()&0xffffffffffff)
		case k < 59:
			row.usageDedupKey = fmt.Sprintf("codex:%d", g.rng.Uint64())
		}
		rows = append(rows, clickSessionUsageOrderedRow{
			scan: row, ts: ts, validTS: true, ordinal: db.UsageRowMessageOrdinal(row.messageOrdinal),
		})
	}
	return rows
}

// BenchmarkActivityUsageRefresh measures the usage phase of an Activity
// report over a range in progress after a push changed five sessions:
// selecting and pricing the survivors, and the report builder summing them.
// "full" selects from every kept row, as the report did before it kept
// survivors; "kept" re-derives what the push changed.
func BenchmarkActivityUsageRefresh(b *testing.B) {
	g := newUsageBenchRange()
	var all []clickSessionUsageOrderedRow
	for session := range usageBenchSessions {
		all = append(all, g.sessionRows(session)...)
	}
	kept := keepTestUsage(all)
	b.Logf("%d rows over %d sessions", kept.count, usageBenchSessions)
	resolver, digest := usageTestResolver(0)

	// Two versions of the pushed sessions, so every refresh changes them.
	pushedSessions := []int{17, 9_000, 40_000, 77_000, 89_000}
	var pushedIDs []string
	versions := make([]*activityUsageKept, 2)
	for v := range versions {
		var rows []clickSessionUsageOrderedRow
		for _, session := range pushedSessions {
			rows = append(rows, g.sessionRows(session)...)
		}
		versions[v] = keepTestUsage(rows)
	}
	for _, session := range pushedSessions {
		pushedIDs = append(pushedIDs, g.ids[session])
	}
	build := func(b *testing.B, usage []activity.UsageRow) {
		b.Helper()
		q := g.q
		_, err := activity.BuildCandidateArtifactsFromSourceWithSurvivorUsage(b.Context(), activity.Params{
			RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
			Partial: q.Partial, Bucket: q.Bucket,
		}, g.sessions, func(context.Context, func(activity.IntervalCandidate) error) error { return nil },
			func() ([]activity.UsageRow, error) { return usage, nil })
		require.NoError(b, err)
	}

	for _, withBuilder := range []bool{false, true} {
		name := "usage"
		if withBuilder {
			name = "usage+builder"
		}
		b.Run("full/"+name, func(b *testing.B) {
			for b.Loop() {
				pricing, _ := usageTestResolver(0)
				ordered, count := activityUsageSelection(kept, g.ids)
				usage, err := selectActivityUsage(b.Context(), ordered, count, g.ids, g.q, pricing)
				require.NoError(b, err)
				if withBuilder {
					build(b, usage)
				}
			}
		})
		b.Run("kept/"+name, func(b *testing.B) {
			var r activityUsageRange
			r.reset(nil, kept)
			usage, err := r.usage(b.Context(), g.ids, g.q, resolver, digest)
			require.NoError(b, err)
			b.Logf("%d survivor rows; kept range %d MiB, its rows alone %d MiB", len(usage),
				usageRangeBytes(&r)>>20, keptUsageBytes(kept)>>20)
			v := 0
			for b.Loop() {
				v = 1 - v
				r.patch(nil, pushedIDs, versions[v])
				g.q.EffectiveEnd = g.q.EffectiveEnd.Add(time.Second)
				pricing, _ := usageTestResolver(0)
				usage, err := r.usage(b.Context(), g.ids, g.q, pricing, digest)
				require.NoError(b, err)
				if withBuilder {
					build(b, usage)
				}
			}
			b.ReportMetric(float64(r.survivors.decoded), "rows-decoded")
		})
	}
	b.Run("kept/cold", func(b *testing.B) {
		for b.Loop() {
			var r activityUsageRange
			r.reset(nil, kept)
			pricing, _ := usageTestResolver(0)
			_, err := r.usage(b.Context(), g.ids, g.q, pricing, digest)
			require.NoError(b, err)
		}
	})
}
