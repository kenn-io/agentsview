package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkGetAnalyticsToolsYearRange(b *testing.B) {
	d := testDB(b)
	start := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	for day := range 1096 {
		id := fmt.Sprintf("day-%d", day)
		ts := start.AddDate(0, 0, day)
		require.NoError(b, d.UpsertSession(b.Context(), Session{ID: id, Project: "bench", Machine: "local", Agent: "claude", StartedAt: new(ts.Format(time.RFC3339)), MessageCount: 30}))
		msgs := make([]Message, 30)
		for n := range msgs {
			msgs[n] = asstMsgAt(id, n, "read", ts.Add(time.Duration(n)*time.Second).Format(time.RFC3339))
			msgs[n].ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		}
		require.NoError(b, d.InsertMessages(b.Context(), msgs))
	}
	f := AnalyticsFilter{From: "2025-01-01", To: "2025-12-31", Timezone: "UTC"}
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.GetAnalyticsTools(b.Context(), f)
		require.NoError(b, err)
		require.Equal(b, 10950, resp.TotalCalls)
	}
}

// Cold dashboard iterations repeat the prompt scan paid by every request before
// memoization. The two axes vary session count and total user-message bytes.
func BenchmarkSignalsFrustration(b *testing.B) {
	for _, size := range []struct {
		name     string
		sessions int
		bytes    int
	}{
		{"sessions/1000", 1000, 1000 * 10 * 500},
		{"sessions/4000", 4000, 4000 * 10 * 500},
		{"bytes/2MB", 500, 2 << 20},
		{"bytes/16MB", 500, 16 << 20},
	} {
		b.Run(size.name, func(b *testing.B) {
			d := testDB(b)
			ctx := b.Context()
			text := "this is broken " + strings.Repeat("x", size.bytes/(size.sessions*10)-len("this is broken "))
			for i := range size.sessions {
				id := fmt.Sprintf("session-%d", i)
				msgs := make([]Message, 10)
				for j := range msgs {
					msgs[j] = Message{SessionID: id, Ordinal: j, Role: "user", Content: text}
				}
				result, err := d.WriteSessionBatchContext(ctx, []SessionBatchWrite{{
					Session:  Session{ID: id, Project: "bench", Machine: "local", Agent: "claude", MessageCount: 10},
					Messages: msgs,
					Signals:  SessionSignalUpdate{ToolRetryCount: 1, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}},
				}})
				require.NoError(b, err)
				require.Empty(b, result.Errors)
			}
			for _, mode := range []string{"cold", "warm", "tool_retries"} {
				b.Run(mode, func(b *testing.B) {
					_, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
					require.NoError(b, err)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if mode == "cold" || mode == "tool_retries" {
							d.frustrationMemo.swap(func() {})
						}
						if mode == "tool_retries" {
							resp, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, mode, 10)
							require.NoError(b, err)
							require.Len(b, resp.Sessions, 10)
						} else {
							resp, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
							require.NoError(b, err)
							require.Equal(b, size.sessions*10, resp.QualityHealth.Totals.FrustrationMarkerCount)
						}
					}
				})
			}
		})
	}
}

