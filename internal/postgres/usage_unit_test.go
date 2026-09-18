package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestPaddedUTCBoundClampsBeforeYearOne(t *testing.T) {
	t.Parallel()
	assert.Equal(t,
		"0001-01-01T00:00:00Z",
		paddedUTCBound("0001-01-01T00:00:00Z", -14),
	)
	assert.Equal(t,
		"2026-03-10T10:00:00Z",
		paddedUTCBound("2026-03-11T00:00:00Z", -14),
	)
}

type usageProbeDriver struct{}

type usageProbeConn struct {
	state *usageProbeState
}

type usageProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type usageProbeState struct {
	mu      sync.Mutex
	queries []string
}

var (
	usageProbeRegisterOnce sync.Once
	usageProbeStatesMu     sync.Mutex
	usageProbeStates       = map[string]*usageProbeState{}
)

func newUsageProbeDB(
	t *testing.T, state *usageProbeState,
) *sql.DB {
	t.Helper()
	usageProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_usage_probe", usageProbeDriver{})
	})
	name := t.Name()
	usageProbeStatesMu.Lock()
	usageProbeStates[name] = state
	usageProbeStatesMu.Unlock()
	t.Cleanup(func() {
		usageProbeStatesMu.Lock()
		delete(usageProbeStates, name)
		usageProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_usage_probe", name)
	require.NoError(t, err, "open usage probe db")
	t.Cleanup(func() { pg.Close() })
	return pg
}

func (usageProbeDriver) Open(name string) (driver.Conn, error) {
	usageProbeStatesMu.Lock()
	state := usageProbeStates[name]
	usageProbeStatesMu.Unlock()
	return &usageProbeConn{state: state}, nil
}

func (c *usageProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *usageProbeConn) Close() error { return nil }

func (c *usageProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (c *usageProbeConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	c.state.mu.Unlock()

	normalized := strings.ToLower(query)
	if strings.Contains(normalized, "from genai_pricing") {
		return &usageProbeRows{columns: []string{
			"version", "source_ref", "source", "data_json", "updated_at",
		}}, nil
	}
	if strings.Contains(normalized, "from model_pricing") {
		return &usageProbeRows{
			columns: []string{
				"model_pattern",
				"input_microdollars_per_mtok",
				"output_microdollars_per_mtok",
				"cache_creation_microdollars_per_mtok",
				"cache_creation_1h_microdollars_per_mtok",
				"cache_read_microdollars_per_mtok",
				"updated_at",
				"above_input_tokens",
				"band_input_microdollars_per_mtok",
				"band_output_microdollars_per_mtok",
				"band_cache_creation_microdollars_per_mtok",
				"band_cache_creation_1h_microdollars_per_mtok",
				"band_cache_read_microdollars_per_mtok",
				"band_updated_at",
			},
			values: [][]driver.Value{{
				"claude-sonnet", int64(3000000), int64(15000000), int64(3750000), int64(0), int64(300000), "2026-06-08",
				nil, nil, nil, nil, nil, nil, nil,
			}},
		}, nil
	}
	if strings.Contains(normalized, "from source_archives") {
		return &usageProbeRows{
			columns: []string{"source_archive_id", "source_archive_salt"},
			values:  [][]driver.Value{{"probe-archive", "probe-salt"}},
		}, nil
	}
	if strings.Contains(normalized, "from source_project_identity_observations") {
		return &usageProbeRows{
			columns: []string{
				"project",
				"machine",
				"root_path",
				"git_remote",
				"git_remote_name",
				"worktree_name",
				"worktree_root_path",
				"observed_at",
				"normalized_remote",
				"key_source",
				"key",
			},
		}, nil
	}
	if strings.Contains(normalized, "select project, cwd") &&
		strings.Contains(normalized, "from sessions") {
		return &usageProbeRows{
			columns: []string{"project", "cwd"},
		}, nil
	}
	if strings.Contains(normalized, "select id from sessions") {
		return &usageProbeRows{
			columns: []string{"id"},
			values: [][]driver.Value{
				{"kimi:project-hash:session-uuid"},
				{"openclaw:project-hash:session-uuid"},
			},
		}, nil
	}
	if strings.Contains(normalized, "select count(*)") &&
		strings.Contains(normalized, "from sessions s") {
		return &usageProbeRows{
			columns: []string{"count"},
			values: [][]driver.Value{
				{int64(1)},
			},
		}, nil
	}
	if strings.Contains(normalized, "from (") &&
		strings.Contains(normalized, "from messages") {
		ts := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
		return &usageProbeRows{
			columns: []string{
				"session_id",
				"message_ordinal",
				"usage_source",
				"ts",
				"pricing_ts",
				"model",
				"provider_id",
				"token_usage",
				"web_search_requests",
				"input_tokens",
				"output_tokens",
				"cache_creation_input_tokens",
				"cache_read_input_tokens",
				"reasoning_tokens",
				"cost_microdollars",
				"cost_source",
				"claude_message_id",
				"claude_request_id",
				"source_uuid",
				"usage_dedup_key",
				"project",
				"agent",
			},
			values: [][]driver.Value{
				usageProbeUsageRow("s-parent", "proj-a", "claude", ts),
				usageProbeUsageRow("s-fork", "proj-b", "codex", ts.Add(time.Minute)),
			},
		}, nil
	}
	return nil, errors.New("unexpected usage query")
}

func usageProbeUsageRow(
	sessionID, project, agent string, ts time.Time,
) []driver.Value {
	return []driver.Value{
		sessionID,
		int64(0),
		"message",
		ts,
		ts,
		"claude-sonnet",
		"",
		`{"input_tokens":100,"output_tokens":50}`,
		int64(0),
		int64(0),
		int64(0),
		int64(0),
		int64(0),
		int64(0),
		nil,
		"",
		"msg-dup",
		"req-dup",
		"",
		"",
		project,
		agent,
	}
}

func (r *usageProbeRows) Columns() []string { return r.columns }

func (r *usageProbeRows) Close() error { return nil }

func (r *usageProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestPGGetDailyUsageReturnsDedupedSessionCounts(t *testing.T) {
	assert := assert.New(t)

	store := &Store{
		pg: newUsageProbeDB(t, &usageProbeState{}),
	}

	result, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2024-06-15",
		To:   "2024-06-15",
	})
	require.NoError(t, err, "GetDailyUsage")

	assert.Equal(1, result.SessionCounts.Total)
	countsByDisplay := make(map[string]int, len(result.Projects))
	for key, project := range result.Projects {
		countsByDisplay[project.DisplayLabel] = result.SessionCounts.ByProject[key]
		assert.NotContains(key, project.DisplayLabel)
	}
	assert.Equal(map[string]int{"proj-a": 1}, countsByDisplay)
	assert.Equal(1, result.SessionCounts.ByAgent["claude"])
	assert.NotContains(countsByDisplay, "proj-b")
	assert.Zero(result.SessionCounts.ByAgent["codex"])
}

func TestPGUsageDedupTokenForRowFallsBackToSourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	got, ok := pgUsageDedupTokenForRow(
		"message",
		"claude-code",
		"msg-dup",
		"",
		"source-dup",
		"",
	)
	require.True(t, ok, "expected source_uuid fallback key")
	assert.Equal(t, pgUsageDedupToken{
		kind:  "source",
		value: "claude-code:source-dup",
	}, got)
}

