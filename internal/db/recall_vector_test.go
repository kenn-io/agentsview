package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanRecallEmbeddingUnitsIncludesCompleteServedCorpus(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	for _, entry := range []RecallEntry{
		{
			ID: "active-entry", Type: "fact", Scope: "project", Status: "accepted",
			Title: "Database pool", Body: "Reuse idle connections.",
			Trigger:         "connection storm",
			SourceSessionID: "s1", SourceRunID: "extract-fp-active",
		},
		{
			ID: "other-generation", Type: "fact", Scope: "project", Status: "accepted",
			Title: "Old pool", Body: "This belongs to an older corpus.",
			SourceSessionID: "s1", SourceRunID: "extract-fp-old",
		},
		{
			ID: "human-import", Type: "fact", Scope: "global", Status: "accepted",
			ReviewState: "human_reviewed",
			Title:       "Imported policy", Body: "This entry has no extraction run.",
			SourceSessionID: "s1",
		},
		{
			ID: "archived-entry", Type: "fact", Scope: "project", Status: "archived",
			Title: "Archived pool", Body: "This is no longer served.",
			SourceSessionID: "s1", SourceRunID: "extract-fp-active",
		},
	} {
		_, err := d.InsertRecallEntry(entry)
		require.NoError(t, err)
	}

	var units []EmbeddableUnit
	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "",
		func(unit EmbeddableUnit) error {
			units = append(units, unit)
			return nil
		},
	)

	require.NoError(t, err)
	require.Len(t, units, 3)
	assert.Equal(t,
		[]string{"active-entry", "human-import", "other-generation"},
		[]string{units[0].SessionID, units[1].SessionID, units[2].SessionID},
	)
	assert.Equal(t, "active-entry", units[0].SourceUUID)
	assert.Equal(t, "user", units[0].Kind)
	assert.Equal(t,
		"Database pool\n\nReuse idle connections.\n\nconnection storm",
		units[0].Content)
	assert.NotEmpty(t, watermark)
}

func TestScanRecallEmbeddingUnitsFullSupportsArchiveWithoutDeletionJournal(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "legacy-entry", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Legacy archive", Body: "Still available.", SourceSessionID: "s1",
	})
	require.NoError(t, err)
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		for _, trigger := range []string{
			"trg_recall_embedding_deletion",
			"trg_recall_embedding_reinsert",
			"trg_recall_corpus_insert",
			"trg_recall_corpus_update",
			"trg_recall_corpus_delete",
		} {
			if _, err := tx.Exec("DROP TRIGGER " + trigger); err != nil {
				return err
			}
		}
		_, err := tx.Exec("DROP TABLE recall_embedding_deletions")
		if err != nil {
			return err
		}
		if _, err = tx.Exec("DROP TABLE recall_embedding_changes"); err != nil {
			return err
		}
		_, err = tx.Exec("DROP TABLE recall_corpus_state")
		return err
	}))

	var ids []string
	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "",
		func(unit EmbeddableUnit) error {
			ids = append(ids, unit.SessionID)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"legacy-entry"}, ids)
	assert.NotEmpty(t, watermark)
	revision, err := d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(revision, "watermark-v1:"))
}

func TestRecallCorpusRevisionTracksServedEntryMutations(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")

	revision, err := d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:0", revision)

	_, err = d.InsertRecallEntry(RecallEntry{
		ID: "entry-1", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Initial", Body: "Body", SourceSessionID: "s1",
	})
	require.NoError(t, err)
	revision, err = d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:1", revision)

	_, err = d.getWriter().Exec(`
		UPDATE recall_entries SET updated_at = '2026-01-01T00:00:00Z'
		WHERE id = 'entry-1'`)
	require.NoError(t, err)
	revision, err = d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:1", revision,
		"non-embedded metadata does not require a vector refresh")

	_, err = d.getWriter().Exec(`
		UPDATE recall_entries SET title = 'Edited' WHERE id = 'entry-1'`)
	require.NoError(t, err)
	revision, err = d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:2", revision)

	_, err = d.getWriter().Exec("DELETE FROM recall_entries WHERE id = 'entry-1'")
	require.NoError(t, err)
	revision, err = d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:3", revision)
}

func TestRecallCorpusRevisionIgnoresUnservedEntryMutations(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")

	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "staging-entry", Type: "fact", Scope: "project", Status: "archived",
		Title: "Draft", Body: "Not served", SourceSessionID: "s1",
	})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		UPDATE recall_entries SET title = 'Edited draft' WHERE id = 'staging-entry'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`DELETE FROM recall_entries WHERE id = 'staging-entry'`)
	require.NoError(t, err)

	revision, err := d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:0", revision,
		"archived staging changes do not alter the served embedding corpus")
}

