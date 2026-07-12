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
)

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
	if strings.Contains(normalized, "from model_pricing") {
		return &usageProbeRows{
			columns: []string{
				"model_pattern",
				"input_per_mtok",
				"output_per_mtok",
				"cache_creation_per_mtok",
				"cache_read_per_mtok",
				"updated_at",
			},
			values: [][]driver.Value{{
				"claude-sonnet", 3.0, 15.0, 3.75, 0.3, "2026-06-08",
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
				"model",
				"token_usage",
				"input_tokens",
				"output_tokens",
				"cache_creation_input_tokens",
				"cache_read_input_tokens",
				"reasoning_tokens",
				"cost_usd",
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
		"claude-sonnet",
		`{"input_tokens":100,"output_tokens":50}`,
		int64(0),
		int64(0),
		int64(0),
		int64(0),
		int64(0),
		nil,
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
	store := &Store{
		pg: newUsageProbeDB(t, &usageProbeState{}),
	}

	result, err := store.GetDailyUsage(context.Background(), db.UsageFilter{
		From: "2024-06-15",
		To:   "2024-06-15",
	})
	require.NoError(t, err, "GetDailyUsage")

	assert.Equal(t, 1, result.SessionCounts.Total)
	countsByDisplay := make(map[string]int, len(result.Projects))
	for key, project := range result.Projects {
		countsByDisplay[project.DisplayLabel] = result.SessionCounts.ByProject[key]
		assert.NotContains(t, key, project.DisplayLabel)
	}
	assert.Equal(t, map[string]int{"proj-a": 1}, countsByDisplay)
	assert.Equal(t, 1, result.SessionCounts.ByAgent["claude"])
	assert.NotContains(t, countsByDisplay, "proj-b")
	assert.Zero(t, result.SessionCounts.ByAgent["codex"])
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
	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "gpt-5.4",
		Rates: export.ModelRates{
			InputPerMTok: 1.0, OutputPerMTok: 2.0,
		},
	}})

	inTok, outTok, _, _, cost, _ := pgDailyUsageAmounts(
		pgDailyUsageScanRow{
			usageSource:  "session",
			model:        "gpt-5.4",
			inputTokens:  rawInput,
			outputTokens: rawOutput,
		},
		resolver,
	)
	assert.Equal(t, rawInput, inTok, "daily input")
	assert.Equal(t, rawOutput, outTok, "daily output")
	wantCost := (float64(rawInput)*1.0 + float64(rawOutput)*2.0) / 1_000_000
	assert.InDelta(t, wantCost, cost, 1e-9, "daily cost")

	cost, priced, contributes := pgSessionRowCost(pgUsageScanRow{
		usageSource:  "session",
		model:        "gpt-5.4",
		inputTokens:  rawInput,
		outputTokens: rawOutput,
	}, resolver)
	require.True(t, priced, "priced")
	require.True(t, contributes, "contributes")
	assert.InDelta(t, wantCost, cost, 1e-9, "session cost")
}

func TestPGUsageRowQueryPushesDateBoundsIntoUnion(t *testing.T) {
	pb := &paramBuilder{}
	query := pgUsageRowQuery(pb, db.UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		ExcludeAutomated: true,
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "and u.ts >=")
	assert.NotContains(t, normalized, "and u.ts <=")
	assert.NotContains(t, normalized, " or ")
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.NotContains(t, normalized, "cost_source")
	assert.Contains(t, normalized, "u.reasoning_tokens")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "from message_timestamp_rows m\njoin sessions s")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "m.timestamp is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= $1::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at >= $1::timestamptz")
	assert.Contains(t, normalized, "s.started_at >= $1::timestamptz")
	assert.Contains(t, normalized, "m.timestamp <= $2::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at <= $2::timestamptz")
	assert.Contains(t, normalized, "s.started_at <= $2::timestamptz")
	require.Len(t, pb.args, 2)
	assert.Equal(t, "2024-05-31T10:00:00Z", pb.args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", pb.args[1])
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
	assert.Contains(t, normalized, "ue.cache_read_input_tokens,\n\t\tue.reasoning_tokens,\n\t\tue.cost_usd")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
}

