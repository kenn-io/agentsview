//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/storage"
)

// TestAnalyticsActivityRoleSplitParity pins the user / assistant / other
// split behind the stacked activity timeline: system-injected and tool-result
// user rows count toward the total but not the user segment on every backend.
func TestAnalyticsActivityRoleSplitParity(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ctx := t.Context()

	sessions := []struct {
		id   string
		ts   string
		msgs []db.Message
	}{
		{"role-assistant-only", "2026-02-01T10:00:00Z", []db.Message{
			{Role: "assistant", Content: "a"},
			{Role: "assistant", Content: "b"},
		}},
		{"role-system-only", "2026-02-02T10:00:00Z", []db.Message{
			{Role: "user", Content: "banner", IsSystem: true},
			{Role: "user", Content: "marker", IsSystem: true},
		}},
		{"role-mixed", "2026-02-03T10:00:00Z", []db.Message{
			{Role: "user", Content: "question"},
			{Role: "user", Content: "banner", IsSystem: true},
			{Role: "user", Content: "result", SourceSubtype: "tool_result"},
			{Role: "assistant", Content: "answer"},
		}},
	}
	for _, sess := range sessions {
		started := sess.ts
		require.NoError(t, local.UpsertSession(ctx, db.Session{
			ID: sess.id, Project: "project", Machine: "local", Agent: "claude",
			StartedAt: &started, EndedAt: &started, MessageCount: len(sess.msgs),
		}))
		for i := range sess.msgs {
			sess.msgs[i].SessionID = sess.id
			sess.msgs[i].Ordinal = i
			sess.msgs[i].Timestamp = sess.ts
			sess.msgs[i].ContentLength = len(sess.msgs[i].Content)
		}
		require.NoError(t, local.InsertMessages(ctx, sess.msgs))
	}

	ps, err := New(t.Context(), pgURL, "agentsview", local, "analytics-test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(ctx))
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), pgURL, "agentsview", true)
	require.NoError(t, err)
	defer store.Close()
	duckPath := filepath.Join(t.TempDir(), "analytics.duckdb")
	_, err = duckdb.Push(ctx, duckPath, local, "analytics-test-machine", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	duckStore, err := duckdb.NewStore(ctx, duckPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, duckStore.Close()) })

	want := []struct {
		date                             string
		messages, user, assistant, other int
	}{
		{"2026-02-01", 2, 0, 2, 0},
		{"2026-02-02", 2, 0, 0, 2},
		{"2026-02-03", 4, 1, 1, 2},
	}
	filter := db.AnalyticsFilter{From: "2026-02-01", To: "2026-02-03", Timezone: "UTC"}
	for name, backend := range map[string]interface {
		GetAnalyticsActivity(context.Context, db.AnalyticsFilter, string) (db.ActivityResponse, error)
	}{"sqlite": local, "postgres": store, "duckdb": duckStore} {
		t.Run(name, func(t *testing.T) {
			report, err := backend.GetAnalyticsActivity(ctx, filter, "day")
			require.NoError(t, err)
			require.Len(t, report.Series, len(want))
			for i, w := range want {
				entry := report.Series[i]
				assert.Equal(t, w.date, entry.Date)
				assert.Equal(t, w.messages, entry.Messages, w.date)
				assert.Equal(t, w.user, entry.UserMessages, w.date)
				assert.Equal(t, w.assistant, entry.AssistantMessages, w.date)
				assert.Equal(t, w.other,
					entry.Messages-entry.UserMessages-entry.AssistantMessages, w.date)
			}
		})
	}
}