func TestPGUsageAmountsPreserveSessionSummaryUsageEventTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "gpt-5.4",
		Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("1.0"), OutputPerMTok: money.MustParseDollars("2.0"),
		},
	}})

	inTok, outTok, _, _, cost, _, priceErr := pgDailyUsageAmounts(
		pgDailyUsageScanRow{
			usageSource:  "session",
			model:        "gpt-5.4",
			inputTokens:  rawInput,
			outputTokens: rawOutput,
		},
		resolver,
	)
	require.NoError(priceErr)
	assert.Equal(rawInput, inTok, "daily input")
	assert.Equal(rawOutput, outTok, "daily output")
	wantCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(rawInput), Rate: money.MustParseDollars("1")},
		{Tokens: int64(rawOutput), Rate: money.MustParseDollars("2")},
	})
	require.NoError(err)
	assert.Equal(wantCost, cost, "daily cost")

	cost, priced, contributes, priceErr := pgSessionRowCost(pgUsageScanRow{
		usageSource:  "session",
		model:        "gpt-5.4",
		inputTokens:  rawInput,
		outputTokens: rawOutput,
	}, resolver)
	require.NoError(priceErr)
	require.True(priced, "priced")
	require.True(contributes, "contributes")
	assert.Equal(wantCost, cost, "session cost")
}

