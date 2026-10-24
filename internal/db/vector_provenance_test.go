package db

import (
	"database/sql"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
