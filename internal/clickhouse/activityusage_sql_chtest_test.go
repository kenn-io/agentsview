//go:build chtest

package clickhouse

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/pricing/catalog"
	"go.kenn.io/agentsview/internal/readbase"
)

// sqlUsageTestStart opens a range that holds the Kimi alias cutoff, a
// catalog start date, and nightly time-of-day windows, so rows on either
// side of each price change land in one report.
var sqlUsageTestStart = time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)

// sqlUsageTestResolver prices the generated models. Its rates change at
// billing time (night-model, kimi-for-coding), carry request bands, scale
// for a provider billing policy (positai), switch to a priority tier
// (openai-priority), or fail to scale (huge-model under positai).
func sqlUsageTestResolver(t *testing.T) *export.PricingResolver {
	t.Helper()
	genAI, err := pricingpkg.ParseGenAIPrices([]byte(`[
		{
			"id": "offpeak",
			"name": "Off Peak",
			"api_pattern": "https://example.invalid",
			"model_match": {"contains": "night-model"},
			"models": [{
				"id": "night-model",
				"match": {"equals": "night-model"},
				"prices": [
					{"prices": {"input_mtok": 10, "output_mtok": 30}},
					{"constraint": {"start_date": "2026-07-20"}, "prices": {"input_mtok": 8, "output_mtok": 24}},
					{"constraint": {"start_time": "22:00:00+00:00", "end_time": "02:00:00+00:00"},
					 "prices": {"input_mtok": 2, "output_mtok": 6}}
				]
			}]
		}
	]`))
	require.NoError(t, err)
	bands := []export.PricingBand{
		{AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("6"), OutputPerMTok: money.MustParseDollars("22"),
			CacheReadPerMTok: money.MustParseDollars("0.6"), CacheWritePerMTok: money.MustParseDollars("7.5")},
		// A second band at the same threshold never prices a request.
		{AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("60")},
		{AboveInputTokens: 500_000, InputPerMTok: money.MustParseDollars("9"), OutputPerMTok: money.MustParseDollars("30"),
			CacheWrite1hPerMTok: money.MustParseDollars("19")},
	}
	return export.NewPricingResolver([]export.EffectivePricingRow{
		{GenAI: genAI, GenAISource: export.PricingRowSourceEmbedded},
		{ModelPattern: "model-a", Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
			CacheWritePerMTok: money.MustParseDollars("3.75"), CacheWrite1hPerMTok: money.MustParseDollars("6"),
			CacheReadPerMTok: money.MustParseDollars("0.3"), Source: export.PricingRowSourceFetched, Bands: bands,
		}},
		{ModelPattern: "model-b", Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("1"), OutputPerMTok: money.MustParseDollars("5"),
			CacheWritePerMTok: money.MustParseDollars("1.25"), Source: export.PricingRowSourceFetched,
		}},
		{ModelPattern: catalog.PriorityPattern("model-b"), Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("10"),
			Source: export.PricingRowSourceFetched,
		}},
		{ModelPattern: "huge-model", Rates: export.ModelRates{
			InputPerMTok: money.Money{Microdollars: math.MaxInt64 - 10}, Source: export.PricingRowSourceFetched,
		}},
		{ModelPattern: pricingpkg.KimiK26Canonical, Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("1"), OutputPerMTok: money.MustParseDollars("4"), Source: export.PricingRowSourceFetched,
		}},
		{ModelPattern: pricingpkg.KimiK3Canonical, Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("4"), OutputPerMTok: money.MustParseDollars("16"), Source: export.PricingRowSourceFetched,
		}},
	})
}