func TestPGDailyUsageAmountsPricingBandRequestScope(t *testing.T) {
	tests := []struct {
		name           string
		usageSource    string
		messageOrdinal sql.NullInt64
		wantCost       int64
		wantAggregate  int
		wantBand       int
	}{
		{
			name:           "ordinal-bound request uses band",
			usageSource:    "usage-event",
			messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
			wantCost:       600_000,
			wantBand:       1,
		},
		{
			name:        "Goose request uses band without message ordinal",
			usageSource: "goose-request",
			wantCost:    600_000,
			wantBand:    1,
		},
		{
			name:        "DeepSeek Harness compaction uses band without message ordinal",
			usageSource: "deepseek-harness",
			wantCost:    600_000,
			wantBand:    1,
		},
		{
			name:          "unbound aggregate uses base",
			usageSource:   "usage-event",
			wantCost:      300_000,
			wantAggregate: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			resolver := pgPricingBandTestResolver()
			_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
				messageOrdinal: tt.messageOrdinal,
				usageSource:    tt.usageSource,
				model:          "banded-model",
				inputTokens:    300_000,
			}, resolver)
			require.NoError(err)
			block, err := resolver.BuildBlock()
			require.NoError(err)
			provenance := block.Models["banded-model"]
			require.Len(provenance.Resolutions, 1)
			application := provenance.Resolutions[0].Application

			assert.Equal(money.Money{Microdollars: tt.wantCost}, cost)
			assert.Equal(tt.wantAggregate, application.AggregateRowCount)
			if tt.wantBand > 0 {
				require.Len(application.Bands, 1)
				assert.Equal(tt.wantBand, application.Bands[0].RequestCount)
			}
		})
	}
}

func pgPricingBandTestResolver() *export.PricingResolver {
	return export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "banded-model",
		Rates: export.ModelRates{
			InputPerMTok:      money.MustParseDollars("1"),
			OutputPerMTok:     money.MustParseDollars("2"),
			CacheWritePerMTok: money.MustParseDollars("0.50"),
			CacheReadPerMTok:  money.MustParseDollars("0.10"),
			Bands: []export.PricingBand{{
				AboveInputTokens:  200_000,
				InputPerMTok:      money.MustParseDollars("2"),
				OutputPerMTok:     money.MustParseDollars("3"),
				CacheWritePerMTok: money.MustParseDollars("1"),
				CacheReadPerMTok:  money.MustParseDollars("0.20"),
			}},
		},
	}})
}

