package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// usageTestRange is a generated range in progress: every session's rows,
// the candidate sessions, and the query.
type usageTestRange struct {
	rng        *rand.Rand
	rows       map[string][]clickSessionUsageOrderedRow
	candidates map[string]bool
	q          activity.Query
	next       int
}

var usageTestStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// sessionRows generates one session's rows. Tokens come from small pools
// so that sessions share Claude pairs, source UUIDs, and usage keys, and
// timestamps fall on whole minutes so that rows tie across sessions.
func (g *usageTestRange) sessionRows(id string) []clickSessionUsageOrderedRow {
	n := g.rng.IntN(14)
	rows := make([]clickSessionUsageOrderedRow, 0, n)
	copilot := g.rng.IntN(8) == 0
	for i := range n {
		var ts time.Time
		switch g.rng.IntN(20) {
		case 0:
			ts = g.q.RangeStart
		case 1:
			ts = g.q.RangeEnd
		default:
			ts = usageTestStart.Add(time.Duration(g.rng.IntN(48*60)) * time.Minute)
		}
		text := ts.Format(time.RFC3339Nano)
		row := clickActivityReportUsageRow{
			sessionID: id, source: "message", model: []string{"model-a", "model-b", "model-c"}[g.rng.IntN(3)],
			providerID: "anthropic", ts: text, pricingTS: text, agent: "claude",
			messageOrdinal: sql.NullInt64{Int64: int64(i), Valid: true},
			inputTok:       g.rng.IntN(300_000), outputTok: g.rng.IntN(4), cacheRd: g.rng.IntN(100),
			webSearchRequests: g.rng.IntN(3),
		}
		if g.rng.IntN(5) == 0 {
			row.source, row.messageOrdinal = "event", sql.NullInt64{}
		}
		switch k := g.rng.IntN(10); {
		case k < 5:
			row.claudeMessageID = fmt.Sprintf("msg-%d", g.rng.IntN(30))
			// Claude Agent SDK transcripts omit requestId.
			if request := g.rng.IntN(3); request < 2 {
				row.claudeRequestID = fmt.Sprintf("req-%d", request)
			}
		case k == 5:
			row.sourceUUID = fmt.Sprintf("uuid-%d", g.rng.IntN(8))
		case k == 6:
			row.usageDedupKey = fmt.Sprintf("key-%d", g.rng.IntN(8))
		}
		switch g.rng.IntN(10) {
		case 0:
			row.cost = sql.NullInt64{Int64: int64(g.rng.IntN(5000)), Valid: true}
			row.costSource = "reported"
		case 1:
			if copilot {
				row.cost = sql.NullInt64{Int64: int64(g.rng.IntN(90_000)), Valid: true}
				row.costSource = db.CopilotReportedCostSource
			}
		}
		rows = append(rows, clickSessionUsageOrderedRow{
			scan: row, ts: ts, validTS: true, ordinal: db.UsageRowMessageOrdinal(row.messageOrdinal),
		})
		if row.claudeMessageID != "" && g.rng.IntN(3) == 0 {
			// A copy of the snapshot that ties with it on everything the
			// selection compares but prices differently.
			twin := row
			twin.messageOrdinal.Int64 += 100
			twin.inputTok = g.rng.IntN(300_000)
			rows = append(rows, clickSessionUsageOrderedRow{
				scan: twin, ts: ts, validTS: true, ordinal: db.UsageRowMessageOrdinal(twin.messageOrdinal),
			})
		}
	}
	return rows
}

func (g *usageTestRange) all() []clickSessionUsageOrderedRow {
	var out []clickSessionUsageOrderedRow
	for _, id := range slices.Sorted(maps.Keys(g.rows)) {
		out = append(out, g.rows[id]...)
	}
	return out
}

