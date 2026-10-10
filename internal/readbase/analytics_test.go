package readbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type analyticsFixtureBackend struct {
	AnalyticsBackend
	sessions []AnalyticsSession
	err      error
	pool     *sql.DB
}

func (b analyticsFixtureBackend) Sessions(context.Context, db.AnalyticsFilter, bool, bool, string, []any) ([]AnalyticsSession, error) {
	return slices.Clone(b.sessions), nil
}

func (b analyticsFixtureBackend) FormatTime(value any) string { return value.(string) }

func (b analyticsFixtureBackend) HeatmapSQL(db.AnalyticsFilter, string) (string, []any) {
	return "heatmap", nil
}

func (analyticsFixtureBackend) SummarySQL(db.AnalyticsFilter) (string, []any) {
	return "summary", nil
}

func (b analyticsFixtureBackend) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.pool.QueryContext(ctx, query, args...)
}

func TestAnalyticsModelSummaryUsesScopedCounts(t *testing.T) {
	pool := sql.OpenDB(analyticsFixtureDriver{})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	backend := analyticsFixtureBackend{
		pool: pool,
		sessions: []AnalyticsSession{
			{ID: "selected", Project: "project", Agent: "claude", StartedAt: "2026-01-05T10:00:00Z", MessageCount: 20, TotalOutputTokens: 100, HasTotalOutputTokens: true},
			{ID: "outside-hour", MessageCount: 40},
		},
	}
	analytics := NewAnalytics(backend, "fixture")
	hour := 10
	filter := db.AnalyticsFilter{Model: "selected", Hour: &hour}
	for range 2 {
		result, err := analytics.GetAnalyticsSummary(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, 1, result.TotalSessions)
		assert.Equal(t, 2, result.TotalMessages)
		assert.Equal(t, 7, result.TotalOutputTokens)
		assert.Equal(t, 2, result.MedianMessages)
		assert.Equal(t, 1, result.TokenReportingSessions)
		assert.Equal(t, []string{"selected"}, result.Models)
		assert.Equal(t, &db.AgentSummary{Sessions: 1, Messages: 2}, result.Agents["claude"])
	}
	assert.Equal(t, 20, backend.sessions[0].MessageCount)
	assert.Equal(t, 100, backend.sessions[0].TotalOutputTokens)
}

func TestAnalyticsPropagatesBackendErrors(t *testing.T) {
	want := errors.New("read failed")
	backend := analyticsFixtureBackend{sessions: []AnalyticsSession{{ID: "session"}}, err: want}
	analytics := NewAnalytics(backend, "fixture")
	_, err := analytics.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Model: "selected"})
	require.ErrorIs(t, err, want)
	_, err = analytics.GetAnalyticsHeatmap(t.Context(), db.AnalyticsFilter{}, "messages")
	assert.EqualError(t, err, "querying fixture analytics heatmap: read failed")
}

var errTopSessionsTerminal = errors.New("terminal read failure")
var errSummaryRead = errors.New("summary read failure")

func TestAnalyticsSummaryReturnsFirstReadError(t *testing.T) {
	pool := sql.OpenDB(analyticsFixtureDriver{})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	analytics := NewAnalytics(analyticsFixtureBackend{pool: pool}, "fixture")
	_, err := analytics.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{})
	require.ErrorIs(t, err, errSummaryRead)
	assert.EqualError(t, err, "iterating fixture analytics summary: summary read failure")
	t.Run("empty summary", func(t *testing.T) {
		pool := sql.OpenDB(analyticsFixtureDriver{emptySummary: true})
		t.Cleanup(func() { require.NoError(t, pool.Close()) })
		analytics := NewAnalytics(analyticsFixtureBackend{pool: pool}, "fixture")
		result, err := analytics.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{})
		require.NoError(t, err)
		assert.Equal(t, db.AnalyticsSummary{}, result)
	})
}

type summaryErrorRows struct{ empty bool }