func TestRecallCorpusRevisionTracksServedStatusBoundary(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "promoted-entry", Type: "fact", Scope: "project", Status: "archived",
		Title: "Draft", Body: "Promote later", SourceSessionID: "s1",
	})
	require.NoError(t, err)

	_, err = d.getWriter().Exec(`
		UPDATE recall_entries SET status = 'accepted' WHERE id = 'promoted-entry'`)
	require.NoError(t, err)
	revision, err := d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:1", revision)

	_, err = d.getWriter().Exec(`
		UPDATE recall_entries SET status = 'archived' WHERE id = 'promoted-entry'`)
	require.NoError(t, err)
	revision, err = d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:2", revision)

	_, err = d.getWriter().Exec(`DELETE FROM recall_entries WHERE id = 'promoted-entry'`)
	require.NoError(t, err)
	revision, err = d.RecallCorpusRevision(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "counter-v1:2", revision,
		"deleting an already-unserved entry does not advance the corpus")
}

func TestScanRecallEmbeddingUnitsFindsBackdatedMutationByRevision(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "backdated", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Initial", Body: "Body", SourceSessionID: "s1",
	})
	require.NoError(t, err)

	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "", func(EmbeddableUnit) error { return nil },
	)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(watermark, "counter-v1:"))

	_, err = d.getWriter().Exec(`
		UPDATE recall_entries
		SET title = 'Backdated edit', updated_at = '2000-01-01T00:00:00Z'
		WHERE id = 'backdated'`)
	require.NoError(t, err)

	var units []EmbeddableUnit
	next, err := d.ScanRecallEmbeddingUnits(
		context.Background(), watermark,
		func(unit EmbeddableUnit) error {
			units = append(units, unit)
			return nil
		},
	)

	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, "backdated", units[0].SessionID)
	assert.Contains(t, units[0].Content, "Backdated edit")
	assert.NotEqual(t, watermark, next)
}

func TestRecallEmbeddingIncrementalPlanStaysBoundedAcrossCorpusSizes(t *testing.T) {
	var plans []string
	for _, size := range []int{10, 5000} {
		t.Run(fmt.Sprintf("entries_%d", size), func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "agentsview")
			tx, err := d.getWriter().Begin()
			require.NoError(t, err)
			stmt, err := tx.Prepare(`
				INSERT INTO recall_entries
					(id, type, scope, status, title, body, source_session_id, updated_at)
				VALUES (?, 'fact', 'project', 'accepted', 'title', 'body', 's1', ?)`)
			require.NoError(t, err)
			for i := range size {
				updatedAt := "2026-01-01T00:00:00.000Z"
				if i == size-1 {
					updatedAt = "2026-03-01T00:00:00.000Z"
				}
				_, err = stmt.Exec(fmt.Sprintf("entry-%05d", i), updatedAt)
				require.NoError(t, err)
			}
			require.NoError(t, stmt.Close())
			require.NoError(t, tx.Commit())
			_, err = d.getWriter().Exec("ANALYZE recall_embedding_changes")
			require.NoError(t, err)

			query, args := recallEmbeddingRevisionScanSQL(int64(size-1), false)
			rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(t, err)
			var plan strings.Builder
			for rows.Next() {
				var id, parent, notUsed int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
				plan.WriteString(detail)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Contains(t, plan.String(), "idx_recall_embedding_changes_revision")
			assert.Contains(t, plan.String(), "revision>?")
			plans = append(plans, plan.String())
		})
	}
	require.Len(t, plans, 2)
	assert.Equal(t, plans[0], plans[1],
		"incremental query shape must not degrade as old corpus cardinality grows")
}

