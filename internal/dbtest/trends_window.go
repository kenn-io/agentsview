package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// SeedTrendsWindow writes messages on both sides of 2025-03-10 in the
// farthest-east (UTC+14) and farthest-west (UTC-12) timezones, messages
// without a timestamp, a second project, and a prompt whose answering model
// replies three days after the range. Use AssertTrendsWindow after pushing it.
func SeedTrendsWindow(t *testing.T, d *db.DB) {
	t.Helper()
	seed := func(id, project, started string, msgs ...db.Message) {
		SeedSession(t, d, id, project, func(s *db.Session) {
			s.StartedAt = new(started)
			s.CreatedAt = started
			s.MessageCount = len(msgs)
			s.UserMessageCount = len(msgs)
		})
		for i := range msgs {
			msgs[i].SessionID = id
			msgs[i].Ordinal = i
			msgs[i].ContentLength = len(msgs[i].Content)
		}
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, msgs))
	}
	user := func(content, ts string) db.Message {
		return db.Message{Role: "user", Content: content, Timestamp: ts}
	}
	// Pacific/Kiritimati's 2025-03-10 is [03-09T10:00Z, 03-10T10:00Z) and
	// Etc/GMT+12's is [03-10T12:00Z, 03-11T12:00Z).
	seed("trends-window-edges", "trends-window", "2025-03-09T09:00:00Z",
		user("kiri", "2025-03-09T09:59:59Z"),
		user("kiri", "2025-03-09T10:00:00Z"),
		user("kiri", "2025-03-10T09:59:59Z"),
		user("kiri", "2025-03-10T10:00:00Z"),
		user("baker", "2025-03-10T11:59:59Z"),
		user("baker", "2025-03-10T12:00:00Z"),
		user("baker", "2025-03-11T11:59:59Z"),
		user("baker", "2025-03-11T12:00:00Z"),
	)
	seed("trends-window-other", "trends-window-other", "2025-03-09T09:00:00Z",
		user("kiri", "2025-03-09T12:00:00Z"))
	// Messages without a timestamp fall back to the session start.
	seed("trends-window-fallback", "trends-window", "2025-03-10T05:00:00Z",
		user("fallback", ""))
	seed("trends-window-old-fallback", "trends-window", "2024-01-01T12:00:00Z",
		user("fallback", ""))
	seed("trends-window-offset", "trends-window", "2024-01-01T00:00:00Z",
		user("offset", "2025-03-08T22:00:00-12:00"))
	// A user prompt counts for the model of the next assistant message.
	seed("trends-window-model", "trends-window", "2025-03-10T08:00:00Z",
		user("prompt", "2025-03-10T09:00:00Z"),
		db.Message{
			Role: "assistant", Content: "reply", Model: "gpt-boundary",
			Timestamp: "2025-03-13T09:00:00Z",
		},
	)
}

// AssertTrendsWindow checks store's term counts for the SeedTrendsWindow
// fixture against the counts the archive defines.
func AssertTrendsWindow(t *testing.T, store db.Store) {
	t.Helper()
	terms, err := db.ParseTrendTerms([]string{"kiri", "baker", "fallback", "prompt", "reply"})
	require.NoError(t, err)
	type want struct {
		messages                             int
		kiri, baker, fallback, prompt, reply int
	}
	day := db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10"}
	for _, tc := range []struct {
		name     string
		timezone string
		project  string
		model    string
		want     want
	}{
		{
			name: "far east", timezone: "Pacific/Kiritimati",
			want: want{messages: 6, kiri: 3, fallback: 1, prompt: 1},
		},
		{
			name: "far east project", timezone: "Pacific/Kiritimati", project: "trends-window",
			want: want{messages: 5, kiri: 2, fallback: 1, prompt: 1},
		},
		{
			name: "far west", timezone: "Etc/GMT+12",
			want: want{messages: 2, baker: 2},
		},
		{
			name: "model reply after range", timezone: "UTC", model: "gpt-boundary",
			want: want{messages: 1, prompt: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := day
			f.Timezone = tc.timezone
			f.Project = tc.project
			f.Model = tc.model
			got, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
			require.NoError(t, err)
			require.Len(t, got.Series, len(terms))
			totals := map[string]int{}
			for _, series := range got.Series {
				totals[series.Term] = series.Total
			}
			assert.Equal(t, tc.want, want{
				messages: got.MessageCount,
				kiri:     totals["kiri"], baker: totals["baker"],
				fallback: totals["fallback"], prompt: totals["prompt"],
				reply: totals["reply"],
			})
		})
	}
}

