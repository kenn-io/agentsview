//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
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

func TestAnalyticsKeptVersion1ReadOnlyPrimary(t *testing.T) {
	t.Parallel()
	local, syncer, owner := newUsageJournalFixture(t)
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	ctx := t.Context()
	_, err := owner.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='1' WHERE key='usage_change_journal_version'; DELETE FROM sync_metadata WHERE key IN ('usage_account_revision', 'usage_history_token'); REVOKE EXECUTE ON FUNCTION pg_catalog.pg_control_system() FROM PUBLIC`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := owner.pg.ExecContext(context.WithoutCancel(ctx), `GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO PUBLIC`)
		require.NoError(t, err)
	})
	roleURL := newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE`)
	for _, cache := range []bool{false, true} {
		t.Run(map[bool]string{false: "without cache", true: "with cache"}[cache], func(t *testing.T) {
			reader, err := NewStore(ctx, roleURL, syncer.schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			if cache {
				attachUsageCacheProbe(t, reader, t.TempDir())
				reader.usageCache.DeferWork()
				t.Cleanup(func() { require.NoError(t, reader.usageCache.Close()) })
			}
			counter := &analyticsReadCounter{storeExecutor: reader.pg}
			reader.pg = counter
			f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
			tools, err := reader.GetAnalyticsTools(ctx, f)
			require.NoError(t, err)
			velocity, err := reader.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			require.Len(t, velocity.ByAgent, 1)
			reads := counter.count()
			require.Positive(t, reads)
			again, err := reader.GetAnalyticsTools(ctx, f)
			require.NoError(t, err)
			againVelocity, err := reader.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, tools, again)
			assert.Equal(t, velocity, againVelocity)
			assert.Equal(t, reads, counter.count())
			_, err = owner.pg.ExecContext(ctx, `UPDATE sessions SET deleted_at=now() WHERE id='session'`)
			require.NoError(t, err)
			deleted, err := reader.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			assert.Empty(t, deleted.ByAgent)
			_, err = owner.pg.ExecContext(ctx, `UPDATE sessions SET deleted_at=NULL WHERE id='session'`)
			require.NoError(t, err)
		})
	}
}

func TestAnalyticsKeptStandbyReflectsChanges(t *testing.T) {
	t.Parallel()
	standbyURL := testPGStandbyURL(t)
	local, syncer, primary := newUsageJournalFixture(t)
	ctx := t.Context()
	write := func(tool, answer string) {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "test-machine", Agent: "claude", StartedAt: new("2026-06-14T10:00:00Z"), EndedAt: new("2026-06-14T10:01:00Z"), MessageCount: 2}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{
			{SessionID: "session", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-14T10:00:00Z"},
			{SessionID: "session", Ordinal: 1, Role: "assistant", Content: answer, ContentLength: len(answer), Timestamp: "2026-06-14T10:01:00Z", ToolCalls: []db.ToolCall{{ToolName: tool, Category: "Read", ToolUseID: "call"}}},
		}))
		pgKeptPush(t, syncer, false)
	}
	standby, err := sql.Open("pgx", standbyURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, standby.Close()) })
	catchUp := func() {
		var lsn string
		require.NoError(t, primary.pg.QueryRowContext(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn))
		require.Eventually(t, func() bool {
			var caughtUp bool
			return standby.QueryRowContext(ctx, `SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn`, lsn).Scan(&caughtUp) == nil && caughtUp
		}, 20*time.Second, 50*time.Millisecond)
	}
	for _, cache := range []bool{false, true} {
		t.Run(map[bool]string{false: "without cache", true: "with cache"}[cache], func(t *testing.T) {
			write("Read", "answer")
			catchUp()
			reader, err := NewStore(ctx, standbyURL, syncer.schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			if cache {
				attachUsageCacheProbe(t, reader, t.TempDir())
				reader.usageCache.DeferWork()
				t.Cleanup(func() { require.NoError(t, reader.usageCache.Close()) })
			}
			f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
			before, err := reader.GetAnalyticsTools(ctx, f)
			require.NoError(t, err)
			require.Len(t, before.ByTool, 1)
			require.Equal(t, "Read", before.ByTool[0].ToolName)
			velocity, err := reader.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			write("Grep", "a longer answer")
			catchUp()
			after, err := reader.GetAnalyticsTools(ctx, f)
			require.NoError(t, err)
			require.Len(t, after.ByTool, 1)
			require.Equal(t, "Grep", after.ByTool[0].ToolName)
			afterVelocity, err := reader.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			require.NotEqual(t, velocity.Overall.CharsPerActiveMin, afterVelocity.Overall.CharsPerActiveMin)
		})
	}
}

