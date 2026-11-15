//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGSchemaRepairsLeaveSessionVersionsSettled(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx := t.Context()
	require.NoError(t, EnsureSchema(ctx, ps.DB(), "agentsview"))

	for _, tc := range []struct {
		name   string
		repair func() error
	}{
		{"EnsureSchema", func() error { return EnsureSchema(ctx, ps.DB(), "agentsview") }},
		{"runSchemaDataRepairsPG", func() error { return runSchemaDataRepairsPG(ctx, ps.DB()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			_, err := ps.DB().ExecContext(ctx, `DELETE FROM sessions`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent, message_count) VALUES ('one', 'host', 'project', 'codex', 1), ('two', 'host', 'project', 'codex', 1)`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `INSERT INTO messages (session_id, ordinal, role, content, timestamp) VALUES ('one', 0, 'user', 'hello', '2026-01-01T12:00:00Z'), ('two', 0, 'user', 'hello', '2026-01-01T12:00:00Z')`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `UPDATE messages SET usage_output_tokens = NULL, usage_web_search_requests = NULL`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = NOW()`)
			require.NoError(t, err)
			require.NoError(t, tc.repair())
			var unstamped int
			require.NoError(t, ps.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE automation_audit_hash IS NULL`).Scan(&unstamped))
			assert.Zero(t, unstamped)
			terms, err := db.ParseTrendTerms([]string{"hello"})
			require.NoError(t, err)
			filter := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
			cacheDir := t.TempDir()
			store, err := NewStore(ctx, pgURL, "agentsview", true)
			require.NoError(t, err)
			require.NoError(t, store.ConfigureUsageCache(ctx, cacheDir, pgURL, "agentsview"))
			require.True(t, store.trendsDisk.Enabled())
			require.NotEmpty(t, store.trendsIdentity)
			var reads [][]string
			reset := db.SetTrendPartsReadObserver(func(ids []string) { reads = append(reads, append([]string(nil), ids...)) })
			t.Cleanup(reset)
			report, err := store.GetTrendsTerms(ctx, filter, terms, "day")
			require.NoError(t, err)
			require.Len(t, reads, 1)
			assert.ElementsMatch(t, []string{"one", "two"}, reads[0])
			assert.Equal(t, 2, report.MessageCount)
			require.Len(t, report.Series, 1)
			assert.Equal(t, 2, report.Series[0].Total)
			require.NoError(t, store.Close())
			reads = nil
			require.NoError(t, tc.repair())
			store, err = NewStore(ctx, pgURL, "agentsview", true)
			require.NoError(t, err)
			defer store.Close()
			require.NoError(t, store.ConfigureUsageCache(ctx, cacheDir, pgURL, "agentsview"))
			require.True(t, store.trendsDisk.Enabled())
			require.NotEmpty(t, store.trendsIdentity)
			after, err := store.GetTrendsTerms(ctx, filter, terms, "day")
			require.NoError(t, err)
			assert.Equal(t, report, after)
			assert.Empty(t, reads)
		})
	}
}