func TestScanRecallEmbeddingUnitsHonorsUpdatedWatermark(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	for _, id := range []string{"older", "newer"} {
		_, err := d.InsertRecallEntry(RecallEntry{
			ID: id, Type: "fact", Scope: "project", Status: "accepted",
			Title: id, Body: "body", SourceSessionID: "s1",
			SourceRunID: "extract-fp",
		})
		require.NoError(t, err)
	}
	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "", func(EmbeddableUnit) error { return nil },
	)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		UPDATE recall_entries
		SET title = 'changed', updated_at = '2000-01-01T00:00:00Z'
		WHERE id = 'newer'`)
	require.NoError(t, err)

	var ids []string
	next, err := d.ScanRecallEmbeddingUnits(
		context.Background(), watermark,
		func(unit EmbeddableUnit) error {
			ids = append(ids, unit.SessionID)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"newer"}, ids)
	assert.NotEqual(t, watermark, next)
}

func TestScanRecallEmbeddingUnitsFullWatermarkSkipsOlderChangesIncrementally(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	for _, entry := range []RecallEntry{
		{
			ID: "accepted-old", Type: "fact", Scope: "project", Status: "accepted",
			Title: "Served entry", Body: "Still available.", SourceSessionID: "s1",
		},
		{
			ID: "archived-newer", Type: "fact", Scope: "project", Status: "archived",
			Title: "Archived entry", Body: "No longer available.", SourceSessionID: "s1",
		},
		{
			ID: "deleted-newest", Type: "fact", Scope: "project", Status: "accepted",
			Title: "Deleted entry", Body: "Removed permanently.", SourceSessionID: "s1",
		},
	} {
		_, err := d.InsertRecallEntry(entry)
		require.NoError(t, err)
	}
	_, err := d.getWriter().Exec(`
		UPDATE recall_entries SET updated_at = CASE id
			WHEN 'accepted-old' THEN '2026-01-01T00:00:00.000Z'
			WHEN 'archived-newer' THEN '2026-02-01T00:00:00.000Z'
			WHEN 'deleted-newest' THEN '2026-03-01T00:00:00.000Z'
		END`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec("DELETE FROM recall_entries WHERE id = 'deleted-newest'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		UPDATE recall_embedding_deletions
		SET deleted_at = '2026-03-01T00:00:00.000Z'
		WHERE entry_id = 'deleted-newest'`)
	require.NoError(t, err)

	var fullIDs []string
	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "",
		func(unit EmbeddableUnit) error {
			fullIDs = append(fullIDs, unit.SessionID)
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"accepted-old"}, fullIDs)
	assert.True(t, strings.HasPrefix(watermark, recallCorpusRevisionPrefix))

	var incrementalIDs []string
	_, err = d.ScanRecallEmbeddingUnits(
		context.Background(), watermark,
		func(unit EmbeddableUnit) error {
			incrementalIDs = append(incrementalIDs, unit.SessionID)
			return nil
		},
	)
	require.NoError(t, err)
	assert.Empty(t, incrementalIDs,
		"the full-scan watermark must not replay older archived changes")
}

func TestScanRecallEmbeddingUnitsEmitsChangedArchivedEntryAsTombstone(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "archived", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Old entry", Body: "Initially served.", SourceSessionID: "s1",
	})
	require.NoError(t, err)
	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "", func(EmbeddableUnit) error { return nil },
	)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		UPDATE recall_entries
		SET status = 'archived', updated_at = '2000-01-01T00:00:00.000Z'
		WHERE id = 'archived'`)
	require.NoError(t, err)

	var units []EmbeddableUnit
	next, err := d.ScanRecallEmbeddingUnits(
		context.Background(), watermark,
		func(unit EmbeddableUnit) error {
			units = append(units, unit)
			return nil
		},
	)

	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, "archived", units[0].SessionID)
	assert.True(t, units[0].Deleted)
	assert.NotEqual(t, watermark, next)
}

func TestScanRecallEmbeddingUnitsTracksDeleteAndReinsert(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	_, err := d.InsertRecallEntry(RecallEntry{
		ID: "retracted", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Private entry", Body: "Must leave semantic search.",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	watermark, err := d.ScanRecallEmbeddingUnits(
		context.Background(), "", func(EmbeddableUnit) error { return nil },
	)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"DELETE FROM recall_entries WHERE id = 'retracted'")
	require.NoError(t, err)

	var units []EmbeddableUnit
	watermark, err = d.ScanRecallEmbeddingUnits(
		context.Background(), watermark,
		func(unit EmbeddableUnit) error {
			units = append(units, unit)
			return nil
		},
	)

	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, "retracted", units[0].SessionID)
	assert.True(t, units[0].Deleted)

	_, err = d.InsertRecallEntry(RecallEntry{
		ID: "retracted", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Restored entry", Body: "This identity serves again.",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	units = nil
	_, err = d.ScanRecallEmbeddingUnits(
		context.Background(), watermark,
		func(unit EmbeddableUnit) error {
			units = append(units, unit)
			return nil
		},
	)

	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, "Restored entry\n\nThis identity serves again.", units[0].Content)
	assert.False(t, units[0].Deleted)
}