func TestPGUsageRowQueryPushesDateBoundsIntoUnion(t *testing.T) {
	assert := assert.New(t)

	pb := &paramBuilder{}
	query := pgUsageRowQuery(pb, db.UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		ExcludeAutomated: true,
	})

	normalized := strings.ToLower(query)
	assert.NotContains(normalized, "and u.ts >=")
	assert.NotContains(normalized, "and u.ts <=")
	assert.NotContains(normalized, " or ")
	assert.NotContains(normalized, "display_name")
	assert.NotContains(normalized, "first_message")
	assert.NotContains(normalized, "cost_status")
	assert.Contains(normalized, "u.cost_source")
	assert.Contains(normalized, "u.reasoning_tokens")
	assert.NotContains(normalized, "user_message_count")
	assert.NotContains(normalized, "session_activity_at")
	assert.NotContains(normalized, " as started_at")
	assert.NotContains(normalized, "u.machine")
	assert.Contains(normalized, "message_timestamp_rows as materialized")
	assert.Contains(normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(normalized, "from message_timestamp_rows m\njoin sessions s")
	assert.Contains(normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
	assert.Contains(normalized, "m.timestamp is not null")
	assert.Contains(normalized, "ue.occurred_at is not null")
	assert.Contains(normalized, "m.timestamp is null")
	assert.Contains(normalized, "ue.occurred_at is null")
	assert.Contains(normalized, "m.timestamp >= $1::timestamptz")
	assert.Contains(normalized, "ue.occurred_at >= $1::timestamptz")
	assert.Contains(normalized, "s.started_at >= $1::timestamptz")
	assert.Contains(normalized, "m.timestamp <= $2::timestamptz")
	assert.Contains(normalized, "ue.occurred_at <= $2::timestamptz")
	assert.Contains(normalized, "s.started_at <= $2::timestamptz")
	require.Len(t, pb.args, 2)
	assert.Equal("2024-05-31T10:00:00Z", pb.args[0])
	assert.Equal("2024-07-01T13:59:59Z", pb.args[1])
}

func TestPGDailyUsageDetailedQuerySelectsMachine(t *testing.T) {
	detailedParams := &paramBuilder{}
	detailed := strings.ToLower(pgDailyUsageRowQuery(
		detailedParams,
		db.UsageFilter{
			From: "2026-07-15", To: "2026-07-15", Breakdowns: true,
		},
		false,
	))
	assert.Contains(t, detailed, "u.machine")

	fastParams := &paramBuilder{}
	fast := strings.ToLower(pgDailyUsageRowQuery(
		fastParams,
		db.UsageFilter{From: "2026-07-15", To: "2026-07-15"},
		false,
	))
	assert.NotContains(t, fast, "u.machine")
}

func TestPGBoundedDailyUsageRowsCTEProjectsReasoningTokens(t *testing.T) {
	pb := &paramBuilder{}
	f := db.UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}
	query := pgDailyUsageRowsSQLForBounds(pb, f, pgUsageBoundsForFilter(pb, f))

	normalized := strings.ToLower(query)
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "ue.cache_read_input_tokens,\n\t\tue.reasoning_tokens,\n\t\tue.cost_microdollars")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
}

// TestPGGetUsageMatchingSessionCountUsesSessionQuery exercises the
// unbounded (no From/To) code path, which counts sessions directly with
// EXISTS subqueries built from the same relaxed matching eligibility
// predicates as the bounded branch. Bounded filters take the
// timestamped-CTE path (see TestPGMatchingUsageRowsSQLForBoundsRelaxesTokenEligibility
// for that SQL shape) and are not exercised by this probe-mock test.
func TestPGGetUsageMatchingSessionCountUsesSessionQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	state := &usageProbeState{}
	store := &Store{
		pg: newUsageProbeDB(t, state),
	}

	count, err := store.GetUsageMatchingSessionCount(t.Context(), db.UsageFilter{
		Agent: "copilot",
		Model: "gpt-5.3-codex",
	})
	require.NoError(err, "GetUsageMatchingSessionCount")
	assert.Equal(1, count)

	state.mu.Lock()
	queries := append([]string(nil), state.queries...)
	state.mu.Unlock()
	require.NotEmpty(queries)

	last := strings.ToLower(queries[len(queries)-1])
	assert.Contains(last, "select count(*)")
	assert.Contains(last, "from sessions s")
	assert.Contains(last, "exists (")
	assert.Contains(last, "from messages m")
	assert.Contains(last, "from usage_events ue")
	assert.Contains(last, "s.agent = ")
	assert.Contains(last, "m.model = ")
	// The message EXISTS uses the relaxed matching eligibility: assistant
	// rows without requiring a model name, so empty-model Copilot
	// assistant messages match the same way they do on the bounded path.
	assert.Contains(last, "m.role = 'assistant'")
	assert.NotContains(last, "m.model != ''")
}

