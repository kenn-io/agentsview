package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

var factsFilter = StatsFilter{Since: "2026-06-01", Until: "2026-07-01", Timezone: "UTC"}

func seedStatsFacts(t *testing.T, d *DB) {
	t.Helper()
	for _, entry := range []struct{ id, agent string }{{"session", "claude"}, {"codex", "codex"}} {
		insertSessionFixture(t, d, sessionFixture{id: entry.id, agent: entry.agent, project: "project-a", startedAt: "2026-06-16T10:00:00Z", userMsgs: 2, messageCount: 4})
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, statsFactMessages(entry.id, 30)))
	}
}

func statsFactMessages(id string, delay int) []Message {
	return []Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-16T10:00:00Z"},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: time.Date(2026, 6, 16, 10, 0, delay, 0, time.UTC).Format(time.RFC3339), Model: "test-model", TokenUsage: jsontext.Value(`{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":40,"cache_creation":{"ephemeral_1h_input_tokens":10}}`), OutputTokens: 20, ToolCalls: []ToolCall{{ToolName: "ExitPlanMode", Category: "Other"}, {ToolName: "Task", Category: "Task"}, {ToolName: "Skill", Category: "Other", SkillName: "review"}}},
	}
}

func freshStatsFacts(t *testing.T, d *DB, f StatsFilter) *SessionStats {
	t.Helper()
	fresh, err := OpenReadOnly(t.Context(), d.Path())
	require.NoError(t, err)
	defer fresh.Close()
	stats, err := fresh.GetSessionStats(t.Context(), f)
	require.NoError(t, err)
	return stats
}

func equalStatsFacts(t *testing.T, want, got *SessionStats) {
	t.Helper()
	want.GeneratedAt = ""
	got.GeneratedAt = ""
	a, err := json.Marshal(want)
	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(a), string(b))
}

func TestSessionStatsFactsChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"warm", "append", "replace", "usage", "committed usage", "recreate", "new", "agent", "project", "visibility", "relationship", "pricing", "agent filter", "narrow window", "wide window", "timezone", "empty"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			d := testDB(t)
			seedStatsFacts(t, d)
			f := factsFilter
			if change == "committed usage" {
				messages := statsFactMessages("session", 30)
				messages[1].TokenUsage = nil
				messages[1].OutputTokens = 0
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", messages))
			}
			_, err := d.GetSessionStats(t.Context(), f)
			require.NoError(t, err)
			switch change {
			case "append":
				require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 2, Role: "user", Content: "again", Timestamp: "2026-06-16T10:02:00Z"}, {SessionID: "session", Ordinal: 3, Role: "assistant", Content: "done", Timestamp: "2026-06-16T10:03:00Z"}}))
			case "replace":
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", statsFactMessages("session", 50)))
			case "usage":
				m := statsFactMessages("session", 30)
				m[1].TokenUsage = jsontext.Value(`{"input_tokens":300,"output_tokens":70}`)
				m[1].OutputTokens = 70
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", m))
			case "committed usage":
				_, err = d.WriteSessionIncremental(t.Context(), "session", nil, IncrementalSessionUpdate{MsgCount: 4, NextOrdinal: 2, MessageTokenUsageUpdates: []MessageTokenUsageUpdate{{Ordinal: 1, TokenUsage: jsontext.Value(`{"input_tokens":300,"output_tokens":70}`), OutputTokens: 70, HasOutputTokens: true}}})
				require.NoError(t, err)
			case "recreate":
				_, err = d.DeleteParserExcludedSessions(t.Context(), []string{"session"})
				require.NoError(t, err)
				insertSessionFixture(t, d, sessionFixture{id: "session", agent: "claude", startedAt: "2026-06-16T10:00:00Z", userMsgs: 2})
				require.NoError(t, d.InsertMessages(t.Context(), statsFactMessages("session", 50)))
			case "new":
				insertSessionFixture(t, d, sessionFixture{id: "new", startedAt: "2026-06-16T10:00:00Z", userMsgs: 2})
				require.NoError(t, d.InsertMessages(t.Context(), statsFactMessages("new", 50)))
			case "agent", "project", "visibility", "relationship":
				query := map[string]string{"agent": "UPDATE sessions SET agent='codex' WHERE id='session'", "project": "UPDATE sessions SET project='project-b' WHERE id='session'", "visibility": "UPDATE sessions SET deleted_at='2026-06-17' WHERE id='session'", "relationship": "UPDATE sessions SET relationship_type='subagent' WHERE id='session'"}[change]
				_, err = d.getWriter().Exec(t.Context(), query)
				require.NoError(t, err)
				got, err := d.GetSessionStats(t.Context(), f)
				require.NoError(t, err)
				equalStatsFacts(t, freshStatsFacts(t, d, f), got)
				if change == "agent" || change == "relationship" {
					column, value := "agent", "claude"
					if change == "relationship" {
						column, value = "relationship_type", ""
					}
					_, err = d.getWriter().Exec(t.Context(), "UPDATE sessions SET "+column+"=? WHERE id='session'", value)
					require.NoError(t, err)
				}
			case "pricing":
				require.NoError(t, d.UpsertModelPricing([]ModelPricing{{ModelPattern: "test-model", InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("4"), CacheReadPerMTok: money.MustParseDollars(".2"), CacheCreationPerMTok: money.MustParseDollars("2.5")}}))
			case "agent filter":
				f.Agent = "codex"
				_, err = d.GetSessionStats(t.Context(), f)
				require.NoError(t, err)
				f.Agent = ""
			case "narrow window":
				f.Until = "2026-06-16T10:00:20Z"
			case "wide window":
				f.Until = "2026-06-16T10:00:20Z"
				_, err = d.GetSessionStats(t.Context(), f)
				require.NoError(t, err)
				f.Until = "2026-07-01"
			case "timezone":
				f.Timezone = "America/New_York"
			case "empty":
				f.IncludeProjects = []string{"absent"}
			}
			got, err := d.GetSessionStats(t.Context(), f)
			require.NoError(t, err)
			equalStatsFacts(t, freshStatsFacts(t, d, f), got)
			if change == "warm" {
				assert.InDelta(t, 30.0, got.Velocity.TurnCycleSeconds.P50, 1e-9)
				assert.Equal(t, 6, got.ToolMix.TotalCalls)
				assert.Equal(t, int64(40), got.ModelMix.ByTokens["test-model"])
			}
		})
	}
}

func TestSessionStatsFactsLimitsAndOwnership(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"byte cap", "readonly", "mutation", "cancel", "no identity"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			d := testDB(t)
			seedStatsFacts(t, d)
			switch mode {
			case "byte cap":
				d.sessionStatsFacts.rows.MaxBytes = 1
			case "readonly":
				r, err := OpenReadOnly(t.Context(), d.Path())
				require.NoError(t, err)
				defer r.Close()
				d = r
			case "no identity":
				_, err := d.getWriter().Exec(t.Context(), "DELETE FROM archive_metadata WHERE key='database_id'")
				require.NoError(t, err)
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, err := d.GetSessionStats(ctx, factsFilter)
				require.ErrorIs(t, err, context.Canceled)
				require.Empty(t, d.sessionStatsFacts.Lookup(0, ""))
				return
			}
			got, err := d.GetSessionStats(t.Context(), factsFilter)
			require.NoError(t, err)
			if mode == "mutation" {
				got.ToolMix.ByCategory["Other"] = 999
				got.ModelMix.ByTokens["test-model"] = 999
				got.CacheEconomics.DollarsSpent = money.MustParseDollars("999")
				got.Adoption.DistinctSkills = 999
			}
			again, err := d.GetSessionStats(t.Context(), factsFilter)
			require.NoError(t, err)
			equalStatsFacts(t, freshStatsFacts(t, d, factsFilter), again)
			if mode == "readonly" {
				assert.Nil(t, d.sessionStatsFacts)
			}
			if mode == "byte cap" || mode == "no identity" {
				id, rev, e := d.databaseIDAndUsagePricingRevision(t.Context())
				require.NoError(t, e)
				entries, ok := d.sessionStatsFacts.rows.Get("facts", fmt.Sprintf("%s|%d", id, rev))
				assert.False(t, ok)
				assert.Empty(t, entries)
			}
		})
	}
}

func TestSessionStatsFactsConcurrent(t *testing.T) {
	d := testDB(t)
	seedStatsFacts(t, d)
	want := freshStatsFacts(t, d, factsFilter)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := d.GetSessionStats(t.Context(), factsFilter)
			if assert.NoError(t, err) {
				got.GeneratedAt = ""
				a, e := json.Marshal(got)
				assert.NoError(t, e)
				wantCopy := *want
				wantCopy.GeneratedAt = ""
				b, e := json.Marshal(wantCopy)
				assert.NoError(t, e)
				assert.JSONEq(t, string(b), string(a))
			}
		})
	}
	wg.Wait()
}

func TestSessionStatsFactsSnapshotAndReaderSwap(t *testing.T) {
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer", true: "reader swap"}[swap], func(t *testing.T) {
			d := testDB(t)
			seedStatsFacts(t, d)
			_, err := d.GetSessionStats(t.Context(), factsFilter)
			require.NoError(t, err)
			d.reader.Load().SetMaxOpenConns(1)
			entered, resume := make(chan struct{}), make(chan struct{})
			var once, resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			defer release()
			conn, err := d.getReader().Conn(t.Context())
			require.NoError(t, err)
			require.NoError(t, conn.Raw(func(raw any) error {
				raw.(interface {
					RegisterAuthorizer(func(int, string, string, string) int)
				}).RegisterAuthorizer(func(op int, table, _, _ string) int {
					if op == sqlite3.SQLITE_READ && table == "usage_session_changes" {
						once.Do(func() {
							close(entered)
							select {
							case <-resume:
							case <-t.Context().Done():
							}
						})
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(t, conn.Close())
			result := make(chan sessionStatsFactsByID, 1)
			fail := make(chan error, 1)
			go func() {
				facts, e := d.readSessionStatsFacts(t.Context(), []sessionStatsRow{{id: "session", agent: "claude"}}, []sessionStatsRow{{id: "session", agent: "claude"}})
				result <- facts
				fail <- e
			}()
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				require.FailNow(t, "validation did not start")
			}
			other, err := OpenIsolated(t.Context(), d.Path())
			require.NoError(t, err)
			require.NoError(t, other.ReplaceSessionMessages(t.Context(), "session", statsFactMessages("session", 50)))
			require.NoError(t, other.Close())
			if swap {
				require.NoError(t, d.Reopen())
			}
			release()
			var facts sessionStatsFactsByID
			select {
			case facts = <-result:
			case <-time.After(30 * time.Second):
				require.FailNow(t, "validation did not finish")
			}
			require.NoError(t, <-fail)
			assert.Equal(t, []float64{30}, facts["session"].velocity.turnCycles)
			if swap {
				id, rev, e := d.databaseIDAndUsagePricingRevision(t.Context())
				require.NoError(t, e)
				rows, ok := d.sessionStatsFacts.rows.Get("facts", fmt.Sprintf("%s|%d", id, rev))
				assert.False(t, ok)
				assert.Empty(t, rows)
			}
			got, err := d.GetSessionStats(t.Context(), factsFilter)
			require.NoError(t, err)
			assert.InDelta(t, 50.0, got.Velocity.TurnCycleSeconds.P50, 1e-9)
		})
	}
}

func TestSessionStatsFactsCacheOneHour(t *testing.T) {
	d := testDB(t)
	insertSessionFixture(t, d, sessionFixture{id: "session", agent: "claude", startedAt: "2026-06-16T10:00:00Z", userMsgs: 2})
	require.NoError(t, d.InsertMessages(t.Context(), statsFactMessages("session", 30)))
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{ModelPattern: "test-model", InputPerMTok: money.MustParseDollars("1"), OutputPerMTok: money.MustParseDollars("2"), CacheCreationPerMTok: money.MustParseDollars("1.25"), CacheCreation1hPerMTok: money.MustParseDollars("2"), CacheReadPerMTok: money.MustParseDollars(".1")}}))
	for range 2 {
		got, err := d.GetSessionStats(t.Context(), factsFilter)
		require.NoError(t, err)
		require.NotNil(t, got.CacheEconomics)
		assert.Equal(t, int64(206), got.CacheEconomics.DollarsSpent.Microdollars)
		assert.Equal(t, int64(54), got.CacheEconomics.DollarsSavedVsUncached.Microdollars)
	}
}

func TestSessionStatsFactsWarmSkipsTranscriptReads(t *testing.T) {
	d := testDB(t)
	seedStatsFacts(t, d)
	want, err := d.GetSessionStats(t.Context(), factsFilter)
	require.NoError(t, err)
	d.rawReader().SetMaxOpenConns(1)
	conn, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(interface {
			RegisterAuthorizer(func(int, string, string, string) int)
		}).RegisterAuthorizer(func(op int, table, column, _ string) int {
			if op == sqlite3.SQLITE_READ && ((table == "messages" && column == "token_usage") || (table == "tool_calls" && column == "category")) {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	// Temporal still reads timestamps while retained facts skip message usage.
	got, err := d.GetSessionStats(t.Context(), factsFilter)
	require.NoError(t, err)
	equalStatsFacts(t, want, got)
}