// sqlUsageTestRows generates one session's rows. Identities come from small
// pools, so sessions share Claude pairs, source UUIDs, and usage keys, and
// timestamps tie across sessions; pricing timestamps may differ from the
// row's own timestamp, cross a price change, or be missing.
func sqlUsageTestRows(rng *rand.Rand, id string, copilot bool) []clickSessionUsageOrderedRow {
	pick := func(options ...string) string { return options[rng.IntN(len(options))] }
	n := rng.IntN(16)
	rows := make([]clickSessionUsageOrderedRow, 0, n)
	for i := range n {
		ts := sqlUsageTestStart.Add(time.Duration(rng.IntN(5*24*60))*time.Minute +
			time.Duration(rng.IntN(3))*time.Microsecond)
		if rng.IntN(25) == 0 {
			ts = sqlUsageTestStart
		}
		pricingTS := ts
		switch rng.IntN(8) {
		case 0:
			pricingTS = time.Time{}
		case 1:
			pricingTS = ts.Add(-time.Duration(rng.IntN(48*60)) * time.Minute)
		}
		row := clickActivityReportUsageRow{
			sessionID: id, source: "message",
			model:      pick("model-a", "model-a", "model-b", "model-c", "night-model", "kimi-for-coding"),
			providerID: pick("anthropic", "anthropic", "anthropic", pricingpkg.PositAssistantProviderID, pricingpkg.OpenAIPriorityProviderID),
			ts:         ts.Format(time.RFC3339Nano), agent: pick("claude", "codex", ""),
			messageOrdinal:    sql.NullInt64{Int64: int64(i), Valid: true},
			inputTok:          rng.IntN(700_000),
			outputTok:         rng.IntN(5),
			cacheCr:           rng.IntN(200),
			cacheRd:           rng.IntN(100),
			reasoningTok:      rng.IntN(3),
			webSearchRequests: rng.IntN(3),
		}
		row.cacheCr1h = rng.IntN(row.cacheCr + 50)
		if !pricingTS.IsZero() {
			row.pricingTS = pricingTS.Format(time.RFC3339Nano)
		}
		switch rng.IntN(6) {
		case 0:
			row.source, row.messageOrdinal = "event", sql.NullInt64{}
		case 1:
			row.source, row.messageOrdinal = pick("goose-request", "posit-assistant-chat", "rollup"), sql.NullInt64{}
		}
		if rng.IntN(8) == 0 {
			row.inputTok, row.outputTok, row.cacheCr, row.cacheCr1h, row.cacheRd, row.reasoningTok = 0, 0, 0, 0, 0, 0
		}
		switch k := rng.IntN(12); {
		case k < 5:
			row.claudeMessageID = fmt.Sprintf("msg-%d", rng.IntN(30))
			if request := rng.IntN(3); request < 2 {
				row.claudeRequestID = fmt.Sprintf("req-%d", request)
			}
		case k == 5:
			// Two pairs whose joined identities collide.
			row.claudeMessageID, row.claudeRequestID = pick("msg:x", "msg"), pick("y", "x:y")
		case k == 6:
			row.sourceUUID = fmt.Sprintf("uuid-%d", rng.IntN(8))
		case k == 7:
			row.usageDedupKey = fmt.Sprintf("key-%d", rng.IntN(8))
		}
		switch rng.IntN(12) {
		case 0:
			row.cost = sql.NullInt64{Int64: int64(rng.IntN(5000)), Valid: true}
			row.costSource = "reported"
		case 1:
			if copilot {
				row.cost = sql.NullInt64{Int64: int64(rng.IntN(90_000)), Valid: true}
				row.costSource = db.CopilotReportedCostSource
			}
		case 2, 3:
			row.recordedCost = sql.NullInt64{Int64: int64(rng.IntN(4000)), Valid: true}
		}
		rows = append(rows, clickSessionUsageOrderedRow{
			scan: row, ts: ts, validTS: true, ordinal: db.UsageRowMessageOrdinal(row.messageOrdinal),
		})
		if row.claudeMessageID != "" && rng.IntN(3) == 0 {
			// A copy of the snapshot that ties with it on everything the
			// selection compares but prices differently.
			twin := row
			twin.messageOrdinal.Int64 += 100
			twin.inputTok = rng.IntN(300_000)
			rows = append(rows, clickSessionUsageOrderedRow{
				scan: twin, ts: ts, validTS: true, ordinal: db.UsageRowMessageOrdinal(twin.messageOrdinal),
			})
		}
	}
	return rows
}

// sqlUsageTestTable holds generated usage rows in the columns of prepared
// usage, for sumActivityUsage to read as its source.
type sqlUsageTestTable struct {
	store *Store
	conn  *sql.DB
}

func newSQLUsageTestTable(t *testing.T) *sqlUsageTestTable {
	t.Helper()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	_, err := conn.ExecContext(t.Context(), `CREATE TABLE usage_rows (
		session_id String, message_ordinal Nullable(Int64),
		ts Nullable(DateTime64(6, 'UTC')), pricing_ts Nullable(DateTime64(6, 'UTC')),
		source String, model String, provider_id String, agent String,
		claude_message_id String, claude_request_id String, source_uuid String, usage_dedup_key String,
		input_tokens_norm Int64, output_tokens_norm Int64, cache_create_norm Int64, cache_create_1h_norm Int64,
		cache_read_norm Int64, reasoning_tokens_norm Int64, web_search_requests_norm Int64,
		cost_microdollars Nullable(Int64), cost_source String, recorded_cost_microdollars Nullable(Int64),
		test_row UInt32
	) ENGINE = MergeTree ORDER BY tuple()`)
	require.NoError(t, err)
	return &sqlUsageTestTable{store: newTestStore(t, conn, Target{URL: dsn, Database: database}), conn: conn}
}