// TestPGMatchingUsageRowsSQLForBoundsRelaxesTokenEligibility asserts the
// bounded matching-session query relaxes token eligibility (no
// m.token_usage check) and model-presence eligibility (m.role = 'assistant'
// instead of m.model != ”, since some Copilot assistant messages parse
// before a model name is known) while filtering Model/ExcludeModel
// directly on the bounded message/event row, matching the normal bounded
// path instead of folding in a session-wide model-match EXISTS clause.
// Mirrors TestPGUsageRowQueryPushesDateBoundsIntoUnion's direct-call style
// with no live DB and no probe mock.
func TestPGMatchingUsageRowsSQLForBoundsRelaxesTokenEligibility(t *testing.T) {
	assert := assert.New(t)

	pb := &paramBuilder{}
	f := db.UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5.3-codex",
	}
	bounds := pgUsageBoundsForFilter(pb, f)
	query := pgMatchingUsageRowsSQLForBounds(pb, f, bounds)

	normalized := strings.ToLower(query)
	assert.Contains(normalized, "message_timestamp_rows as materialized")
	assert.Contains(normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(normalized, "m.role = 'assistant'")
	assert.NotContains(normalized, "m.model != ''")
	assert.NotContains(normalized, "m.token_usage != ''")
	// Model is filtered on the bounded row directly, not via a
	// session-wide EXISTS: each of the four branches (message-timestamp
	// source, event-timestamp source, message fallback, event fallback)
	// applies its own m.model/ue.model comparison, no EXISTS subqueries.
	assert.Equal(0, strings.Count(normalized, "exists ("))
	assert.Equal(2, strings.Count(normalized, "m.model = "))
	assert.Equal(2, strings.Count(normalized, "ue.model = "))
}

func TestPGTopSessionsUsageRowQueryUsesNarrowScan(t *testing.T) {
	assert := assert.New(t)

	pb := &paramBuilder{}
	query := pgTopSessionsUsageRowQuery(pb, db.UsageFilter{
		From:     "2024-06-01",
		To:       "2024-06-30",
		Timezone: "America/New_York",
	})

	normalized := strings.ToLower(query)
	assert.NotContains(normalized, "display_name")
	assert.NotContains(normalized, "first_message")
	assert.NotContains(normalized, "cost_status")
	assert.Contains(normalized, "u.cost_source")
	assert.Contains(normalized, "u.reasoning_tokens")
	assert.NotContains(normalized, "user_message_count")
	assert.NotContains(normalized, "session_activity_at")
	assert.NotContains(normalized, " as started_at")
	assert.NotContains(normalized, "u.machine")
	assert.Contains(normalized, "m.timestamp is not null")
	assert.Contains(normalized, "ue.occurred_at is not null")
	assert.Contains(normalized, "m.timestamp is null")
	assert.Contains(normalized, "ue.occurred_at is null")
	assert.Contains(normalized, "m.timestamp >= $1::timestamptz")
	assert.Contains(normalized, "ue.occurred_at >= $1::timestamptz")
	assert.Contains(normalized,
		"m.timestamp is null\n\tand s.started_at >= $1::timestamptz")
	assert.Contains(normalized,
		"ue.occurred_at is null\n\tand s.started_at >= $1::timestamptz")
	assert.Contains(normalized, "m.timestamp <= $2::timestamptz")
	assert.Contains(normalized, "ue.occurred_at <= $2::timestamptz")
	assert.Contains(normalized, "u.ts >= $3::timestamptz")
	assert.Contains(normalized, "u.ts < $4::timestamptz")
	require.Len(t, pb.args, 4)
	assert.Equal("2024-05-31T10:00:00Z", pb.args[0])
	assert.Equal("2024-07-01T13:59:59Z", pb.args[1])
	assert.Equal(time.Date(2024, 6, 1, 4, 0, 0, 0, time.UTC), pb.args[2])
	assert.Equal(time.Date(2024, 7, 1, 4, 0, 0, 0, time.UTC), pb.args[3])
}

