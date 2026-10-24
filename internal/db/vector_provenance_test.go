package db

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestEmbeddingDataVersion(t *testing.T) {
	current := CurrentDataVersion()
	for _, tc := range []struct {
		name                                                 string
		archiveVersion, humanVersion, automatedVersion, want int
		includeAutomated, pending, tooNew                    bool
	}{
		{name: "excluded stale automation", archiveVersion: current, humanVersion: current, automatedVersion: current - 1, want: current},
		{name: "included mixed versions", archiveVersion: current, humanVersion: current, automatedVersion: current - 1, includeAutomated: true},
		{name: "uniform included source", archiveVersion: current, humanVersion: current, automatedVersion: current, includeAutomated: true, want: current},
		{name: "pending scoped upgrade", archiveVersion: current, humanVersion: current, automatedVersion: current, pending: true},
		{name: "legacy session", archiveVersion: current, automatedVersion: current},
		{name: "old uniform source", archiveVersion: current - 1, humanVersion: current - 1, automatedVersion: current - 1, includeAutomated: true, want: current - 1},
		{name: "old rows in advanced archive", archiveVersion: current, humanVersion: current - 1, automatedVersion: current - 1},
		{name: "future source", archiveVersion: current + 1, humanVersion: current, automatedVersion: current, tooNew: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "A", "proj")
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "B", Project: "proj", Agent: "claude", IsAutomated: true}))
			require.NoError(t, d.SetSessionDataVersion(t.Context(), "A", tc.humanVersion))
			require.NoError(t, d.SetSessionDataVersion(t.Context(), "B", tc.automatedVersion))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if tc.pending {
					if _, err := tx.ExecContext(t.Context(), "INSERT INTO stats(key,value) VALUES('scoped_upgrade_pending',1)"); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(t.Context(), "PRAGMA user_version="+strconv.Itoa(tc.archiveVersion))
				return err
			}))
			got, err := d.EmbeddingDataVersion(t.Context(), tc.includeAutomated)
			if tc.tooNew {
				var newer *DataVersionTooNewError
				require.ErrorAs(t, err, &newer)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// Readiness runs twice per incremental notification. Both version directions
// must seek within fixed automation partitions even when every row is ready or
// many stale automated sessions are outside the selected corpus.
func TestEmbeddingDataVersionBoundedProbe(t *testing.T) {
	for _, tc := range []struct {
		name             string
		staleAutomation  bool
		includeAutomated bool
	}{
		{name: "ready human"},
		{name: "ready all", includeAutomated: true},
		{name: "excluded stale automation", staleAutomation: true},
		{name: "included stale automation", staleAutomation: true, includeAutomated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var smallPlan []string
			for _, size := range []int{100, 10000} {
				t.Run(strconv.Itoa(size), func(t *testing.T) {
					d := testDB(t)
					current := CurrentDataVersion()
					automatedVersion := current
					if tc.staleAutomation {
						automatedVersion--
					}
					require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
       VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
       INSERT INTO sessions(id,project,is_automated,data_version,session_kind)
       SELECT 'session-'||x,'proj',x%2,CASE WHEN x%2=1 THEN ? ELSE ? END,
         CASE WHEN x%2=1 THEN ? ELSE '' END FROM n`, size, automatedVersion, current, parser.SessionKindRoborev)
						return err
					}))
					require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), "ANALYZE sessions")
						return err
					}))
					want := current
					if tc.staleAutomation && tc.includeAutomated {
						want = 0
					}
					got, err := d.EmbeddingDataVersion(t.Context(), tc.includeAutomated)
					require.NoError(t, err)
					assert.Equal(t, want, got)
					rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+embeddingReadinessQuery, tc.includeAutomated, current)
					require.NoError(t, err)
					defer rows.Close()
					details := explainQueryPlanDetails(t, rows)
					require.NoError(t, rows.Close())
					var searches []string
					for _, detail := range details {
						if strings.Contains(detail, "sessions") {
							assert.Contains(t, detail, "SEARCH sessions", "readiness must never scan the archive: %v", details)
							assert.Contains(t, detail, "is_automated=? AND data_version", "both keys must bound the probe: %v", details)
							searches = append(searches, detail)
						}
					}
					assert.Len(t, searches, 4, "both version directions need bounded seeks: %v", details)
					assert.Contains(t, strings.Join(searches, "\n"), "data_version<?")
					assert.Contains(t, strings.Join(searches, "\n"), "data_version>?")
					if size == 100 {
						smallPlan = searches
					} else {
						assert.Equal(t, smallPlan, searches, "archive growth must keep the same bounded seeks")
					}
					t.Logf("archive=%d scope=%t plan=%v", size, tc.includeAutomated, details)
					// Check both sides of the version boundary and live/deleted semantics.
					for _, version := range []int{current - 1, current + 1} {
						require.NoError(t, d.SetSessionDataVersion(t.Context(), "session-2", version))
						got, err = d.EmbeddingDataVersion(t.Context(), tc.includeAutomated)
						require.NoError(t, err)
						assert.Zero(t, got)
					}
					require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET deleted_at='2026-01-01T00:00:00Z' WHERE id='session-2'")
						return err
					}))
					got, err = d.EmbeddingDataVersion(t.Context(), tc.includeAutomated)
					require.NoError(t, err)
					assert.Equal(t, want, got)
				})
			}
		})
	}
}

func TestEmbeddingDataVersionLegacyReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "persistent archive")
	insertSession(t, d, "A", "proj")
	require.NoError(t, d.SetSessionDataVersion(t.Context(), "A", CurrentDataVersion()))
	// An archive created by the preceding binary has no readiness index.
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
          VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
          INSERT INTO sessions(id,project,is_automated,data_version,session_kind)
          SELECT 'session-'||x,'proj',x%2,?,
            CASE WHEN x%2=1 THEN ? ELSE '' END FROM n`, CurrentDataVersion(), parser.SessionKindRoborev)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), "DROP INDEX IF EXISTS idx_sessions_embedding_readiness")
		return err
	}))
	require.NoError(t, d.Close())
	ro, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	got, err := ro.EmbeddingDataVersion(t.Context(), true)
	require.NoError(t, err)
	assert.Equal(t, CurrentDataVersion(), got)
	require.NoError(t, ro.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, d.Close()) }()
	var automatedCount int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sessions WHERE is_automated=1").Scan(&automatedCount))
	assert.Equal(t, 5000, automatedCount, "writable reopen must preserve the automated partition")
	got, err = d.EmbeddingDataVersion(t.Context(), false)
	require.NoError(t, err)
	assert.Equal(t, CurrentDataVersion(), got)
	rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+embeddingReadinessQuery, false, CurrentDataVersion())
	require.NoError(t, err)
	defer rows.Close()
	details := explainQueryPlanDetails(t, rows)
	require.NoError(t, rows.Close())
	assert.Contains(t, strings.Join(details, "\n"), "idx_sessions_embedding_readiness")
}