// replace makes rows the table's contents.
func (u *sqlUsageTestTable) replace(t *testing.T, rows []clickSessionUsageOrderedRow) {
	t.Helper()
	ctx := t.Context()
	_, err := u.conn.ExecContext(ctx, "TRUNCATE TABLE usage_rows")
	require.NoError(t, err)
	tx, err := u.conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO usage_rows")
	require.NoError(t, err)
	nullTime := func(text string) any {
		if text == "" {
			return nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		require.NoError(t, err)
		return parsed
	}
	nullInt := func(v sql.NullInt64) any {
		if !v.Valid {
			return nil
		}
		return v.Int64
	}
	for i, o := range rows {
		r := o.scan
		_, err := stmt.ExecContext(ctx, r.sessionID, nullInt(r.messageOrdinal), nullTime(r.ts), nullTime(r.pricingTS),
			r.source, r.model, r.providerID, r.agent, r.claudeMessageID, r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
			int64(r.inputTok), int64(r.outputTok), int64(r.cacheCr), int64(r.cacheCr1h), int64(r.cacheRd),
			int64(r.reasoningTok), int64(r.webSearchRequests), nullInt(r.cost), r.costSource, nullInt(r.recordedCost), uint32(i))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// ties returns the tie-break hash of each row replace stored, by position.
func (u *sqlUsageTestTable) ties(t *testing.T) map[int]int64 {
	t.Helper()
	rows, err := u.conn.QueryContext(t.Context(), "SELECT test_row, "+clickActivityUsageTieSQL+" FROM usage_rows")
	require.NoError(t, err)
	defer rows.Close()
	ties := map[int]int64{}
	for rows.Next() {
		var i uint32
		var tie int64
		require.NoError(t, rows.Scan(&i, &tie))
		ties[int(i)] = tie
	}
	require.NoError(t, rows.Err())
	return ties
}

func (u *sqlUsageTestTable) source() activityUsageSource {
	return activityUsageSource{
		sql:    "SELECT " + chPreparedUsageColumns + " FROM usage_rows",
		tables: func() ([]*ext.Table, error) { return nil, nil },
	}
}

// sumAndSelect returns the usage of the SQL path and of selectActivityUsage
// over the same rows, each priced on its own resolver.
func (u *sqlUsageTestTable) sumAndSelect(
	t *testing.T, g *usageTestRange,
) (got []activity.UsageRow, gotPricing *export.PricingResolver, gotErr error,
	want []activity.UsageRow, wantPricing *export.PricingResolver, wantErr error,
) {
	t.Helper()
	all := g.all()
	u.replace(t, all)
	// A read returns rows that tie on time, session, and ordinal in no set
	// order, and the Go path keeps them in the order they arrive; the SQL
	// orders them by a hash of their other columns. Arriving in that order
	// makes the two paths comparable.
	ties := u.ties(t)
	order := make([]int, len(all))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		a, b := all[i], all[j]
		return cmp.Or(cmp.Compare(a.ts.UnixMicro(), b.ts.UnixMicro()), cmp.Compare(a.scan.sessionID, b.scan.sessionID),
			cmp.Compare(a.ordinal, b.ordinal), cmp.Compare(ties[i], ties[j]))
	})
	arrived := make([]clickSessionUsageOrderedRow, len(all))
	for i, k := range order {
		arrived[i] = all[k]
	}
	all = arrived
	gotPricing, wantPricing = sqlUsageTestResolver(t), sqlUsageTestResolver(t)
	got, gotErr = u.store.sumActivityUsage(t.Context(), u.source(), chSessionSetFromIDs(g.ids()), g.q, gotPricing)
	ordered, count := activityUsageSelection(all, g.ids())
	want, wantErr = selectActivityUsage(t.Context(), ordered, count, g.ids(), g.q, wantPricing)
	return got, gotPricing, gotErr, want, wantPricing, wantErr
}

// The SQL path must give the report and the pricing block that selecting
// and pricing every row in Go gives, across the rules of billing-time
// pricing: rates that change with the time of day, a catalog start date,
// the Kimi alias cutoff, a pricing timestamp apart from the row's timestamp
// or missing, request bands, provider billing policies, priority tiers,
// reported and recorded costs, Copilot session totals, and zero-token rows,
// along with the survivor rules over candidates, Claude pairs, dedup
// tokens, the effective end, and bucket windows.
func TestSQLActivityUsageMatchesGoSelection(t *testing.T) {
	t.Parallel()
	u := newSQLUsageTestTable(t)
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	end := sqlUsageTestStart.Add(5 * 24 * time.Hour)
	queries := []activity.Query{
		{Loc: time.UTC, RangeStart: sqlUsageTestStart, RangeEnd: end, EffectiveEnd: end,
			Bucket: activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400}},
		{Loc: time.UTC, RangeStart: sqlUsageTestStart.Add(90 * time.Minute), RangeEnd: end,
			EffectiveEnd: sqlUsageTestStart.Add(70*time.Hour + 500*time.Nanosecond), Partial: true,
			Bucket: activity.BucketSpec{Unit: activity.BucketHour, NominalSeconds: 3600}},
		{Loc: newYork, RangeStart: sqlUsageTestStart.Add(4 * time.Hour), RangeEnd: end.Add(4 * time.Hour),
			EffectiveEnd: end.Add(4 * time.Hour), Bucket: activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400}},
	}
	for seed := range uint64(8) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 19))
			g := &usageTestRange{
				rows: map[string][]clickSessionUsageOrderedRow{}, candidates: map[string]bool{},
				q: queries[seed%uint64(len(queries))],
			}
			for i := range 40 {
				id := fmt.Sprintf("s%02d", i)
				g.rows[id] = sqlUsageTestRows(rng, id, rng.IntN(6) == 0)
				g.candidates[id] = rng.IntN(4) != 0
			}
			got, gotPricing, gotErr, want, wantPricing, wantErr := u.sumAndSelect(t, g)
			require.NoError(t, wantErr)
			require.NoError(t, gotErr)
			wantReport, wantBlock := usageTestReport(t, g, want, wantPricing)
			gotReport, gotBlock := usageTestReport(t, g, got, gotPricing)
			require.Equal(t, wantReport, gotReport)
			require.Equal(t, wantBlock, gotBlock)
			require.NotZero(t, wantReport.Report.Totals.Cost.Microdollars)
		})
	}
}