func TestAnalyticsKeptPushAndSelection(t *testing.T) {
	t.Parallel()
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
	signals, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	terms := []db.TrendTermInput{{Term: "answer", Variants: []string{"answer"}, Matchers: []string{"answer"}}}
	trends, err := store.GetTrendsTerms(ctx, f, terms, "week")
	require.NoError(t, err)
	require.Equal(t, 1, trends.Series[0].Total)
	reads := counter.count()
	_, err = store.GetTrendsTerms(ctx, f, terms, "week")
	require.NoError(t, err)
	toolsAgain, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocityAgain, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	signalsAgain, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, signals, signalsAgain)
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
	write("Grep", "a longer reply")
	trends, err = store.GetTrendsTerms(ctx, f, terms, "week")
	require.NoError(t, err)
	require.Zero(t, trends.Series[0].Total)
	changed, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, "Grep", changed.ByTool[0].ToolName)
	changedVelocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.NotEqual(t, velocity.Overall.CharsPerActiveMin, changedVelocity.Overall.CharsPerActiveMin)
	assert.Zero(t, changed.ByTool[0].EmptyCalls)
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{
		QualitySignals:   db.QualitySignals{Version: db.CurrentQualitySignalVersion},
		ToolRetryCount:   2,
		ToolObservations: []db.ToolObservation{{MessageOrdinal: 1, Outcome: "empty", Repeat: "none"}},
	}))
	pgKeptPush(t, syncer, true)
	observed, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, 1, observed.ByTool[0].EmptyCalls)
	updatedSignals, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 2, updatedSignals.ToolHealth.TotalRetries)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestAnalyticsKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	dir := t.TempDir()
	url := testPGURL(t)
	require.NoError(t, store.ConfigureUsageCache(ctx, dir, url, syncer.schema))
	prices := map[string]config.CustomModelRate{"model": {OutputMicrodollarsPerMTok: 100}}
	store.SetCustomPricing(prices)
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
	tools, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	signals, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	reopened, err := NewStore(ctx, url, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.ConfigureUsageCache(ctx, dir, url, syncer.schema))
	reopened.SetCustomPricing(prices)
	counter := &analyticsReadCounter{storeExecutor: reopened.pg}
	reopened.pg = counter
	gotTools, err := reopened.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	gotVelocity, err := reopened.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	gotSignals, err := reopened.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Equal(t, tools, gotTools)
	require.Equal(t, velocity, gotVelocity)
	require.Equal(t, signals, gotSignals)
	require.Zero(t, counter.count())
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{HealthScore: new(90)}))
	pgKeptPush(t, syncer, false)
	scoreStore, err := NewStore(ctx, url, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, scoreStore.Close()) })
	require.NoError(t, scoreStore.ConfigureUsageCache(ctx, dir, url, syncer.schema))
	var counts [3]int
	_, err = keptAnalytics(ctx, scoreStore, "tools", f, func(ctx context.Context, f db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error) {
		counts[0]++
		return scoreStore.loadAnalyticsTools(ctx, f)
	})
	require.NoError(t, err)
	_, err = keptAnalytics(ctx, scoreStore, "velocity", f, func(ctx context.Context, f db.AnalyticsFilter) (db.VelocityResponse, error) {
		counts[1]++
		return scoreStore.loadAnalyticsVelocity(ctx, f)
	})
	require.NoError(t, err)
	score, err := keptAnalytics(ctx, scoreStore, "signals", f, func(ctx context.Context, f db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error) {
		counts[2]++
		return scoreStore.loadAnalyticsSignals(ctx, f)
	})
	require.NoError(t, err)
	require.NotNil(t, score.AvgHealthScore)
	require.InDelta(t, 90, *score.AvgHealthScore, 0)
	require.Equal(t, [3]int{0, 0, 1}, counts)

	t.Run("corrupt", func(t *testing.T) {
		t.Setenv("CACHE_DIRECTORY", t.TempDir())
		local, syncer, store := newUsageJournalFixture(t)
		ctx := t.Context()
		pgKeptSession(t, local, "session", "project", "2026-06-14")
		pgKeptPush(t, syncer, false)
		dir, url := t.TempDir(), testPGURL(t)
		require.NoError(t, store.ConfigureUsageCache(ctx, dir, url, syncer.schema))
		f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
		want, err := store.GetAnalyticsSignals(ctx, f)
		require.NoError(t, err)
		files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-trends", "*", "*.gob"))
		require.NoError(t, err)
		require.Len(t, files, 1)
		data, err := os.ReadFile(files[0])
		require.NoError(t, err)
		header, _, ok := bytes.Cut(data, []byte("\n"))
		require.True(t, ok)
		require.NoError(t, os.WriteFile(files[0], append(header, []byte("\n{broken")...), 0o600))
		reopened, err := NewStore(ctx, url, syncer.schema, true)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reopened.Close()) })
		require.NoError(t, reopened.ConfigureUsageCache(ctx, dir, url, syncer.schema))
		counter := &analyticsReadCounter{storeExecutor: reopened.pg}
		reopened.pg = counter
		got, err := reopened.GetAnalyticsSignals(ctx, f)
		require.NoError(t, err)
		require.Equal(t, want, got)
		reads := counter.count()
		require.Positive(t, reads)
		again, err := reopened.GetAnalyticsSignals(ctx, f)
		require.NoError(t, err)
		require.Equal(t, want, again)
		require.Equal(t, reads, counter.count())
	})
}