func seedTrendsSessions(b *testing.B, d *DB, prefix string, n int, baseDay string, spanDays int, body string) {
	b.Helper()
	ctx := b.Context()
	_, err := d.getWriter().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n < ? - 1)
		INSERT INTO sessions (id, project, machine, agent, message_count, user_message_count, started_at, ended_at)
		SELECT printf('%s-%06d', ?, n), 'bench', 'local', 'claude', 10, 5,
			strftime('%Y-%m-%dT12:00:00Z', ?, printf('-%d days', n % ?)),
			strftime('%Y-%m-%dT12:10:00Z', ?, printf('-%d days', n % ?))
		FROM seq`, n, prefix, baseDay, spanDays, baseDay, spanDays)
	require.NoError(b, err)
	_, err = d.getWriter().ExecContext(ctx, `
		WITH RECURSIVE o(k) AS (SELECT 0 UNION ALL SELECT k + 1 FROM o WHERE k < 9)
		INSERT INTO messages (session_id, ordinal, role, content, timestamp, content_length)
		SELECT s.id, o.k, CASE WHEN o.k % 2 = 0 THEN 'user' ELSE 'assistant' END, ?,
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d minutes', o.k)), ?
		FROM sessions s, o WHERE s.id LIKE ? || '-%'`, body, len(body), prefix)
	require.NoError(b, err)
}

// The selected window stays fixed while three kinds of old history grow.
// The resumed axis with a model filter still grows because model mode reads old assistant rows with blank text to pair user turns by ordinal.
func BenchmarkGetTrendsTermsOutOfRangeHistory(b *testing.B) {
	body := strings.Repeat("the quick brown fox jumps over the lazy dog seam ", 20)
	for _, axis := range []string{"sessions", "resumed", "untimestamped"} {
		for _, old := range []int{0, 3000, 30000} {
			for _, model := range []string{"", "gpt-4o"} {
				b.Run(fmt.Sprintf("%s/history=%d/model=%s", axis, old, model), func(b *testing.B) {
					d := testDB(b)
					ctx := b.Context()
					seedTrendsSessions(b, d, "recent", 300, "2026-11-01", 30, body)
					if old > 0 {
						switch axis {
						case "sessions", "untimestamped":
							seedTrendsSessions(b, d, "old", old, "2026-09-15", 700, body)
							if axis == "untimestamped" {
								_, err := d.getWriter().ExecContext(ctx, "UPDATE messages SET timestamp = NULL WHERE session_id LIKE 'old-%'")
								require.NoError(b, err)
							}
						case "resumed":
							_, err := d.getWriter().ExecContext(ctx, `
								WITH RECURSIVE seq(n) AS (SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n < ? - 1)
								INSERT INTO messages (session_id, ordinal, role, content, timestamp, content_length)
								SELECT 'recent-000000', n + 10, CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END,
									?, '2024-01-01T12:00:00Z', ? FROM seq`, old, body, len(body))
							require.NoError(b, err)
						}
					}
					_, err := d.getWriter().ExecContext(ctx, "UPDATE messages SET model = 'gpt-4o' WHERE role = 'assistant'")
					require.NoError(b, err)
					terms, err := ParseTrendTerms([]string{"load bearing | load-bearing", "seam", "blast radius"})
					require.NoError(b, err)
					f := AnalyticsFilter{From: "2026-10-03", To: "2026-11-01", Timezone: "UTC", Model: model}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						resp, err := d.GetTrendsTerms(ctx, f, terms, "week")
						require.NoError(b, err)
						require.Equal(b, 3000, resp.MessageCount)
					}
				})
			}
		}
	}
}

func BenchmarkAnalyticsDateWindow(b *testing.B) {
	for _, old := range []int{0, 3000, 30000} {
		b.Run(fmt.Sprintf("history=%d", old), func(b *testing.B) {
			d := testDB(b)
			seedTrendsSessions(b, d, "recent", 300, "2026-11-01", 2, "hello")
			if old > 0 {
				seedTrendsSessions(b, d, "old", old, "2026-09-15", 300, "hello")
			}
			shuffleAnalyticsSessions(b, d)
			for _, window := range []struct {
				name, from, to string
				want           int
			}{
				{"dense_two_days", "2026-10-31", "2026-11-01", 300},
				{"empty", "2026-11-03", "2026-11-04", 0},
				{"year", "2025-11-02", "2026-11-01", old + 300},
				{"top_year", "2025-11-02", "2026-11-01", 10},
			} {
				b.Run(window.name, func(b *testing.B) {
					f := AnalyticsFilter{From: window.from, To: window.to, Timezone: "UTC"}
					b.ReportAllocs()
					for b.Loop() {
						if window.name == "top_year" {
							resp, err := d.GetAnalyticsTopSessions(b.Context(), f, "messages")
							require.NoError(b, err)
							require.Len(b, resp.Sessions, window.want)
						} else {
							resp, err := d.GetAnalyticsSummary(b.Context(), f)
							require.NoError(b, err)
							require.Equal(b, window.want, resp.TotalSessions)
						}
					}
				})
			}
		})
	}
}

// Out-of-order syncs can place recent and old sessions on the same table pages.
func shuffleAnalyticsSessions(b *testing.B, d *DB) {
	b.Helper()
	_, err := d.getWriter().ExecContext(b.Context(), `
		WITH shuffled AS MATERIALIZED (
			SELECT rowid AS old, -row_number() OVER (ORDER BY random()) AS shuffled FROM sessions
		)
		UPDATE sessions SET rowid = (SELECT shuffled FROM shuffled WHERE old = sessions.rowid)`)
	require.NoError(b, err)
}
