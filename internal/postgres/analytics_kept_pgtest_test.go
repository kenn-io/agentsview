//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type analyticsReadCounter struct {
	storeExecutor
	mu    sync.Mutex
	reads int
}

func (c *analyticsReadCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func (c *analyticsReadCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

func TestAnalyticsKeptPushAndSelection(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	write := func(tool, answer string) {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "test-machine", Agent: "claude", StartedAt: new("2026-06-14T10:00:00Z"), EndedAt: new("2026-06-14T10:01:00Z"), MessageCount: 2}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{
			{SessionID: "session", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-14T10:00:00Z"},
			{SessionID: "session", Ordinal: 1, Role: "assistant", Content: answer, ContentLength: len(answer), Timestamp: "2026-06-14T10:01:00Z", Model: "model-a", ToolCalls: []db.ToolCall{{ToolName: tool, Category: "Read", ToolUseID: "call"}}},
		}))
		pgKeptPush(t, syncer, false)
	}
	write("Read", "answer")
	counter := &analyticsReadCounter{storeExecutor: store.pg}
	store.pg = counter
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
	tools, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, tools.TotalCalls)
	require.Equal(t, "Read", tools.ByTool[0].ToolName)
	velocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.Equal(t, float64(60), velocity.Overall.FirstResponseSec.P50)
	reads := counter.count()
	toolsAgain, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocityAgain, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, reads, counter.count())
	freshTools, err := store.loadAnalyticsTools(ctx, f)
	require.NoError(t, err)
	freshVelocity, err := store.loadAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, freshTools, toolsAgain)
	assert.Equal(t, freshVelocity, velocityAgain)
	toolsAgain.ByTool[0].ToolName = "changed"
	toolsAgain.Trend[0].ByCat["Read"] = 99
	velocityAgain.ByAgent[0].Label = "changed"
	toolsAgain, err = store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocityAgain, err = store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, freshTools, toolsAgain)
	assert.Equal(t, freshVelocity, velocityAgain)

	for _, selection := range []struct {
		filter    db.AnalyticsFilter
		wantCalls int
	}{
		{db.AnalyticsFilter{From: f.From, To: f.To, Timezone: "America/New_York"}, 1},
		{db.AnalyticsFilter{From: f.From, To: f.To, Timezone: "UTC", Model: "other"}, 0},
		{db.AnalyticsFilter{From: f.From, To: f.To, Timezone: "UTC", Hour: new(11)}, 0},
	} {
		reads = counter.count()
		selectedTools, err := store.GetAnalyticsTools(ctx, selection.filter)
		require.NoError(t, err)
		assert.Equal(t, selection.wantCalls, selectedTools.TotalCalls)
		assert.Greater(t, counter.count(), reads)
		reads = counter.count()
		selectedVelocity, err := store.GetAnalyticsVelocity(ctx, selection.filter)
		require.NoError(t, err)
		assert.Len(t, selectedVelocity.ByAgent, selection.wantCalls)
		assert.Greater(t, counter.count(), reads)
	}
	write("Grep", "a longer answer")
	changed, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, "Grep", changed.ByTool[0].ToolName)
	changedVelocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.NotEqual(t, velocity.Overall.CharsPerActiveMin, changedVelocity.Overall.CharsPerActiveMin)
	assert.Zero(t, changed.ByTool[0].EmptyCalls)
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{
		QualitySignals:   db.QualitySignals{Version: db.CurrentQualitySignalVersion},
		ToolObservations: []db.ToolObservation{{MessageOrdinal: 1, Outcome: "empty", Repeat: "none"}},
	}))
	pgKeptPush(t, syncer, true)
	observed, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, 1, observed.ByTool[0].EmptyCalls)
	projectFilter := f
	projectFilter.Project = "project"
	projectTools, err := store.GetAnalyticsTools(ctx, projectFilter)
	require.NoError(t, err)
	require.Equal(t, 1, projectTools.TotalCalls)
	projectVelocity, err := store.GetAnalyticsVelocity(ctx, projectFilter)
	require.NoError(t, err)
	require.Len(t, projectVelocity.ByAgent, 1)
	session, err := local.GetSession(ctx, "session")
	require.NoError(t, err)
	session.Project = "other-project"
	require.NoError(t, local.UpsertSession(ctx, *session))
	pgKeptPush(t, syncer, true)
	projectTools, err = store.GetAnalyticsTools(ctx, projectFilter)
	require.NoError(t, err)
	assert.Zero(t, projectTools.TotalCalls)
	projectVelocity, err = store.GetAnalyticsVelocity(ctx, projectFilter)
	require.NoError(t, err)
	assert.Empty(t, projectVelocity.ByAgent)
	require.NoError(t, local.DeleteSession(ctx, "session"))
	pgKeptPush(t, syncer, true)
	deleted, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Zero(t, deleted.TotalCalls)
	deletedVelocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Empty(t, deletedVelocity.ByAgent)
}