func TestAnalyticsKeptScoreOnly(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{HealthScore: new(30)}))
	pgKeptPush(t, syncer, false)
	url := testPGURL(t)
	dir := t.TempDir()
	require.NoError(t, store.ConfigureUsageCache(ctx, dir, url, syncer.schema))
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
	read := func(s *Store, counts *[3]int) db.SignalsAnalyticsResponse {
		_, err := keptAnalytics(ctx, s, "tools", f, func(ctx context.Context, f db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error) {
			counts[0]++
			return s.loadAnalyticsTools(ctx, f)
		})
		require.NoError(t, err)
		_, err = keptAnalytics(ctx, s, "velocity", f, func(ctx context.Context, f db.AnalyticsFilter) (db.VelocityResponse, error) {
			counts[1]++
			return s.loadAnalyticsVelocity(ctx, f)
		})
		require.NoError(t, err)
		signals, err := keptAnalytics(ctx, s, "signals", f, func(ctx context.Context, f db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error) {
			counts[2]++
			return s.loadAnalyticsSignals(ctx, f)
		})
		require.NoError(t, err)
		return signals
	}
	counts := [3]int{}
	first := read(store, &counts)
	require.NotNil(t, first.AvgHealthScore)
	require.InDelta(t, 30, *first.AvgHealthScore, 0)
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{HealthScore: new(60)}))
	pgKeptPush(t, syncer, false)
	counts = [3]int{}
	warm := read(store, &counts)
	require.NotNil(t, warm.AvgHealthScore)
	require.InDelta(t, 60, *warm.AvgHealthScore, 0)
	assert.Equal(t, [3]int{0, 0, 1}, counts)
}