// TestPGGetUsageMatchingSessionCountUsesSessionQuery exercises the
// unbounded (no From/To) code path, which counts sessions directly with
// EXISTS subqueries built from the same relaxed matching eligibility
// predicates as the bounded branch. Bounded filters take the
// timestamped-CTE path (see TestPGMatchingUsageRowsSQLForBoundsRelaxesTokenEligibility
// for that SQL shape) and are not exercised by this probe-mock test.
func TestPGGetUsageMatchingSessionCountUsesSessionQuery(t *testing.T) {
	state := &usageProbeState{}
	store := &Store{
		pg: newUsageProbeDB(t, state),
	}

	count, err := store.GetUsageMatchingSessionCount(context.Background(), db.UsageFilter{
		Agent: "copilot",
		Model: "gpt-5.3-codex",
	})
	require.NoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, count)

	state.mu.Lock()
	queries := append([]string(nil), state.queries...)
	state.mu.Unlock()
	require.NotEmpty(t, queries)

	last := strings.ToLower(queries[len(queries)-1])
	assert.Contains(t, last, "select count(*)")
	assert.Contains(t, last, "from sessions s")
	assert.Contains(t, last, "exists (")
	assert.Contains(t, last, "from messages m")
	assert.Contains(t, last, "from usage_events ue")
	assert.Contains(t, last, "s.agent = ")
	assert.Contains(t, last, "m.model = ")
	// The message EXISTS uses the relaxed matching eligibility: assistant
	// rows without requiring a model name, so empty-model Copilot
	// assistant messages match the same way they do on the bounded path.
	assert.Contains(t, last, "m.role = 'assistant'")
	assert.NotContains(t, last, "m.model != ''")
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
	pb := &paramBuilder{}
	f := db.UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5.3-codex",
	}
	bounds := pgUsageBoundsForFilter(pb, f)
	query := pgMatchingUsageRowsSQLForBounds(pb, f, bounds)

	normalized := strings.ToLower(query)
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "m.role = 'assistant'")
	assert.NotContains(t, normalized, "m.model != ''")
	assert.NotContains(t, normalized, "m.token_usage != ''")
	// Model is filtered on the bounded row directly, not via a
	// session-wide EXISTS: each of the four branches (message-timestamp
	// source, event-timestamp source, message fallback, event fallback)
	// applies its own m.model/ue.model comparison, no EXISTS subqueries.
	assert.Equal(t, 0, strings.Count(normalized, "exists ("))
	assert.Equal(t, 2, strings.Count(normalized, "m.model = "))
	assert.Equal(t, 2, strings.Count(normalized, "ue.model = "))
}

func TestPGTopSessionsUsageRowQueryUsesNarrowScan(t *testing.T) {
	pb := &paramBuilder{}
	query := pgTopSessionsUsageRowQuery(pb, db.UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.NotContains(t, normalized, "cost_source")
	assert.Contains(t, normalized, "u.reasoning_tokens")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "m.timestamp is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= $1::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at >= $1::timestamptz")
	assert.Contains(t, normalized,
		"m.timestamp is null\n\tand s.started_at >= $1::timestamptz")
	assert.Contains(t, normalized,
		"ue.occurred_at is null\n\tand s.started_at >= $1::timestamptz")
	assert.Contains(t, normalized, "m.timestamp <= $2::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at <= $2::timestamptz")
	require.Len(t, pb.args, 2)
	assert.Equal(t, "2024-05-31T10:00:00Z", pb.args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", pb.args[1])
}

func TestPGSessionRowCostIncludesReasoningOnlyRows(t *testing.T) {
	resolver := export.NewPricingResolver(
		[]export.EffectivePricingRow{{
			ModelPattern: "reasoning-model",
			Rates: export.ModelRates{
				OutputPerMTok: 20,
				Source:        export.PricingRowSourceFetched,
			},
		}},
	)

	cost, priced, contributes := pgSessionRowCost(pgUsageScanRow{
		usageSource:     "provider",
		model:           "reasoning-model",
		reasoningTokens: 25,
	}, resolver)

	assert.True(t, contributes)
	assert.True(t, priced)
	assert.InDelta(t, 0.0005, cost, 0.0000001)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "reasoning-model")
	assert.Equal(t, export.CostSourceComputed,
		block.Models["reasoning-model"].CostSource)
}

func TestPGUsageAmountsIncludeMessageReasoningTokens(t *testing.T) {
	resolver := export.NewPricingResolver(
		[]export.EffectivePricingRow{{
			ModelPattern: "gpt-5.4",
			Rates: export.ModelRates{
				InputPerMTok:  1,
				OutputPerMTok: 2,
			},
		}},
	)
	row := pgDailyUsageScanRow{
		usageSource: "message",
		model:       "gpt-5.4",
		tokenJSON: `{"input_tokens":1000,"output_tokens":0,` +
			`"reasoning_tokens":500}`,
	}

	inTok, outTok, _, _, cost, _ := pgDailyUsageAmounts(row, resolver)
	assert.Equal(t, 1000, inTok)
	assert.Zero(t, outTok)
	assert.InDelta(t, 0.002, cost, 1e-12)

	sessionCost, priced, contributes := pgSessionRowCost(pgUsageScanRow{
		usageSource: "message",
		model:       "gpt-5.4",
		tokenJSON:   row.tokenJSON,
	}, resolver)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.InDelta(t, 0.002, sessionCost, 1e-12)
}
