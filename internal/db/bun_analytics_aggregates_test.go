package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type sqliteAnalyticsAggregateBackend struct {
	store bun.IDB
}

func (*sqliteAnalyticsAggregateBackend) Name() string   { return "sqlite-analytics-aggregate" }
func (*sqliteAnalyticsAggregateBackend) ReadOnly() bool { return true }
func (*sqliteAnalyticsAggregateBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{AnalyticsDialect: SQLiteBunAnalyticsDialect()}
}
func (*sqliteAnalyticsAggregateBackend) TimestampOrderExpr(column string) string {
	return sqliteTimestampOrderExpr(column)
}
func (*sqliteAnalyticsAggregateBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}
func (b *sqliteAnalyticsAggregateBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.store)
}
func (b *sqliteAnalyticsAggregateBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.store)
}
func (*sqliteAnalyticsAggregateBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

func TestBunAnalyticsToolAndSkillPanelsUseOneAggregateQuery(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T12:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "aggregate-tools", Project: "aggregate", Machine: "host", Agent: "codex",
		CreatedAt: started, StartedAt: &started, MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "aggregate-tools", Ordinal: 0, Role: "assistant",
		Content: "done", ContentLength: 4, Timestamp: started, Model: "gpt-5",
		HasToolUse: true, ToolCalls: []ToolCall{
			{ToolName: "Read", Category: "Read"},
			{ToolName: "Skill", Category: "Skill", SkillName: "review"},
		},
	}}))

	filter := AnalyticsFilter{
		Project: "aggregate", Model: "gpt-5", From: "2026-08-04",
		To: "2026-08-04", Timezone: "UTC",
	}
	for _, test := range []struct {
		name string
		run  func(*BunStore) error
	}{
		{name: "tools", run: func(store *BunStore) error {
			got, err := store.GetAnalyticsTools(t.Context(), filter)
			assert.Equal(t, 2, got.TotalCalls)
			return err
		}},
		{name: "skills", run: func(store *BunStore) error {
			got, err := store.GetAnalyticsSkills(t.Context(), filter, "week")
			assert.Equal(t, 1, got.TotalSkillCalls)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			hook := new(countingQueryHook)
			store := NewBunStore(&sqliteAnalyticsAggregateBackend{
				store: database.bunReader.WithQueryHook(hook),
			})
			require.NoError(t, test.run(store))
			assert.Equal(t, 1, hook.selects,
				"the database should return compact aggregates without hydrating sessions, messages, and calls")
		})
	}
}

func TestBunAnalyticsVelocityUsesOneAggregateQuery(t *testing.T) {
	database := testDB(t)
	insertConversation(t, database, "aggregate-velocity", "aggregate", "codex",
		"2024-06-01T09:00:00Z", []time.Duration{0, 10 * time.Second})

	hook := new(countingQueryHook)
	store := NewBunStore(&sqliteAnalyticsAggregateBackend{
		store: database.bunReader.WithQueryHook(hook),
	})
	got, err := store.GetAnalyticsVelocity(t.Context(), AnalyticsFilter{
		Project: "aggregate", From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 10.0, got.Overall.TurnCycleSec.P50)
	assert.Equal(t, 1, hook.selects,
		"the database should reduce message timing and tool facts before returning rows")
}

func TestBunAnalyticsVelocityTimeFilterQualifiesWholeSession(t *testing.T) {
	database := testDB(t)
	insertConversation(t, database, "velocity-hour", "aggregate", "codex",
		"2024-06-01T09:00:00Z", []time.Duration{0, 10 * time.Second})

	hour := 9
	store := NewBunStore(&sqliteAnalyticsAggregateBackend{store: database.bunReader})
	got, err := store.GetAnalyticsVelocity(t.Context(), AnalyticsFilter{
		Project: "aggregate", Hour: &hour, Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 10.0, got.Overall.TurnCycleSec.P50)
}

func TestBunAnalyticsVelocityTimeFilterKeepsWholeSessionToolCounts(t *testing.T) {
	database := testDB(t)
	started := "2024-06-01T09:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "velocity-tools", Project: "aggregate", Machine: "host", Agent: "codex",
		CreatedAt: started, StartedAt: &started,
		MessageCount: 2, UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "velocity-tools", Ordinal: 0, Role: "user",
			Content: "start", ContentLength: 5, Timestamp: started},
		{SessionID: "velocity-tools", Ordinal: 1, Role: "assistant",
			Content: "done", ContentLength: 4, Timestamp: "2024-06-01T10:00:00Z",
			HasToolUse: true, ToolCalls: []ToolCall{{
				ToolUseID: "call-1", ToolName: "Read", Category: "Read",
			}}},
	}))

	hour := 9
	store := NewBunStore(&sqliteAnalyticsAggregateBackend{store: database.bunReader})
	got, err := store.GetAnalyticsVelocity(t.Context(), AnalyticsFilter{
		Project: "aggregate", Hour: &hour, Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 0.2, got.Overall.ToolCallsPerActiveMin)
}

func TestBunAnalyticsSignalsExcludeUncomputedFrustration(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T12:00:00Z"
	for _, id := range []string{"computed", "uncomputed"} {
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "signals", Machine: "host", Agent: "codex",
			CreatedAt: started, StartedAt: &started,
			MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "user",
			Content: "this is broken again", ContentLength: 20, Timestamp: started,
		}}))
	}
	_, err := database.getWriter().Exec(
		"UPDATE sessions SET quality_signal_version = ? WHERE id = ?",
		CurrentQualitySignalVersion, "computed",
	)
	require.NoError(t, err)

	store := NewBunStore(&sqliteAnalyticsAggregateBackend{store: database.bunReader})
	got, err := store.GetAnalyticsSignals(t.Context(), AnalyticsFilter{
		Project: "signals", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.QualityHealth.Totals.FrustrationMarkerCount)
	assert.Equal(t, 1,
		got.QualityHealth.SessionsWithSignal.FrustrationMarkerCount)
}