func TestPGSessionRowCostIncludesReasoningOnlyRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver(
		[]export.EffectivePricingRow{{
			ModelPattern: "reasoning-model",
			Rates: export.ModelRates{
				OutputPerMTok: money.MustParseDollars("20"),
				Source:        export.PricingRowSourceFetched,
			},
		}},
	)

	cost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource:     "provider",
		model:           "reasoning-model",
		reasoningTokens: 25,
	}, resolver)

	require.NoError(err)
	assert.True(contributes)
	assert.True(priced)
	assert.Equal(money.MustParseDollars("0.0005"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(err)
	require.Contains(block.Models, "reasoning-model")
	assert.Equal(export.CostSourceComputed,
		block.Models["reasoning-model"].CostSource)
}

func TestPGActivityReportRowStatusCanonicalizesKimiAliasByTimestamp(t *testing.T) {
	tests := []struct {
		name         string
		timestamp    time.Time
		canonical    string
		expectedCost money.Money
	}{
		{
			name:         "before cutoff",
			timestamp:    pricingpkg.KimiModelEraCutoff.Add(-time.Second),
			canonical:    pricingpkg.KimiK26Canonical,
			expectedCost: money.MustParseDollars("1"),
		},
		{
			name:         "at cutoff",
			timestamp:    pricingpkg.KimiModelEraCutoff,
			canonical:    pricingpkg.KimiK3Canonical,
			expectedCost: money.MustParseDollars("2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			resolver := export.NewPricingResolver([]export.EffectivePricingRow{
				{
					ModelPattern: pricingpkg.KimiK26Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("1"),
					},
				},
				{
					ModelPattern: pricingpkg.KimiK3Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("2"),
					},
				},
			})

			cost, priced, contributes, err := pgActivityReportRowStatus(
				pgDailyUsageScanRow{
					usageSource: "provider",
					model:       "daimon-kimi-code",
					ts:          sql.NullTime{Time: tt.timestamp, Valid: true},
					pricingTS:   sql.NullTime{Time: tt.timestamp, Valid: true},
					inputTokens: 1_000_000,
				},
				resolver,
			)

			require.NoError(err)
			assert.True(priced)
			assert.True(contributes)
			assert.Equal(tt.expectedCost, cost)
			block, err := resolver.BuildBlock()
			require.NoError(err)
			require.Contains(block.Models, "daimon-kimi-code")
			resolutions := block.Models["daimon-kimi-code"].Resolutions
			require.Len(resolutions, 1)
			assert.Equal(tt.canonical, resolutions[0].PricedModel)
			assert.NotContains(block.Models, tt.canonical)
		})
	}
}

func TestPGActivityReportRowStatusPrefersExactCustomKimiAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "daimon-kimi-code",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	cost, priced, contributes, err := pgActivityReportRowStatus(
		pgDailyUsageScanRow{
			usageSource: "provider",
			model:       "daimon-kimi-code",
			ts: sql.NullTime{
				Time:  pricingpkg.KimiModelEraCutoff,
				Valid: true,
			},
			pricingTS: sql.NullTime{
				Time:  pricingpkg.KimiModelEraCutoff,
				Valid: true,
			},
			inputTokens: 1_000_000,
		},
		resolver,
	)

	require.NoError(err)
	assert.True(priced)
	assert.True(contributes)
	assert.Equal(money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(err)
	require.Contains(block.Models, "daimon-kimi-code")
	resolutions := block.Models["daimon-kimi-code"].Resolutions
	require.Len(resolutions, 1)
	assert.Equal("daimon-kimi-code", resolutions[0].PricedModel)
}

func TestPGUsageAmountsIncludeMessageReasoningTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver(
		[]export.EffectivePricingRow{{
			ModelPattern: "gpt-5.4",
			Rates: export.ModelRates{
				InputPerMTok:  money.MustParseDollars("1"),
				OutputPerMTok: money.MustParseDollars("2"),
			},
		}},
	)
	row := pgDailyUsageScanRow{
		usageSource: "message",
		model:       "gpt-5.4",
		tokenJSON: `{"input_tokens":1000,"output_tokens":0,` +
			`"reasoning_tokens":500}`,
	}

	inTok, outTok, _, _, cost, _, err := pgDailyUsageAmounts(row, resolver)
	require.NoError(err)
	assert.Equal(1000, inTok)
	assert.Zero(outTok)
	assert.Equal(money.MustParseDollars("0.002"), cost)

	sessionCost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource: "message",
		model:       "gpt-5.4",
		tokenJSON:   row.tokenJSON,
	}, resolver)
	require.NoError(err)
	assert.True(priced)
	assert.True(contributes)
	assert.Equal(money.MustParseDollars("0.002"), sessionCost)
}