func (g *usageTestRange) ids() []string {
	ids := []string{"absent"}
	for _, id := range slices.Sorted(maps.Keys(g.candidates)) {
		if g.candidates[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

func (g *usageTestRange) sessions() []activity.SessionMeta {
	var out []activity.SessionMeta
	for i, id := range g.ids() {
		out = append(out, activity.SessionMeta{
			SessionID: id, Project: fmt.Sprintf("project-%d", i%3), Agent: "claude",
			IsSubagent: i%4 == 1, IsAutomated: i%5 == 2,
		})
	}
	return out
}

func usageTestResolver(version int) (*export.PricingResolver, string) {
	band := []export.PricingBand{{
		AboveInputTokens: 200_000,
		InputPerMTok:     money.MustParseDollars("6"), OutputPerMTok: money.MustParseDollars("22"),
	}}
	input := money.MustParseDollars(strconv.Itoa(3 + version))
	rows := []export.EffectivePricingRow{
		{ModelPattern: "model-a", Rates: export.ModelRates{
			InputPerMTok: input, OutputPerMTok: money.MustParseDollars("15"),
			CacheReadPerMTok: money.MustParseDollars("0.3"), Source: export.PricingRowSourceFetched, Bands: band,
		}},
		{ModelPattern: "model-b", Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("1"), OutputPerMTok: money.MustParseDollars("5"),
			Source: export.PricingRowSourceFetched,
		}},
	}
	digest := fmt.Sprintf("digest-%d", version)
	return export.NewPricingResolverWithDigest(rows, digest), digest
}

// usageTestReport builds the report the builder makes from usage.
func usageTestReport(t *testing.T, g *usageTestRange, usage []activity.UsageRow, pricing *export.PricingResolver) (activity.CandidateArtifacts, export.PricingBlock) {
	t.Helper()
	q := g.q
	artifacts, err := activity.BuildCandidateArtifactsFromSourceWithSurvivorUsage(t.Context(), activity.Params{
		RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
		Partial: q.Partial, GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
	}, g.sessions(), func(context.Context, func(activity.IntervalCandidate) error) error { return nil },
		func() ([]activity.UsageRow, error) { return usage, nil })
	require.NoError(t, err)
	block, err := pricing.BuildBlock()
	require.NoError(t, err)
	return artifacts, block
}

// The kept survivors must give the report a full selection over every kept
// row gives, after every kind of change a range in progress sees: pushes
// that move a Claude pair's best snapshot to another session or drop it,
// sessions entering and leaving the candidate set, a moving effective end,
// a pricing change, and another bucket size.
func TestKeptActivityUsageMatchesFullSelection(t *testing.T) {
	for seed := range uint64(12) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			g := &usageTestRange{
				rng:        rand.New(rand.NewPCG(seed, 7)),
				rows:       map[string][]clickSessionUsageOrderedRow{},
				candidates: map[string]bool{},
				q: activity.Query{
					Loc: time.UTC, RangeStart: usageTestStart, RangeEnd: usageTestStart.Add(48 * time.Hour),
					EffectiveEnd: usageTestStart.Add(30 * time.Hour), Partial: true,
					Bucket: activity.BucketSpec{Unit: activity.BucketHour, NominalSeconds: 3600},
				},
			}
			for range 30 {
				id := fmt.Sprintf("s%02d", g.next)
				g.next++
				g.rows[id] = g.sessionRows(id)
				g.candidates[id] = g.rng.IntN(4) != 0
			}
			version := 0
			var r activityUsageRange
			r.reset(nil, keepTestUsage(g.all()))
			for step := range 24 {
				var changed []string
				switch step % 6 {
				case 1, 4:
					for range 1 + g.rng.IntN(3) {
						id := fmt.Sprintf("s%02d", g.rng.IntN(g.next+1))
						if id == fmt.Sprintf("s%02d", g.next) {
							g.next++
							g.candidates[id] = true
						}
						g.rows[id] = g.sessionRows(id)
						if g.rng.IntN(6) == 0 {
							delete(g.rows, id)
						}
						if !slices.Contains(changed, id) {
							changed = append(changed, id)
						}
					}
				case 2:
					for range 1 + g.rng.IntN(3) {
						id := fmt.Sprintf("s%02d", g.rng.IntN(g.next))
						g.candidates[id] = !g.candidates[id]
					}
				case 3:
					g.q.EffectiveEnd = g.q.EffectiveEnd.Add(time.Duration(g.rng.IntN(240)-60) * time.Minute)
				case 5:
					if step%12 == 5 {
						version++
					} else if g.q.Bucket.Unit == activity.BucketHour {
						g.q.Bucket = activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400}
					} else {
						g.q.Bucket = activity.BucketSpec{Unit: activity.BucketHour, NominalSeconds: 3600}
					}
				}
				var pushed []clickSessionUsageOrderedRow
				for _, id := range changed {
					pushed = append(pushed, g.rows[id]...)
				}
				if len(changed) > 0 {
					r.patch(nil, changed, keepTestUsage(pushed))
				}

				resolver, digest := usageTestResolver(version)
				got, err := r.usage(t.Context(), g.ids(), g.q, resolver, digest)
				require.NoError(t, err)
				gotReport, gotPricing := usageTestReport(t, g, got, resolver)

				full := keepTestUsage(g.all())
				fullResolver, _ := usageTestResolver(version)
				ordered, count := activityUsageSelection(full, g.ids())
				want, err := selectActivityUsage(t.Context(), ordered, count, g.ids(), g.q, fullResolver)
				require.NoError(t, err)
				wantReport, wantPricing := usageTestReport(t, g, want, fullResolver)

				require.Equal(t, wantReport, gotReport, "step %d", step)
				require.Equal(t, wantPricing, gotPricing, "step %d", step)
				require.NotZero(t, wantReport.Report.Totals.Cost.Microdollars, "step %d", step)
			}
		})
	}
}