func TestAnalyticsKeptDirectSourceEvents(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	pgKeptPush(t, syncer, false)
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
	f := db.AnalyticsFilter{Timezone: "UTC"}
	var loads [3]int
	read := func() db.ToolsAnalyticsResponse {
		tools, err := keptAnalytics(ctx, store, "tools", f, func(ctx context.Context, f db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error) {
			loads[0]++
			return store.loadAnalyticsTools(ctx, f)
		})
		require.NoError(t, err)
		_, err = keptAnalytics(ctx, store, "velocity", f, func(ctx context.Context, f db.AnalyticsFilter) (db.VelocityResponse, error) {
			loads[1]++
			return store.loadAnalyticsVelocity(ctx, f)
		})
		require.NoError(t, err)
		_, err = keptAnalytics(ctx, store, "signals", f, func(ctx context.Context, f db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error) {
			loads[2]++
			return store.loadAnalyticsSignals(ctx, f)
		})
		require.NoError(t, err)
		return tools
	}
	require.Zero(t, read().TotalCalls)
	loads = [3]int{}
	store.SetCustomPricing(map[string]config.CustomModelRate{"model": {OutputMicrodollarsPerMTok: 100}})
	require.Zero(t, read().TotalCalls)
	require.Equal(t, [3]int{}, loads)
	for _, tc := range []struct {
		query string
		loads [3]int
		calls int
		tool  string
		check func(*testing.T, db.ToolUsageAnalysis)
	}{
		{`UPDATE messages SET content_length=20 WHERE session_id='session' AND ordinal=1`, [3]int{0, 1, 0}, 0, "", nil},
		{`INSERT INTO messages(session_id,ordinal,role,content,timestamp) VALUES('session',2,'assistant','answer','2026-06-14T10:02:00Z')`, [3]int{1, 1, 1}, 0, "", nil},
		{`DELETE FROM messages WHERE session_id='session' AND ordinal=2`, [3]int{1, 1, 1}, 0, "", nil},
		{`INSERT INTO tool_calls(session_id,message_ordinal,tool_name,category) VALUES('session',1,'Read','Read')`, [3]int{1, 1, 0}, 1, "Read", nil},
		{`UPDATE tool_calls SET tool_name='Grep' WHERE session_id='session'`, [3]int{1, 0, 0}, 1, "Grep", nil},
		{`UPDATE tool_calls SET category='Bash' WHERE session_id='session'`, [3]int{1, 0, 0}, 1, "Grep", func(t *testing.T, row db.ToolUsageAnalysis) { require.Equal(t, "Bash", row.Category) }},
		{`UPDATE tool_calls SET observed_outcome='empty' WHERE session_id='session'`, [3]int{1, 0, 0}, 1, "Grep", func(t *testing.T, row db.ToolUsageAnalysis) { require.Equal(t, 1, row.EmptyCalls) }},
		{`UPDATE tool_calls SET observed_repeat='identical' WHERE session_id='session'`, [3]int{1, 0, 0}, 1, "Grep", func(t *testing.T, row db.ToolUsageAnalysis) { require.Equal(t, 1, row.RepeatedCalls) }},
		{`UPDATE tool_calls SET sequence_ending='recovered' WHERE session_id='session'`, [3]int{1, 0, 0}, 1, "Grep", func(t *testing.T, row db.ToolUsageAnalysis) { require.Equal(t, 1, row.RecoveredSequences) }},
		{`UPDATE tool_calls SET tool_name='Grep' WHERE session_id='session'`, [3]int{}, 1, "Grep", nil},
		{`DELETE FROM tool_calls WHERE session_id='session'`, [3]int{1, 1, 0}, 0, "", nil},
	} {
		loads = [3]int{}
		_, err := store.pg.ExecContext(ctx, tc.query)
		require.NoError(t, err)
		tools := read()
		require.Equal(t, tc.loads, loads, tc.query)
		require.Equal(t, tc.calls, tools.TotalCalls, tc.query)
		if tc.tool != "" {
			require.Equal(t, tc.tool, tools.ByTool[0].ToolName)
			if tc.check != nil {
				tc.check(t, tools.ByTool[0])
			}
		}
	}
}

