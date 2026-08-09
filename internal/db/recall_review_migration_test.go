package db

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

func TestOpenMigratesLegacyRecallReviewConstraint(t *testing.T) {
	d := testDB(t)
	path := d.Path()
	insertSession(t, d, "review-migration-session", "agentsview")

	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "legacy-old", Type: "fact", Scope: "project",
		Status:      corerecall.StatusArchived,
		ReviewState: corerecall.ReviewStateHumanReviewed,
		Title:       "Old preserved entry", Body: "Preserved before replacement.",
		SourceSessionID:     "review-migration-session",
		SupersededByEntryID: "legacy-new",
	})
	require.NoError(t, err)
	_, err = d.InsertRecallEntry(RecallEntry{
		ID: "legacy-new", Type: "fact", Scope: "project",
		Status:      corerecall.StatusAccepted,
		ReviewState: corerecall.ReviewStateUnreviewedAuto,
		Title:       "Migration marker", Body: "preservedmarker remains searchable",
		SourceSessionID:   "review-migration-session",
		SupersedesEntryID: "legacy-old",
		ProvenanceOK:      true,
		Evidence: []RecallEvidence{{
			SessionID:           "review-migration-session",
			MessageStartOrdinal: 2,
			MessageEndOrdinal:   4,
			Snippet:             "preserved migration evidence",
		}},
	})
	require.NoError(t, err)

	wantRowIDs := make(map[string]int64, 2)
	for _, id := range []string{"legacy-old", "legacy-new"} {
		var rowID int64
		require.NoError(t, d.getReader().QueryRow(
			`SELECT rowid FROM recall_entries WHERE id = ?`, id,
		).Scan(&rowID))
		wantRowIDs[id] = rowID
	}

	conn, err := d.getWriter().Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	tx, err := conn.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`
		CREATE TABLE recall_entries_legacy_review_check (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			scope TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'accepted',
			review_state TEXT NOT NULL DEFAULT 'unreviewed_auto'
				CHECK (review_state IN (
					'human_reviewed', 'unreviewed_auto',
					'calibrated_auto', 'eval_raw'
				)),
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			trigger TEXT NOT NULL DEFAULT '',
			confidence REAL,
			uncertainty TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL DEFAULT '',
			cwd TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT '',
			agent TEXT NOT NULL DEFAULT '',
			source_session_id TEXT NOT NULL
				REFERENCES sessions(id) ON DELETE CASCADE,
			source_episode_id TEXT NOT NULL DEFAULT '',
			source_run_id TEXT NOT NULL DEFAULT '',
			extractor_method TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			transferable INTEGER NOT NULL DEFAULT 0,
			provenance_ok INTEGER NOT NULL DEFAULT 0,
			supersedes_entry_id TEXT NOT NULL DEFAULT '',
			superseded_by_entry_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
				DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL
				DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
		INSERT INTO recall_entries_legacy_review_check (
			rowid, ` + recallBaseCols + `
		) SELECT rowid, ` + recallBaseCols + ` FROM recall_entries;
		DROP TABLE recall_entries;
		ALTER TABLE recall_entries_legacy_review_check RENAME TO recall_entries;
	`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, d.Close())

	for pass := 0; pass < 2; pass++ {
		reopened, err := Open(path)
		require.NoError(t, err)

		for id, wantRowID := range wantRowIDs {
			var gotRowID int64
			require.NoError(t, reopened.getReader().QueryRow(
				`SELECT rowid FROM recall_entries WHERE id = ?`, id,
			).Scan(&gotRowID))
			assert.Equal(t, wantRowID, gotRowID)
		}

		got, err := reopened.GetRecallEntry(context.Background(), "legacy-new")
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Len(t, got.Evidence, 1)
		assert.Equal(t, "legacy-old", got.SupersedesEntryID)
		assert.Equal(t, "preserved migration evidence", got.Evidence[0].Snippet)

		matches, err := reopened.QueryRecallEntries(context.Background(), RecallQuery{
			Text:  "preservedmarker",
			Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, matches.RecallEntries, 1)
		assert.Equal(t, "legacy-new", matches.RecallEntries[0].ID)

		var tableSQL string
		require.NoError(t, reopened.getReader().QueryRow(`
			SELECT sql FROM sqlite_master
			WHERE type = 'table' AND name = 'recall_entries'
		`).Scan(&tableSQL))
		assert.NotContains(t, tableSQL, "CHECK (review_state IN")

		rows, err := reopened.getReader().Query(`PRAGMA foreign_key_check`)
		require.NoError(t, err)
		assert.False(t, rows.Next())
		require.NoError(t, rows.Close())

		if pass == 0 {
			_, err = reopened.InsertRecallEntry(RecallEntry{
				ID: "rejected", Type: "fact", Scope: "project",
				Status:      corerecall.StatusArchived,
				ReviewState: corerecall.ReviewStateHumanRejected,
				Title:       "Rejected", Body: "Rejected after review.",
				SourceSessionID: "review-migration-session",
			})
			require.NoError(t, err)
		}
		require.NoError(t, reopened.Close())
	}

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	got, err := reopened.GetRecallEntry(context.Background(), "rejected")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, corerecall.ReviewStateHumanRejected, got.ReviewState)
	assert.True(t, strings.Contains(got.Body, "Rejected"))
}