// AssertTrendsPartsReuse checks the persisted result across reopen and rewrite.
func AssertTrendsPartsReuse(t *testing.T, open func(bool) db.Store, change func(string)) {
	t.Helper()
	terms, err := db.ParseTrendTerms([]string{"fallback", "offset"})
	require.NoError(t, err)
	f := db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}
	var reads [][]string
	reset := db.SetTrendPartsReadObserver(func(ids []string) {
		reads = append(reads, append([]string(nil), ids...))
	})
	t.Cleanup(reset)
	firstStore := open(true)
	first, err := firstStore.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, firstStore.(interface{ Close() error }).Close())
	require.NoError(t, err)
	require.Equal(t, 1, first.Series[0].Total)
	require.NotEmpty(t, reads)
	for _, ids := range reads {
		assert.NotContains(t, ids, "trends-window-old-fallback")
	}
	reads = nil
	restartedStore := open(true)
	restarted, err := restartedStore.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, restartedStore.(interface{ Close() error }).Close())
	require.NoError(t, err)
	assert.Equal(t, first, restarted)
	assert.Empty(t, reads)
	change("rewrite")
	changedStore := open(true)
	changed, err := changedStore.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	assert.Equal(t, 2, changed.Series[0].Total)
	assert.Equal(t, [][]string{{"trends-window-fallback"}}, reads)

	directStore := open(false)
	for _, filter := range []db.AnalyticsFilter{
		{From: "2025-03-10", To: "2025-03-11", Timezone: "UTC"},
		{From: "2025-03-10", To: "2025-03-10", Timezone: "Pacific/Kiritimati"},
	} {
		got, err := changedStore.GetTrendsTerms(t.Context(), filter, terms, "day")
		require.NoError(t, err)
		want, err := directStore.GetTrendsTerms(t.Context(), filter, terms, "day")
		require.NoError(t, err)
		assert.Equal(t, want, got, "%+v", filter)
		if filter.Timezone == "Pacific/Kiritimati" {
			assert.Equal(t, 1, got.Series[1].Total)
		}
	}
	require.NoError(t, directStore.(interface{ Close() error }).Close())
	require.NoError(t, changedStore.(interface{ Close() error }).Close())
	for _, tc := range []struct {
		action string
		total  int
	}{{"delete", 0}, {"restore", 2}} {
		change(tc.action)
		retained, direct := open(true), open(false)
		got, err := retained.GetTrendsTerms(t.Context(), db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}, terms, "day")
		require.NoError(t, err)
		want, err := direct.GetTrendsTerms(t.Context(), db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}, terms, "day")
		require.NoError(t, err)
		assert.Equal(t, tc.total, got.Series[0].Total)
		assert.Equal(t, want, got)
		require.NoError(t, retained.(interface{ Close() error }).Close())
		require.NoError(t, direct.(interface{ Close() error }).Close())
	}
}

// ChangeTrendsWindow changes one session for retention tests; a rewrite keeps its timestamp.
func ChangeTrendsWindow(t *testing.T, d *db.DB, action string) {
	t.Helper()
	switch action {
	case "rewrite":
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), "trends-window-fallback", []db.Message{{
			SessionID: "trends-window-fallback", Ordinal: 0, Role: "user", Content: "fallback fallback", ContentLength: 17,
		}}))
		require.NoError(t, d.SetSessionDataVersion(t.Context(), "trends-window-fallback", db.CurrentDataVersion()))
	case "delete":
		require.NoError(t, d.SoftDeleteSession(t.Context(), "trends-window-fallback"))
	case "restore":
		_, err := d.RestoreSession(t.Context(), "trends-window-fallback")
		require.NoError(t, err)
	default:
		require.FailNow(t, "unknown action", "action: %q", action)
	}
}
