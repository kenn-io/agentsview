package db

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecallQueryRevisionTracksEntryAndEvidenceMutations(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "query-revision-session", "project-a")

	revision, err := d.RecallQueryRevision(ctx)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(revision, recallQueryRevisionPrefix))

	_, err = d.InsertRecallEntry(RecallEntry{
		ID:              "query-revision-entry",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Revision tracking",
		Body:            "Ranked pages must stay on one corpus.",
		Project:         "project-a",
		SourceSessionID: "query-revision-session",
		ProvenanceOK:    true,
		Evidence: []RecallEvidence{{
			SessionID:           "query-revision-session",
			MessageStartOrdinal: 1,
			MessageEndOrdinal:   2,
			Snippet:             "Original evidence.",
		}},
	})
	require.NoError(t, err)
	revision = requireRecallQueryRevisionChanged(t, d, revision)

	for _, update := range []string{
		`UPDATE recall_entries SET project = 'project-b'
			WHERE id = 'query-revision-entry'`,
		`UPDATE recall_entries SET review_state = 'human_reviewed'
			WHERE id = 'query-revision-entry'`,
		`UPDATE recall_entries SET provenance_ok = 0
			WHERE id = 'query-revision-entry'`,
		`UPDATE recall_evidence SET snippet = 'Changed evidence.'
			WHERE entry_id = 'query-revision-entry'`,
	} {
		_, err = d.getWriter().Exec(update)
		require.NoError(t, err)
		revision = requireRecallQueryRevisionChanged(t, d, revision)
	}

	_, err = d.getWriter().Exec(
		`DELETE FROM recall_evidence WHERE entry_id = 'query-revision-entry'`,
	)
	require.NoError(t, err)
	revision = requireRecallQueryRevisionChanged(t, d, revision)

	_, err = d.getWriter().Exec(
		`DELETE FROM recall_entries WHERE id = 'query-revision-entry'`,
	)
	require.NoError(t, err)
	requireRecallQueryRevisionChanged(t, d, revision)
}

func requireRecallQueryRevisionChanged(
	t *testing.T,
	d *DB,
	previous string,
) string {
	t.Helper()
	revision, err := d.RecallQueryRevision(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, previous, revision)
	return revision
}
