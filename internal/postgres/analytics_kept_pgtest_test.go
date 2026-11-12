//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

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
	if !strings.Contains(query, "usage_change_revision") {
		c.mu.Lock()
		c.reads++
		c.mu.Unlock()
	}
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
	assert.Equal(t, tools, toolsAgain)
	assert.Equal(t, velocity, velocityAgain)
	assert.Equal(t, reads, counter.count())
	toolsAgain.ByTool[0].ToolName = "changed"
	velocityAgain.ByAgent[0].Label = "changed"
	toolsAgain, err = store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocityAgain, err = store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, tools, toolsAgain)
	assert.Equal(t, velocity, velocityAgain)

	for _, selection := range []db.AnalyticsFilter{
		{From: f.From, To: f.To, Timezone: "America/New_York"},
		{From: f.From, To: f.To, Timezone: "UTC", Model: "other"},
		{From: f.From, To: f.To, Timezone: "UTC", Hour: new(11)},
	} {
		_, err = store.GetAnalyticsTools(ctx, selection)
		require.NoError(t, err)
		_, err = store.GetAnalyticsVelocity(ctx, selection)
		require.NoError(t, err)
	}
	assert.Greater(t, counter.count(), reads)
	write("Grep", "a longer answer")
	changed, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, "Grep", changed.ByTool[0].ToolName)
	changedVelocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.NotEqual(t, velocity.Overall.CharsPerActiveMin, changedVelocity.Overall.CharsPerActiveMin)
	require.NoError(t, local.DeleteSession(ctx, "session"))
	pgKeptPush(t, syncer, true)
	deleted, err := store.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	assert.Zero(t, deleted.TotalCalls)
	deletedVelocity, err := store.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Empty(t, deletedVelocity.ByAgent)
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
