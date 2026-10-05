//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction/review"
)

// The same archive digested on SQLite and on its PostgreSQL hub produces
// identical Markdown and summary JSON.
func TestFrictionRunnerParitySQLiteAndPG(t *testing.T) {
	local, pgStore := openFrictionParityStores(t)
	now := func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	build := func(s review.Store) ([]byte, []byte) {
		r := &review.Runner{Store: s, Loc: time.UTC, Now: now, BackfillDays: 30}
		reps, err := r.CatchUp(t.Context())
		require.NoError(t, err)
		require.Equal(t, []string{"2026-09-15", "2026-09-16", "2026-09-17", "2026-09-18", "2026-09-19", "2026-09-20", "2026-09-21"},
			func() []string {
				out := []string{}
				for _, rep := range reps {
					out = append(out, rep.Date)
				}
				return out
			}())
		d, err := s.GetFrictionDigest(t.Context(), "2026-09-15")
		require.NoError(t, err)
		require.NotNil(t, d)
		return d.Markdown, d.SummaryJSON
	}
	sqliteMD, sqliteSummary := build(local)
	pgMD, pgSummary := build(pgStore)
	assert.Equal(t, string(sqliteMD), string(pgMD))
	assert.Equal(t, string(sqliteSummary), string(pgSummary))
	assert.Contains(t, string(pgMD), "`seat:seat-02`")
}
