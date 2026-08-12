package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBunAnalyticsDialectPreservesNestedArguments(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		d    BunAnalyticsDialect
		want int
	}{
		{name: "sqlite", d: SQLiteBunAnalyticsDialect(), want: 2},
		{name: "postgres", d: PostgresBunAnalyticsDialect(), want: 1},
		{name: "duckdb", d: DuckDBBunAnalyticsDialect(), want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := test.d.LocalTimestamp("message.timestamp", "America/New_York")
			bucket := test.d.Bucket(local, "week")

			assert.Len(t, bucket.Args, test.want)
			for _, arg := range bucket.Args {
				assert.Equal(t, "America/New_York", arg)
			}
		})
	}
}

func TestBunAnalyticsSQLBuildsPairedModelAndToolFactCTEs(t *testing.T) {
	t.Parallel()
	day, hour := 1, 9

	builder, err := newBunAnalyticsSQL(
		SQLiteBunAnalyticsDialect(), AnalyticsFilter{
			Model: "gpt-5, claude-opus", Timezone: "America/New_York",
			DayOfWeek: &day, Hour: &hour,
		},
	)
	require.NoError(t, err)

	messageCTEs := builder.scopedMessageCTEs()
	require.Len(t, messageCTEs, 3)
	assert.Equal(t, "analytics_message_candidates", messageCTEs[0].Name)
	assert.Contains(t, messageCTEs[0].Query.SQL,
		"ROWS BETWEEN 1 FOLLOWING AND UNBOUNDED FOLLOWING")
	assert.Equal(t, "analytics_scoped_messages", messageCTEs[1].Name)
	assert.Contains(t, messageCTEs[1].Query.SQL,
		"next_assistant.ordinal = candidate.next_assistant_ordinal")
	assert.Contains(t, messageCTEs[1].Query.SQL, "candidate.model IN (?)")
	assert.Contains(t, messageCTEs[1].Query.SQL, "next_assistant.model IN (?)")
	assert.NotContains(t, messageCTEs[1].Query.SQL, "2026-")
	assert.NotContains(t, messageCTEs[1].Query.SQL, "gpt-5")
	assert.Contains(t, messageCTEs[1].Query.Args, "America/New_York")
	assert.Equal(t, "analytics_qualified_sessions", messageCTEs[2].Name)
	assert.Contains(t, messageCTEs[2].Query.SQL, "GROUP BY session_id")

	tools := builder.directToolFactsCTE()
	assert.Equal(t, "analytics_tool_facts", tools.Name)
	assert.Contains(t, tools.Query.SQL,
		"tool_call.message_ordinal = message.ordinal")
	assert.Contains(t, tools.Query.SQL, "message.model IN (?)")
	assert.NotContains(t, tools.Query.SQL, "claude-opus")
}

func TestBunAnalyticsSQLExecutesPairedMessageScopeThroughBun(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T12:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "paired-scope", Project: "scope", Machine: "host", Agent: "codex",
		CreatedAt: started, StartedAt: &started,
		MessageCount: 4, UserMessageCount: 2,
	}))
	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "paired-scope", Ordinal: 0, Role: "user", Content: "selected"},
		{SessionID: "paired-scope", Ordinal: 1, Role: "assistant", Model: "gpt-5", Content: "yes"},
		{SessionID: "paired-scope", Ordinal: 2, Role: "user", Content: "other"},
		{SessionID: "paired-scope", Ordinal: 3, Role: "assistant", Model: "other", Content: "no"},
	}))

	builder, err := newBunAnalyticsSQL(
		SQLiteBunAnalyticsDialect(), AnalyticsFilter{Project: "scope", Model: "gpt-5"},
	)
	require.NoError(t, err)
	ctes := []BunCTEFragment{builder.filteredSessionsCTE(
		database.bunReader, sqliteTimestampOrderExpr, false,
	)}
	ctes = append(ctes, builder.scopedMessageCTEs()...)
	with := renderBunCTEs(ctes...)
	var rows []struct {
		Ordinal int    `bun:"ordinal"`
		Role    string `bun:"role"`
	}
	err = database.bunReader.NewRaw("WITH "+with.SQL+
		" SELECT ordinal, role FROM "+bunAnalyticsScopedMessagesCTE+
		" ORDER BY ordinal", with.Args...).Scan(t.Context(), &rows)
	require.NoError(t, err)
	assert.Equal(t, []struct {
		Ordinal int    `bun:"ordinal"`
		Role    string `bun:"role"`
	}{
		{Ordinal: 0, Role: "user"},
		{Ordinal: 1, Role: "assistant"},
	}, rows)
}

func TestBunAnalyticsModelScopeLeavesDateRangeAtSessionGrain(t *testing.T) {
	t.Parallel()

	builder, err := newBunAnalyticsSQL(
		SQLiteBunAnalyticsDialect(), AnalyticsFilter{
			From: "2026-08-01", To: "2026-08-31", Model: "gpt-5", Timezone: "UTC",
		},
	)
	require.NoError(t, err)

	messageCTEs := builder.scopedMessageCTEs()
	require.Len(t, messageCTEs, 3)
	assert.NotContains(t, messageCTEs[1].Query.SQL, "candidate.timestamp")
	assert.NotContains(t, messageCTEs[1].Query.Args, "2026-08-01")
	assert.NotContains(t, messageCTEs[1].Query.Args, "2026-08-31")

	tools := builder.directToolFactsCTE()
	assert.Contains(t, tools.Query.Args, "2026-08-01")
	assert.Contains(t, tools.Query.Args, "2026-08-31")
}

func TestSQLiteAnalyticsLocalTimestampIsStrictAndDSTCorrect(t *testing.T) {
	database := testDB(t)
	var before, after, invalid, missing *string

	err := database.bunReader.NewRaw(`SELECT
		agentsview_local_timestamp(?, ?),
		agentsview_local_timestamp(?, ?),
		agentsview_local_timestamp(?, ?),
		agentsview_local_timestamp(NULL, ?)`,
		"2026-03-08T06:59:00Z", "America/New_York",
		"2026-03-08T07:01:00Z", "America/New_York",
		"not-a-timestamp", "America/New_York",
		"America/New_York",
	).Scan(t.Context(), &before, &after, &invalid, &missing)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, after)
	assert.Equal(t, "2026-03-08 01:59:00", *before)
	assert.Equal(t, "2026-03-08 03:01:00", *after)
	assert.Nil(t, invalid)
	assert.Nil(t, missing)
}
