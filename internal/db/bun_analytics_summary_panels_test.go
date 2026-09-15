package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBunAnalyticsSummaryPanelsReduceModelScopedFactsInDatabase(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T12:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "aggregate-summary-panels", Project: "aggregate", Machine: "host",
		Agent: "codex", CreatedAt: started, StartedAt: &started,
		MessageCount: 2, UserMessageCount: 1,
		TotalOutputTokens: 105, HasTotalOutputTokens: true,
	}))
	require.NoError(t, database.InsertMessages([]Message{
		{
			SessionID: "aggregate-summary-panels", Ordinal: 0, Role: "user",
			Content: "question", ContentLength: 8, Timestamp: started,
		},
		{
			SessionID: "aggregate-summary-panels", Ordinal: 1, Role: "assistant",
			Content: "answer", ContentLength: 6, Timestamp: started, Model: "gpt-5",
			OutputTokens: 5, HasOutputTokens: true,
		},
	}))

	filter := AnalyticsFilter{
		Project: "aggregate", Model: "gpt-5", From: "2026-08-04",
		To: "2026-08-04", Timezone: "UTC",
	}
	tests := []struct {
		name string
		run  func(*BunStore)
	}{
		{name: "summary", run: func(store *BunStore) {
			got, err := store.GetAnalyticsSummary(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 2, got.TotalMessages)
			assert.Equal(t, 5, got.TotalOutputTokens)
		}},
		{name: "heatmap", run: func(store *BunStore) {
			got, err := store.GetAnalyticsHeatmap(t.Context(), filter, "messages")
			require.NoError(t, err)
			require.Len(t, got.Entries, 1)
			assert.Equal(t, 2, got.Entries[0].Value)
		}},
		{name: "projects", run: func(store *BunStore) {
			got, err := store.GetAnalyticsProjects(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, got.Projects, 1)
			assert.Equal(t, 2, got.Projects[0].Messages)
		}},
		{name: "hour of week", run: func(store *BunStore) {
			got, err := store.GetAnalyticsHourOfWeek(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 2, findHOWCell(got.Cells, 1, 12))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hook := new(countingQueryHook)
			store := NewBunStore(&sqliteAnalyticsAggregateBackend{
				store: database.bunReader.WithQueryHook(hook),
			})
			test.run(store)
			assert.Equal(t, 1, hook.selects,
				"the database should return compact panel aggregates")
		})
	}
}