// A survivor that cannot be priced fails the report in both paths: a
// billing policy that overflows the rates, and a cost that overflows.
func TestSQLActivityUsagePricingFailures(t *testing.T) {
	t.Parallel()
	u := newSQLUsageTestTable(t)
	end := sqlUsageTestStart.Add(24 * time.Hour)
	q := activity.Query{Loc: time.UTC, RangeStart: sqlUsageTestStart, RangeEnd: end, EffectiveEnd: end,
		Bucket: activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400}}
	row := func(provider string, input int) clickSessionUsageOrderedRow {
		ts := sqlUsageTestStart.Add(time.Hour)
		text := ts.Format(time.RFC3339Nano)
		return clickSessionUsageOrderedRow{scan: clickActivityReportUsageRow{
			sessionID: "s00", source: "message", model: "huge-model", providerID: provider,
			ts: text, pricingTS: text, messageOrdinal: sql.NullInt64{Valid: true}, inputTok: input,
		}, ts: ts, validTS: true}
	}
	for name, r := range map[string]clickSessionUsageOrderedRow{
		"billing policy": row(pricingpkg.PositAssistantProviderID, 1),
		"cost overflow":  row("anthropic", 10_000_000),
	} {
		t.Run(name, func(t *testing.T) {
			g := &usageTestRange{
				rows: map[string][]clickSessionUsageOrderedRow{"s00": {r}}, candidates: map[string]bool{"s00": true}, q: q,
			}
			_, _, gotErr, _, _, wantErr := u.sumAndSelect(t, g)
			require.Error(t, wantErr)
			require.Error(t, gotErr)
			if name == "cost overflow" {
				require.ErrorIs(t, wantErr, money.ErrOverflow)
				require.ErrorIs(t, gotErr, money.ErrOverflow)
				return
			}
			require.EqualError(t, gotErr, wantErr.Error())
		})
	}
}

// usageTestRange is a generated report: every session's rows, the
// candidate sessions, and the query.
type usageTestRange struct {
	rows       map[string][]clickSessionUsageOrderedRow
	candidates map[string]bool
	q          activity.Query
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

// sumActivityUsage is the usage readActivityUsageSums gives a report, with
// its pricing recorded on pricing.
func (s *Store) sumActivityUsage(
	ctx context.Context, source activityUsageSource, candidates chSessionSet,
	q activity.Query, pricing *export.PricingResolver,
) ([]activity.UsageRow, error) {
	startUS, endUS := activityUsageSpan(q)
	if startUS >= endUS {
		return []activity.UsageRow{}, nil
	}
	sums, err := s.readActivityUsageSums(ctx, source, candidates, q, pricing, startUS, endUS)
	if err != nil {
		return nil, err
	}
	return readbase.AssembleActivityUsage(pricing, sums.schedule, sums.groups)
}
