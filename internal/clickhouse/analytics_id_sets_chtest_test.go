//go:build chtest

package clickhouse

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/storage"
)

// seedIDSetMirror writes sessions to a SQLite archive and pushes them to a
// fresh ClickHouse database. Session i starts on January (i%28)+1, 2026, at
// 11:00 when i%8 is 0 and at 10:00 otherwise. Its messages alternate user
// and assistant. Assistant message 1 uses model "shared" and calls a tool
// when i is even; assistant message 3 uses model "solo-<i%soloModels>" and
// calls a tool when i%3 is 0. Later assistant messages repeat that pattern.
func seedIDSetMirror(tb testing.TB, sessions, messagesPerSession, soloModels int) (*Store, *db.DB) {
	tb.Helper()
	ctx := tb.Context()
	local, err := db.Open(ctx, filepath.Join(tb.TempDir(), "local.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, local.Close()) })

	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	const batch = 500
	for start := 0; start < sessions; start += batch {
		writes := make([]db.SessionBatchWrite, 0, batch)
		for i := start; i < min(start+batch, sessions); i++ {
			id := fmt.Sprintf("id-set-%05d", i)
			began := base.AddDate(0, 0, i%28)
			if i%8 == 0 {
				began = began.Add(time.Hour)
			}
			msgs := make([]db.Message, 0, messagesPerSession)
			for ord := range messagesPerSession {
				ts := began.Add(time.Duration(ord) * time.Second).Format("2006-01-02T15:04:05.000Z")
				if ord%2 == 0 {
					msg := fixtureMessage(id, ord, "user", fmt.Sprintf("prompt %d", ord), ts)
					msg.Model = ""
					msgs = append(msgs, msg)
					continue
				}
				model, calls := "shared", i%2 == 0
				if ord%4 == 3 {
					model, calls = fmt.Sprintf("solo-%05d", i%soloModels), i%3 == 0
				}
				var toolCalls []db.ToolCall
				if calls {
					toolCalls = append(toolCalls, db.ToolCall{
						ToolName:  "Read",
						Category:  "Read",
						ToolUseID: fmt.Sprintf("%s-tool-%d", id, ord),
						InputJSON: `{"path":"main.go"}`,
					})
				}
				msg := fixtureMessage(id, ord, "assistant", fmt.Sprintf("reply %d", ord), ts, toolCalls...)
				msg.Model = model
				msgs = append(msgs, msg)
			}
			session := fixtureSession(id, fmt.Sprintf("project-%d", i%5), "prompt 0",
				began.Format("2006-01-02T15:04:05.000Z"), len(msgs))
			ended := began.Add(time.Duration(len(msgs)) * time.Second).Format("2006-01-02T15:04:05.000Z")
			session.EndedAt = &ended
			writes = append(writes, db.SessionBatchWrite{
				Session:         session,
				Messages:        msgs,
				DataVersion:     1,
				ReplaceMessages: true,
			})
		}
		_, err := local.WriteSessionBatchAtomic(ctx, writes)
		require.NoError(tb, err)
	}

	dsn, database := chtest.FreshDatabase(tb)
	target := Target{URL: dsn, Database: database}
	syncer, err := New(ctx, target, local, fixtureMachine, storage.PusherOptions{})
	require.NoError(tb, err)
	tb.Cleanup(func() { syncer.Close() })
	require.NoError(tb, syncer.EnsureSchema(ctx))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(tb, err)
	store, err := NewStore(ctx, target)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	return store, local
}

// TestAnalyticsIDSetReadsMatchSQLite covers the analytics reads that take
// the selected session IDs as one set: the summary model list with and
// without a time filter, and tool counts under a model filter. Each read
// selects more sessions than one placeholder batch and must return what
// SQLite returns. BenchmarkAnalyticsIDSetReads reports how many statements
// the reads run.
func TestAnalyticsIDSetReadsMatchSQLite(t *testing.T) {
	t.Parallel()
	sessions := readbase.MaxSQLVars + 300
	store, local := seedIDSetMirror(t, sessions, 4, sessions)
	ctx := t.Context()
	january := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}
	hour := 10
	atTen := january
	atTen.Hour = &hour
	sharedAtTen := atTen
	sharedAtTen.Model = "shared"
	shared := january
	shared.Model = "shared"

	// Every eighth session starts at 11:00, outside the hour filter.
	atTenSessions := sessions - (sessions+7)/8
	require.Greater(t, atTenSessions, readbase.MaxSQLVars)
	for _, tc := range []struct {
		name   string
		filter db.AnalyticsFilter
		models int
	}{
		{"no_time_filter", january, sessions + 1},
		{"hour_filter", atTen, atTenSessions + 1},
		{"model_and_hour_filter", sharedAtTen, 1},
	} {
		t.Run("summary_models_"+tc.name, func(t *testing.T) {
			want, err := local.GetAnalyticsSummary(ctx, tc.filter)
			require.NoError(t, err)
			got, err := store.GetAnalyticsSummary(ctx, tc.filter)
			require.NoError(t, err)
			assert.Equal(t, want.Models, got.Models)
			assert.Len(t, got.Models, tc.models)
		})
	}

	t.Run("activity_tool_calls_model_and_hour_filter", func(t *testing.T) {
		want, err := local.GetAnalyticsActivity(ctx, sharedAtTen, "day")
		require.NoError(t, err)
		got, err := store.GetAnalyticsActivity(ctx, sharedAtTen, "day")
		require.NoError(t, err)
		assert.Equal(t, want, got)
		toolCalls := 0
		for _, entry := range got.Series {
			toolCalls += entry.ToolCalls
		}
		// Even sessions call a tool on their "shared" message; those whose
		// index is a multiple of 8 start outside the hour.
		assert.Equal(t, sessions/2-(sessions+7)/8, toolCalls)
	})

	t.Run("velocity_tool_calls_model_filter", func(t *testing.T) {
		want, err := local.GetAnalyticsVelocity(ctx, shared)
		require.NoError(t, err)
		got, err := store.GetAnalyticsVelocity(ctx, shared)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.NotEmpty(t, got.ByAgent)
	})
}