func (summaryErrorRows) Columns() []string { return []string{"total_sessions"} }
func (summaryErrorRows) Close() error      { return nil }
func (r summaryErrorRows) Next([]driver.Value) error {
	if r.empty {
		return io.EOF
	}
	return errSummaryRead
}

type analyticsFixtureDriver struct{ emptySummary bool }

func (analyticsFixtureDriver) Open(string) (driver.Conn, error) { return analyticsFixtureDriver{}, nil }

func (d analyticsFixtureDriver) Connect(context.Context) (driver.Conn, error) {
	return d, nil
}
func (analyticsFixtureDriver) Driver() driver.Driver { return analyticsFixtureDriver{} }
func (analyticsFixtureDriver) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}

func (analyticsFixtureDriver) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (analyticsFixtureDriver) Close() error { return nil }
func (d analyticsFixtureDriver) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query == "summary" {
		return summaryErrorRows{empty: d.emptySummary}, nil
	}
	if strings.Contains(query, "SELECT model, timestamp") {
		return &modelTimesRows{}, nil
	}
	if strings.Contains(query, "has_thinking, has_tool_use, timestamp,") {
		return &candidateMessageRows{}, nil
	}
	return &topSessionsRows{}, nil
}

type topSessionsRows struct{ count int }

func (*topSessionsRows) Columns() []string { return []string{"id"} }
func (*topSessionsRows) Close() error      { return nil }
func (r *topSessionsRows) Next(values []driver.Value) error {
	if r.count == 10 {
		return errTopSessionsTerminal
	}
	r.count++
	values[0] = "session"
	return nil
}

func (analyticsFixtureBackend) TopSessionsSQL(db.AnalyticsFilter, string, bool) (string, []any) {
	return "SELECT id FROM sessions", nil
}

func (analyticsFixtureBackend) ScanTopSession(rows *sql.Rows) (db.TopSession, error) {
	var row db.TopSession
	err := rows.Scan(&row.ID)
	return row, err
}

func TestAnalyticsTopSessionsReturnsTerminalErrorAfterTenRows(t *testing.T) {
	pool := sql.OpenDB(analyticsFixtureDriver{})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	analytics := NewAnalytics(analyticsFixtureBackend{pool: pool}, "fixture")
	_, err := analytics.GetAnalyticsTopSessions(t.Context(), db.AnalyticsFilter{}, "messages")
	require.ErrorIs(t, err, errTopSessionsTerminal)
	assert.EqualError(t, err, "iterating fixture analytics top sessions: terminal read failure")
}

type modelTimesRows struct{ done bool }

func (*modelTimesRows) Columns() []string { return []string{"model", "timestamp"} }
func (*modelTimesRows) Close() error      { return nil }
func (r *modelTimesRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	values[0], values[1] = "selected", "2026-01-05T10:00:00Z"
	return nil
}

type candidateMessageRows struct{ index int }

func (*candidateMessageRows) Columns() []string {
	return []string{"session_id", "ordinal", "role", "source_subtype", "is_system", "model", "has_thinking", "has_tool_use", "timestamp", "output_tokens", "has_output_tokens", "content_length", "content"}
}
func (*candidateMessageRows) Close() error { return nil }
func (r *candidateMessageRows) Next(values []driver.Value) error {
	rows := [][]driver.Value{
		{"selected", int64(0), "user", "", false, "", false, false, "2026-01-05T10:00:00Z", int64(0), false, int64(0), ""},
		{"selected", int64(1), "assistant", "", false, "selected", false, false, "2026-01-05T10:00:00Z", int64(7), true, int64(0), ""},
		{"outside-hour", int64(0), "assistant", "", false, "selected", false, false, "2026-01-05T11:00:00Z", int64(90), true, int64(0), ""},
	}
	if r.index == len(rows) {
		return io.EOF
	}
	copy(values, rows[r.index])
	r.index++
	return nil
}