func TestAnalyticsKeptAdvancingCutoff(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	ctx := t.Context()
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC", Termination: "active"}
	var cutoff time.Time
	var before db.VelocityResponse
	for window := 5 * time.Second; window <= 80*time.Second; window *= 2 {
		cutoff = time.Now().Add(window)
		_, err := syncer.pg.ExecContext(ctx, `UPDATE sessions SET ended_at = $1 WHERE id = 'session'`, cutoff.Add(-10*time.Minute))
		require.NoError(t, err)
		before, err = store.GetAnalyticsVelocity(ctx, f)
		require.NoError(t, err)
		if len(before.ByAgent) == 1 {
			break
		}
	}
	require.Len(t, before.ByAgent, 1)
	timer := time.NewTimer(time.Until(cutoff) + time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Empty(t, after.ByAgent)
}

func TestAnalyticsKeptLocalTimezoneChange(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "test-machine", Agent: "claude", StartedAt: new("2026-06-15T00:30:00Z"), EndedAt: new("2026-06-15T00:31:00Z"), MessageCount: 2}))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{
		{SessionID: "session", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-15T00:30:00Z"},
		{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: "2026-06-15T00:31:00Z", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "Read", ToolUseID: "call"}}},
	}))
	pgKeptPush(t, syncer, false)
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-15", Timezone: "Local"}
	t.Setenv("TZ", "UTC")
	first, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, first.Trend, 1)
	assert.Equal(t, "2026-06-15", first.Trend[0].Date)
	t.Setenv("TZ", "America/New_York")
	want, err := store.loadAnalyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, want.Trend, 1)
	require.Equal(t, "2026-06-08", want.Trend[0].Date)
	second, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, want, second)
}

func TestAnalyticsKeptActiveSincePreservesStableSelections(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	ctx := t.Context()
	counter := &analyticsReadCounter{storeExecutor: store.pg}
	store.pg = counter
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
	stableTools, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	stableVelocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	filtered := f
	cutoff := time.Date(2026, 6, 14, 10, 1, 1, 0, time.UTC)
	for i := range 65 {
		filtered.ActiveSince = cutoff.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		_, err = store.GetAnalyticsTools(ctx, filtered)
		require.NoError(t, err)
		_, err = store.GetAnalyticsVelocity(ctx, filtered)
		require.NoError(t, err)
	}
	reads := counter.count()
	tools, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, stableTools, tools)
	assert.Equal(t, stableVelocity, velocity)
	assert.Equal(t, reads, counter.count())
}

func TestAnalyticsKeptBypasses(t *testing.T) {
	for _, mode := range []string{"active", "hosted", "borrowed", "no journal"} {
		t.Run(mode, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			pgKeptSession(t, local, "session", "project", "2026-06-14")
			pgKeptPush(t, syncer, false)
			f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
			switch mode {
			case "active":
				f.Termination = "active"
			case "hosted":
				store.hostedRelations = true
			case "borrowed":
				tx, err := store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				defer tx.Rollback()
				store = store.readView(tx)
			case "no journal":
				_, err := syncer.pg.ExecContext(t.Context(), dropUsageChangeJournalSQL)
				require.NoError(t, err)
			}
			counter := &analyticsReadCounter{storeExecutor: store.pg}
			store.pg = counter
			read := func() {
				_, err := store.GetAnalyticsTools(t.Context(), f)
				require.NoError(t, err)
				_, err = store.GetAnalyticsVelocity(t.Context(), f)
				require.NoError(t, err)
			}
			read()
			first := counter.count()
			require.Positive(t, first)
			read()
			assert.Equal(t, 2*first, counter.count())
		})
	}
}

func TestAnalyticsKeptCommitOrder(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "first", "project", "2026-06-14")
	pgKeptSession(t, local, "second", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	ctx := t.Context()
	first, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer first.Rollback()
	second, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer second.Rollback()
	_, err = first.ExecContext(ctx, `UPDATE sessions SET project = 'later' WHERE id = 'first'`)
	require.NoError(t, err)
	_, err = second.ExecContext(ctx, `UPDATE sessions SET project = 'earlier' WHERE id = 'second'`)
	require.NoError(t, err)
	require.NoError(t, second.Commit())
	f := db.AnalyticsFilter{Project: "later", From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
	before, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Empty(t, before.ByAgent)
	require.NoError(t, first.Commit())
	after, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.Len(t, after.ByAgent, 1)
	assert.Equal(t, 1, after.ByAgent[0].Sessions)
}
