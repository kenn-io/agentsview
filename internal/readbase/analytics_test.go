package readbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type analyticsFixtureBackend struct {
	AnalyticsBackend
	sessions []AnalyticsSession
	scope    db.MessageScope
	err      error
}

func (b analyticsFixtureBackend) Sessions(context.Context, db.AnalyticsFilter, bool, bool, string, []any) ([]AnalyticsSession, error) {
	return slices.Clone(b.sessions), nil
}

func (b analyticsFixtureBackend) MessageScope(context.Context, []string, db.AnalyticsFilter, bool) (db.MessageScope, error) {
	return b.scope, b.err
}

func (b analyticsFixtureBackend) VisitModelTimes(ctx context.Context, ids []string, emit func(model, timestamp string)) error {
	emit("selected", "2026-01-05T10:00:00Z")
	return nil
}

func (b analyticsFixtureBackend) HeatmapSQL(db.AnalyticsFilter, string) (string, []any) {
	return "heatmap", nil
}

func (b analyticsFixtureBackend) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, b.err
}

func TestAnalyticsModelSummaryUsesScopedCounts(t *testing.T) {
	backend := analyticsFixtureBackend{
		sessions: []AnalyticsSession{
			{ID: "selected", Project: "project", Agent: "claude", StartedAt: "2026-01-05T10:00:00Z", MessageCount: 20, TotalOutputTokens: 100, HasTotalOutputTokens: true},
			{ID: "outside-hour", MessageCount: 40},
		},
		scope: db.MessageScope{"selected": {
			{Role: "user"},
			{Role: "assistant", OutputTokens: 7, HasOutputTokens: true},
		}},
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
	assert.ErrorIs(t, err, want)
	_, err = analytics.GetAnalyticsHeatmap(t.Context(), db.AnalyticsFilter{}, "messages")
	assert.ErrorIs(t, err, want)
	assert.EqualError(t, err, "querying fixture analytics heatmap: read failed")
}

var errTopSessionsTerminal = errors.New("terminal read failure")

type topSessionsDriver struct{}

func (topSessionsDriver) Open(string) (driver.Conn, error) { return topSessionsDriver{}, nil }
func (topSessionsDriver) Connect(context.Context) (driver.Conn, error) {
	return topSessionsDriver{}, nil
}
func (topSessionsDriver) Driver() driver.Driver { return topSessionsDriver{} }
func (topSessionsDriver) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}

func (topSessionsDriver) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (topSessionsDriver) Close() error              { return nil }
func (topSessionsDriver) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
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

type topSessionsBackend struct {
	AnalyticsBackend
	pool *sql.DB
}

func (b topSessionsBackend) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return b.pool.QueryContext(ctx, query, args...)
}

func (topSessionsBackend) TopSessionsSQL(db.AnalyticsFilter, string, bool) (string, []any) {
	return "SELECT id FROM sessions", nil
}

func (topSessionsBackend) ScanTopSession(rows *sql.Rows) (db.TopSession, error) {
	var row db.TopSession
	err := rows.Scan(&row.ID)
	return row, err
}

func TestAnalyticsTopSessionsReturnsTerminalErrorAfterTenRows(t *testing.T) {
	pool := sql.OpenDB(topSessionsDriver{})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	analytics := NewAnalytics(topSessionsBackend{pool: pool}, "fixture")
	_, err := analytics.GetAnalyticsTopSessions(t.Context(), db.AnalyticsFilter{}, "messages")
	require.ErrorIs(t, err, errTopSessionsTerminal)
	assert.EqualError(t, err, "iterating fixture analytics top sessions: terminal read failure")
}