func TestPGDailyUsageAmountsPrefersExactCustomKimiAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "kimi-for-coding",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
		usageSource: "provider",
		model:       "kimi-for-coding",
		ts: sql.NullTime{
			Time:  pricingpkg.KimiModelEraCutoff,
			Valid: true,
		},
		inputTokens: 1_000_000,
	}, resolver)

	require.NoError(err)
	assert.Equal(money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(err)
	require.Contains(block.Models, "kimi-for-coding")
	resolutions := block.Models["kimi-for-coding"].Resolutions
	require.Len(resolutions, 1)
	assert.Equal("kimi-for-coding", resolutions[0].PricedModel)
}

func TestPGDailyUsageAmountsPricesGPTReserveAsLuna(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	lunaCost := money.MustParseDollars("0.20")
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: pricingpkg.GPT56LunaCanonical,
		Rates:        export.ModelRates{InputPerMTok: lunaCost},
	}})

	_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
		usageSource: "provider",
		model:       pricingpkg.GPTReserveModelName,
		inputTokens: 1_000_000,
	}, resolver)
	require.NoError(err)
	assert.Equal(lunaCost, cost)
	block, err := resolver.BuildBlock()
	require.NoError(err)
	require.Contains(block.Models, pricingpkg.GPTReserveModelName)
	resolutions := block.Models[pricingpkg.GPTReserveModelName].Resolutions
	require.Len(resolutions, 1)
	assert.Equal(pricingpkg.GPT56LunaCanonical, resolutions[0].PricedModel)
	assert.NotContains(block.Models, pricingpkg.GPT56LunaCanonical)
}

func TestPGDailyUsageAmountsPrefersExactCustomGPTReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: pricingpkg.GPTReserveModelName,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.GPT56LunaCanonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("0.20"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
		usageSource: "provider",
		model:       pricingpkg.GPTReserveModelName,
		inputTokens: 1_000_000,
	}, resolver)
	require.NoError(err)
	assert.Equal(money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(err)
	resolutions := block.Models[pricingpkg.GPTReserveModelName].Resolutions
	require.Len(resolutions, 1)
	assert.Equal(pricingpkg.GPTReserveModelName, resolutions[0].PricedModel)
}

func TestPGDailyUsageAmountsForwardsProviderToBilling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "posit-model",
		Rates:        export.ModelRates{InputPerMTok: money.MustParseDollars("1")},
	}})
	row := func(providerID string) pgDailyUsageScanRow {
		return pgDailyUsageScanRow{
			usageSource: "provider", model: "posit-model", providerID: providerID,
			pricingTS: sql.NullTime{
				Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Valid: true,
			},
			inputTokens: 1_000_000,
		}
	}
	_, _, _, _, positCost, _, err := pgDailyUsageAmounts(row("positai"), resolver)
	require.NoError(err)
	_, _, _, _, plainCost, _, err := pgDailyUsageAmounts(row("claude"), resolver)
	require.NoError(err)
	assert.Equal(money.MustParseDollars("1.1"), positCost)
	assert.Equal(money.MustParseDollars("1"), plainCost)
}

func TestPGDailyUsageAmountsUsesBilledRatesForReportedCacheSavings(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "posit-model",
		Rates: export.ModelRates{
			InputPerMTok:     money.MustParseDollars("1"),
			CacheReadPerMTok: money.MustParseDollars("0.1"),
		},
	}})

	_, _, _, _, cost, savings, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
		usageSource: "provider", model: "posit-model", providerID: "positai",
		inputTokens: 1_000_000, cacheReadInputTokens: 1_000_000,
		cost:       sql.NullInt64{Int64: 77, Valid: true},
		costSource: "provider-reported",
	}, resolver)
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 77}, cost)
	assert.Equal(t, money.Money{Microdollars: 990_000}, savings)
}
