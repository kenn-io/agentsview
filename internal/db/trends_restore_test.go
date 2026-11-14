package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestTrendsSQLiteRestoreAndResync(t *testing.T) {
	for _, mode := range []string{"replace", "append"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			path := filepath.Join(t.TempDir(), "archive.db")
			open := func() *db.DB {
				d, err := db.Open(t.Context(), path)
				require.NoError(t, err)
				return d
			}
			seeded := open()
			dbtest.SeedTrendsWindow(t, seeded)
			identity, err := seeded.GetDatabaseID(t.Context())
			require.NoError(t, err)
			require.NoError(t, seeded.Close())
			backup, err := os.ReadFile(path)
			require.NoError(t, err)
			terms, err := db.ParseTrendTerms([]string{"fallback"})
			require.NoError(t, err)
			filter := db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}
			write := func(content string) string {
				d := open()
				message := db.Message{SessionID: "trends-window-fallback", Role: "user", Content: content, ContentLength: len(content)}
				if mode == "append" {
					message.Ordinal = 1
					require.NoError(t, d.InsertMessages(t.Context(), []db.Message{message}))
				} else {
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), message.SessionID, []db.Message{message}))
				}
				revision, err := d.TranscriptRevision(t.Context(), message.SessionID)
				require.NoError(t, err)
				require.NoError(t, d.Close())
				return revision
			}
			read := func() int {
				d := open()
				got, err := d.GetTrendsTerms(t.Context(), filter, terms, "day")
				require.NoError(t, err)
				require.NoError(t, d.Close())
				return got.Series[0].Total
			}
			firstRevision := write("fallback fallback")
			first, second := 2, 0
			if mode == "append" {
				first, second = 3, 1
			}
			require.Equal(t, first, read())
			require.Equal(t, first, read())
			require.NoError(t, os.WriteFile(path, backup, 0600))
			require.Equal(t, firstRevision, write("orange"))
			d := open()
			gotIdentity, err := d.GetDatabaseID(t.Context())
			require.NoError(t, err)
			require.Equal(t, identity, gotIdentity)
			require.NoError(t, d.Close())
			require.Equal(t, second, read())
		})
	}
}