func TestAnalyticsKeptModelTimeScopeChanges(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	for _, tc := range []struct{ name, query string }{
		{"model", `UPDATE messages SET model='model-b' WHERE session_id='session' AND ordinal=1`},
		{"role", `UPDATE messages SET role='assistant' WHERE session_id='session' AND ordinal=0`},
		{"system", `UPDATE messages SET is_system=true WHERE session_id='session' AND ordinal=0`},
		{"timestamp", `UPDATE messages SET timestamp=NULL WHERE session_id='session' AND ordinal=0`},
		{"ordinal", `UPDATE messages SET ordinal=3 WHERE session_id='session' AND ordinal=0`},
		{"move", `UPDATE messages SET session_id='other' WHERE session_id='session' AND ordinal=0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			ctx := t.Context()
			pgKeptSession(t, local, "session", "project", "2026-06-14")
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "other", Machine: "test-machine", Project: "other", Agent: "claude", MessageCount: 1}))
			require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{
				{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2026-06-14T10:00:00Z"},
				{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "answer", Model: "model-a", Timestamp: "2026-06-14T11:00:00Z"},
			}))
			pgKeptPush(t, syncer, false)
			require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
			f := db.AnalyticsFilter{Timezone: "UTC", Model: "model-a", Project: "project", Hour: new(10), DayOfWeek: new(6)}
			before, err := store.GetAnalyticsSignals(ctx, f)
			require.NoError(t, err)
			require.Equal(t, 1, before.UnscoredSessions)
			_, err = store.pg.ExecContext(ctx, tc.query)
			require.NoError(t, err)
			after, err := store.GetAnalyticsSignals(ctx, f)
			require.NoError(t, err)
			require.Zero(t, after.UnscoredSessions)
		})
	}
}

func TestAnalyticsKeptRolledBackSourceEvent(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
	f := db.AnalyticsFilter{Timezone: "UTC"}
	loads := 0
	read := func() db.ToolsAnalyticsResponse {
		result, err := keptAnalytics(ctx, store, "tools", f, func(ctx context.Context, f db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error) {
			loads++
			return store.loadAnalyticsTools(ctx, f)
		})
		require.NoError(t, err)
		return result
	}
	require.Zero(t, read().TotalCalls)
	tx, err := store.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO tool_calls(session_id,message_ordinal,tool_name,category) VALUES('session',1,'Read','Read')`)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.Zero(t, read().TotalCalls)
	require.Equal(t, 1, loads)
}

func TestAnalyticsKeptInvalidPanelToken(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
	for _, token := range []string{"", "invalid"} {
		_, err := store.pg.ExecContext(ctx, `UPDATE sync_metadata SET value=$1 WHERE key='dashboard_signals'`, token)
		require.NoError(t, err)
		got, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Timezone: "UTC"})
		require.NoError(t, err)
		require.Equal(t, 1, got.UnscoredSessions)
	}
	files, err := filepath.Glob(filepath.Join(cache, "postgres-trends", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestAnalyticsKeptJournalUpgrade(t *testing.T) {
	t.Parallel()
	_, _, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `DROP FUNCTION mark_dashboard_sessions() CASCADE; DROP FUNCTION mark_dashboard_messages() CASCADE; DROP FUNCTION mark_dashboard_tool_calls() CASCADE;
 DELETE FROM sync_metadata WHERE key IN ('dashboard_common','dashboard_tools','dashboard_velocity','dashboard_signals','dashboard_scope');
 UPDATE sync_metadata SET value='2' WHERE key='usage_change_journal_version'`)
	require.NoError(t, err)
	var revision, account, history string
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT (SELECT value FROM sync_metadata WHERE key='usage_change_revision'), (SELECT value FROM sync_metadata WHERE key='usage_account_revision'), (SELECT value FROM sync_metadata WHERE key='usage_history_token')`).Scan(&revision, &account, &history))
	tx, err := store.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, installDashboardJournalPG(ctx, tx))
	require.NoError(t, tx.Commit())
	var newRevision, newAccount, newHistory, version string
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT (SELECT value FROM sync_metadata WHERE key='usage_change_revision'), (SELECT value FROM sync_metadata WHERE key='usage_account_revision'), (SELECT value FROM sync_metadata WHERE key='usage_history_token'), (SELECT value FROM sync_metadata WHERE key='usage_change_journal_version')`).Scan(&newRevision, &newAccount, &newHistory, &version))
	require.Equal(t, []string{revision, account, history, "3"}, []string{newRevision, newAccount, newHistory, version})
	tx, err = store.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, installDashboardJournalPG(ctx, tx))
	require.NoError(t, tx.Commit())
}