// A push of a few sessions must re-derive the survivors from those sessions
// and the sessions they share tokens with, not from every kept row.
func TestKeptActivityUsageDecodesOnlyChangedSessions(t *testing.T) {
	q := activity.Query{
		Loc: time.UTC, RangeStart: usageTestStart, RangeEnd: usageTestStart.Add(48 * time.Hour),
		EffectiveEnd: usageTestStart.Add(47 * time.Hour), Partial: true,
		Bucket: activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400},
	}
	var all []clickSessionUsageOrderedRow
	var ids []string
	for i := range 200 {
		id := fmt.Sprintf("s%03d", i)
		ids = append(ids, id)
		for j := range 10 {
			all = append(all, keptTestRow(id, "message", usageTestStart.Add(time.Duration(i*10+j)*time.Minute).Format(time.RFC3339),
				int64(j), fmt.Sprintf("msg-%d-%d", i, j), "req", 10))
		}
	}
	resolver, digest := usageTestResolver(0)
	var r activityUsageRange
	r.reset(nil, keepTestUsage(all))
	_, err := r.usage(t.Context(), ids, q, resolver, digest)
	require.NoError(t, err)

	pushed := []clickSessionUsageOrderedRow{
		keptTestRow("s007", "message", usageTestStart.Add(70*time.Minute).Format(time.RFC3339), 0, "msg-7-0", "req", 11),
	}
	r.patch(nil, []string{"s007"}, keepTestUsage(pushed))
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Second)
	rows, err := r.usage(t.Context(), ids, q, resolver, digest)
	require.NoError(t, err)
	require.Less(t, r.survivors.decoded, 40)
	var outputs int
	for _, row := range rows {
		outputs += row.OutputTokens
	}
	require.Equal(t, 199*10*10+11, outputs)
}

// A report that fails after its refresh changed a kept range must leave the
// memo with the range's current size. Otherwise a range that outgrew the
// byte cap stays kept, counted at its earlier size.
func TestKeptActivityUsageFailureRecordsGrownRange(t *testing.T) {
	q := activity.Query{
		Loc: time.UTC, RangeStart: usageTestStart, RangeEnd: usageTestStart.Add(48 * time.Hour),
		EffectiveEnd: usageTestStart.Add(47 * time.Hour), Partial: true,
		Bucket: activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400},
	}
	row := func(session string, i int) clickSessionUsageOrderedRow {
		return keptTestRow(session, "message", usageTestStart.Add(time.Duration(i)*time.Minute).Format(time.RFC3339),
			int64(i), "", "", 1)
	}
	const lower, upper = "2026-09-01T00:00:00Z", "2999-01-01T00:00:00Z"
	slot := lower + "|" + upper
	s := NewStoreFromDB(nil, nil)
	r := &activityUsageRange{}
	r.reset(map[string]string{"session-a": "1"}, keepTestUsage([]clickSessionUsageOrderedRow{row("session-a", 0)}))
	s.activityUsageRanges.MaxBytes = usageRangeBytes(r) + 1
	s.activityUsageRanges.Put(slot, "", []*activityUsageRange{r})
	_, kept := s.activityUsageRanges.Get(slot, "")
	require.True(t, kept)

	// A refresh read session-b's new rows, then the report was canceled.
	revisions := map[string]string{"session-a": "1", "session-b": "1"}
	var pushed []clickSessionUsageOrderedRow
	for i := range 10 {
		pushed = append(pushed, row("session-b", i))
	}
	r.patch(revisions, []string{"session-b"}, keepTestUsage(pushed))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	resolver, digest := usageTestResolver(0)
	_, err := s.keptActivityUsage(ctx, preparedUsageState{revisions: revisions}, lower, upper,
		[]string{"session-a", "session-b"}, q, resolver, digest)
	require.ErrorIs(t, err, context.Canceled)

	_, kept = s.activityUsageRanges.Get(slot, "")
	require.False(t, kept, "the grown range is still kept at its earlier size")
}

// The survivors' byte estimate must cover every array their slices keep. The
// first token index reserves a place for every row, but only rows with a
// dedup token fill it.
func TestUsageSurvivorsBytesCountsReservedCapacity(t *testing.T) {
	q := activity.Query{
		Loc: time.UTC, RangeStart: usageTestStart, RangeEnd: usageTestStart.Add(48 * time.Hour),
		EffectiveEnd: usageTestStart.Add(47 * time.Hour), Partial: true,
		Bucket: activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400},
	}
	rows := []clickSessionUsageOrderedRow{
		keptTestRow("session-b", "message", usageTestStart.Format(time.RFC3339), 0, "msg", "req", 1),
	}
	for i := range 2000 {
		rows = append(rows, keptTestRow("session-a", "message",
			usageTestStart.Add(time.Duration(i)*time.Second).Format(time.RFC3339), int64(i), "", "", 1))
	}
	var r activityUsageRange
	r.reset(nil, keepTestUsage(rows))
	resolver, digest := usageTestResolver(0)
	_, err := r.usage(t.Context(), []string{"session-a", "session-b"}, q, resolver, digest)
	require.NoError(t, err)

	u := &r.survivors
	require.Greater(t, cap(u.tokens), len(u.tokens))
	arrays := int64(cap(u.tokens))*int64(unsafe.Sizeof(usageTokenRow{})) +
		int64(cap(u.results))*int64(unsafe.Sizeof(usageTokenSurvivor{}))
	require.GreaterOrEqual(t, usageSurvivorsBytes(u), arrays)
}
