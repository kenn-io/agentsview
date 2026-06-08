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
				"cost_usd",
				"claude_message_id",
				"claude_request_id",
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
		nil,
		"msg-dup",
		"req-dup",
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
	assert.Equal(t, 1, result.SessionCounts.ByProject["proj-a"])
	assert.Equal(t, 1, result.SessionCounts.ByAgent["claude"])
	assert.Zero(t, result.SessionCounts.ByProject["proj-b"])
	assert.Zero(t, result.SessionCounts.ByAgent["codex"])
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
	assert.NotContains(t, normalized, "reasoning_tokens")
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
	assert.NotContains(t, normalized, "reasoning_tokens")
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