func TestAnalyticsKeptRestoredHistory(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "first", "2026-06-14")
	pgKeptPush(t, syncer, false)
	dir, url := t.TempDir(), testPGURL(t)
	require.NoError(t, store.ConfigureUsageCache(ctx, dir, url, syncer.schema))
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC", Project: "second"}
	before, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Zero(t, before.UnscoredSessions)
	var revision int64
	var token string
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT (SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'), (SELECT value FROM sync_metadata WHERE key='usage_history_token')`).Scan(&revision, &token))
	_, err = store.pg.ExecContext(ctx, `UPDATE sessions SET project='second' WHERE id='session'`)
	require.NoError(t, err)
	_, err = store.pg.ExecContext(ctx, `UPDATE sync_metadata SET value=$1::bigint::text WHERE key='usage_change_revision'`, revision)
	require.NoError(t, err)
	var restoredRevision int64
	var restoredToken string
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT (SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'), (SELECT value FROM sync_metadata WHERE key='usage_history_token')`).Scan(&restoredRevision, &restoredToken))
	require.Equal(t, revision, restoredRevision)
	require.NotEqual(t, token, restoredToken)
	reopened, err := NewStore(ctx, url, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.ConfigureUsageCache(ctx, dir, url, syncer.schema))
	after, err := reopened.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, after.UnscoredSessions)
}

func TestAnalyticsKeptIdentitylessFallback(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	local, syncer, owner := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	pgKeptPush(t, syncer, false)
	roleURL := newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE`)
	_, err := owner.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='1' WHERE key='usage_change_journal_version'; DELETE FROM sync_metadata WHERE key IN ('usage_account_revision', 'usage_history_token'); REVOKE EXECUTE ON FUNCTION pg_catalog.pg_control_system() FROM PUBLIC`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := owner.pg.ExecContext(context.WithoutCancel(ctx), `GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO PUBLIC`)
		require.NoError(t, err)
	})
	reader, err := NewStore(ctx, roleURL, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.NoError(t, reader.ConfigureUsageCache(ctx, t.TempDir(), roleURL, syncer.schema))
	if reader.usageCache != nil {
		require.NoError(t, reader.usageCache.Close())
		reader.usageCache = nil
	}
	counter := &analyticsReadCounter{storeExecutor: reader.pg}
	reader.pg = counter
	f := db.AnalyticsFilter{From: "2026-06-14", To: "2026-06-14", Timezone: "UTC"}
	first, err := reader.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, first.UnscoredSessions)
	reads := counter.count()
	require.Positive(t, reads)
	again, err := reader.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, reads, counter.count())
	require.True(t, reader.runtimeState().usageIdentityDenied.Load())
	require.NoError(t, filepath.WalkDir(cache, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		require.False(t, strings.HasSuffix(path, ".gob"), path)
		return nil
	}))
}

func TestAnalyticsSignalsMessageRepair(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	pgKeptSession(t, local, "session", "project", "2026-06-14")
	require.NoError(t, local.UpdateSessionSignals(ctx, "session", db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	pgKeptPush(t, syncer, false)
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
	f := db.AnalyticsFilter{Timezone: "UTC"}
	before, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Zero(t, before.QualityHealth.Totals.FrustrationMarkerCount)
	_, err = store.DB().ExecContext(ctx, `UPDATE messages SET content='this is broken' WHERE session_id='session' AND ordinal=0`)
	require.NoError(t, err)
	after, err := store.GetAnalyticsSignals(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, after.QualityHealth.Totals.FrustrationMarkerCount)
}
