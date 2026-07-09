package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoriesSchemaIndexesSourceEpisode(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_memories_source_episode'`,
	).Scan(&count)

	require.NoError(t, err, "query memory source episode index")
	assert.Equal(t, 1, count)
}

func TestOpenRepairsMissingMemorySourceEpisodeIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	require.NoError(t, err, "initial open")
	d.Close()

	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "raw open")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_memories_source_episode`)
	require.NoError(t, err, "drop source episode index")
	var count int
	err = conn.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_memories_source_episode'`,
	).Scan(&count)
	require.NoError(t, err, "verify source episode index removed")
	require.Equal(t, 0, count)
	conn.Close()

	reopened, err := Open(path)
	require.NoError(t, err, "reopen after dropping index")
	defer reopened.Close()

	err = reopened.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_memories_source_episode'`,
	).Scan(&count)
	require.NoError(t, err, "verify source episode index restored")
	assert.Equal(t, 1, count)
}

func TestOpenCreatesSearchableMemoryFTSWhenRuntimeSupportsFTS4(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() && !sqliteRuntimeSupportsFTS4(t, d) {
		t.Skip("no FTS4 or FTS5 support")
	}
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "m1",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Portal filter finding",
		Body:            "The decisive clue was heliotrope parser overflow.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)

	var count int
	err = d.getReader().QueryRowContext(
		ctx,
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH ?`,
		"heliotrope",
	).Scan(&count)

	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestOpenCreatesSearchableMemoryEvidenceFTSWhenRuntimeSupportsFTS4(
	t *testing.T,
) {
	d := testDB(t)
	if !d.HasFTS() && !sqliteRuntimeSupportsFTS4(t, d) {
		t.Skip("no FTS4 or FTS5 support")
	}
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "m1",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Portal filter finding",
		Body:            "The dropdown was inspected.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
		Evidence: []MemoryEvidence{
			{
				SessionID:           "s1",
				MessageStartOrdinal: 3,
				MessageEndOrdinal:   3,
				Snippet:             "The decisive clue was heliotrope parser overflow.",
			},
		},
	})
	require.NoError(t, err)

	var count int
	err = d.getReader().QueryRowContext(
		ctx,
		`SELECT count(*) FROM memory_evidence_fts
		 WHERE memory_evidence_fts MATCH ?`,
		"heliotrope",
	).Scan(&count)

	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func sqliteRuntimeSupportsFTS4(t *testing.T, d *DB) bool {
	t.Helper()
	_, err := d.getWriter().Exec(
		`CREATE VIRTUAL TABLE temp.memory_fts4_probe USING fts4(value)`,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such module") {
			return false
		}
		require.NoError(t, err, "probe fts4 support")
	}
	_, err = d.getWriter().Exec(`DROP TABLE temp.memory_fts4_probe`)
	require.NoError(t, err, "drop fts4 probe table")
	return true
}

func requireMemoryFTS(t *testing.T, d *DB) {
	t.Helper()
	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'table' AND name = 'memories_fts'`,
	).Scan(&count)
	require.NoError(t, err, "query memory fts table")
	if count == 0 {
		t.Skip("no memory FTS support")
	}
	_, err = d.getReader().Exec(`SELECT 1 FROM memories_fts LIMIT 1`)
	if err != nil {
		t.Skipf("no memory FTS support: %v", err)
	}
}

func requireMemoryFTS4(t *testing.T, d *DB) {
	t.Helper()
	var ddl string
	err := d.getReader().QueryRow(
		`SELECT lower(sql) FROM sqlite_master
		 WHERE type = 'table' AND name = 'memories_fts'`,
	).Scan(&ddl)
	require.NoError(t, err, "query memory fts ddl")
	if !strings.Contains(ddl, "using fts4") {
		t.Skip("memory FTS table is not FTS4")
	}
}

func requireMemoryFTS5(t *testing.T, d *DB) {
	t.Helper()
	var ddl string
	err := d.getReader().QueryRow(
		`SELECT lower(sql) FROM sqlite_master
		 WHERE type = 'table' AND name = 'memories_fts'`,
	).Scan(&ddl)
	require.NoError(t, err, "query memory fts ddl")
	if !strings.Contains(ddl, "using fts5") {
		t.Skip("memory FTS table is not FTS5")
	}
}

func TestMemoriesInsertGetAndQuery(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo/agentsview"
		s.GitBranch = "main"
	})
	insertSession(t, d, "s2", "other", func(s *Session) {
		s.Agent = "codex"
	})

	_, err := d.InsertMemory(Memory{
		ID:              "m1",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Check cwd before file reads",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		CWD:             "/repo/agentsview",
		GitBranch:       "main",
		Agent:           "codex",
		SourceSessionID: "s1",
		Transferable:    true,
		ProvenanceOK:    true,
		Evidence: []MemoryEvidence{
			{
				SessionID:           "s1",
				MessageStartOrdinal: 3,
				MessageEndOrdinal:   7,
				ToolUseID:           "toolu_1",
				Snippet:             "Verify cwd before retrying wal_checkpoint",
			},
		},
	})
	require.NoError(t, err, "InsertMemory")

	_, err = d.InsertMemory(Memory{
		ID:              "m2",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Other project note",
		Body:            "Unrelated note.",
		Project:         "other",
		Agent:           "codex",
		SourceSessionID: "s2",
	})
	require.NoError(t, err, "InsertMemory other")

	got, err := d.GetMemory(ctx, "m1")
	require.NoError(t, err, "GetMemory")
	require.NotNil(t, got, "memory")
	assert.Equal(t, "Check cwd before file reads", got.Title)
	assert.True(t, got.Transferable)
	assert.True(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "s1", got.Evidence[0].SessionID)
	assert.Equal(t, 3, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "cwd reads",
		Project: "agentsview",
		Agent:   "codex",
		Limit:   10,
	})
	require.NoError(t, err, "QueryMemories")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "m1", page.Memories[0].ID)
	assert.Greater(t, page.Memories[0].Score, 0.0)
	assert.Equal(t, 2, page.Memories[0].ScoreBreakdown.KeywordOverlap)
	assert.Greater(t, page.Memories[0].ScoreBreakdown.KeywordIDFScore, 0.0)
	assert.Equal(t, page.Memories[0].Score, page.Memories[0].ScoreBreakdown.Total)
	assert.Equal(t, []string{"keyword", "evidence"}, page.Memories[0].MatchReasons)

	page, err = d.QueryMemories(ctx, MemoryQuery{
		Text:    "wal_checkpoint",
		Project: "agentsview",
		Agent:   "codex",
		Limit:   10,
	})
	require.NoError(t, err, "QueryMemories evidence")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "m1", page.Memories[0].ID)
	assert.Equal(t, 1, page.Memories[0].ScoreBreakdown.EvidenceKeywordOverlap)
	assert.Greater(t, page.Memories[0].ScoreBreakdown.EvidenceIDFScore, 0.0)
	assert.Greater(t, page.Memories[0].ScoreBreakdown.IdentifierBoost, 0.0)
	assert.Equal(t, []string{"evidence", "identifier"}, page.Memories[0].MatchReasons)
}

func TestQueryMemoriesFiltersTrustedOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	for _, memory := range []Memory{
		{
			ID:              "trusted",
			Type:            "procedure",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Trusted cwd memory",
			Body:            "Recover from wrong cwd before reading files.",
			Project:         "agentsview",
			Agent:           "codex",
			SourceSessionID: "s1",
			Transferable:    true,
			ProvenanceOK:    true,
		},
		{
			ID:              "not-transferable",
			Type:            "procedure",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Local cwd memory",
			Body:            "Recover from wrong cwd before reading files.",
			Project:         "agentsview",
			Agent:           "codex",
			SourceSessionID: "s1",
			Transferable:    false,
			ProvenanceOK:    true,
		},
		{
			ID:              "unverified-provenance",
			Type:            "procedure",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Unverified cwd memory",
			Body:            "Recover from wrong cwd before reading files.",
			Project:         "agentsview",
			Agent:           "codex",
			SourceSessionID: "s1",
			Transferable:    true,
			ProvenanceOK:    false,
		},
	} {
		_, err := d.InsertMemory(memory)
		require.NoError(t, err, "InsertMemory %s", memory.ID)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:        "wrong cwd files",
		Project:     "agentsview",
		Agent:       "codex",
		TrustedOnly: true,
		Limit:       10,
	})

	require.NoError(t, err, "QueryMemories")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "trusted", page.Memories[0].ID)
}

func TestQueryMemoriesFiltersByExtractorMethod(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	insertSession(t, d, "s2", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})

	_, err := d.InsertMemory(Memory{
		ID:              "raw",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Raw trajectory",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
		ExtractorMethod: "session-transcript-import",
	})
	require.NoError(t, err, "InsertMemory raw")
	_, err = d.InsertMemory(Memory{
		ID:              "extracted",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Extracted trajectory",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s2",
		ExtractorMethod: "memory-probe-single-call",
	})
	require.NoError(t, err, "InsertMemory extracted")

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:            "wrong cwd files",
		Project:         "test-agent",
		Agent:           "test-agent",
		ExtractorMethod: "session-transcript-import",
		Limit:           10,
	})

	require.NoError(t, err, "QueryMemories")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "raw", page.Memories[0].ID)
}

func TestQueryMemoriesTrimsExactMatchFilters(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo/agentsview"
		s.GitBranch = "main"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "m1",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Check cwd before file reads",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		CWD:             "/repo/agentsview",
		GitBranch:       "main",
		Agent:           "codex",
		SourceSessionID: "s1",
		SourceRunID:     "run1",
		ExtractorMethod: "single",
	})
	require.NoError(t, err)

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:            "cwd reads",
		Project:         " agentsview ",
		CWD:             " /repo/agentsview ",
		GitBranch:       " main ",
		Agent:           " codex ",
		Type:            " procedure ",
		Scope:           " project ",
		Status:          " accepted ",
		ExtractorMethod: " single ",
		SourceSessionID: " s1 ",
		SourceRunID:     " run1 ",
		Limit:           10,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "m1", page.Memories[0].ID)
}

func TestQueryMemoriesTieBreaksByStableSourceEpisode(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	insertSession(t, d, "s2", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})

	for _, m := range []Memory{
		{
			ID:              "z-run-specific-id",
			Title:           "Raw chunk",
			Body:            "Shared tie token.",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
			SourceEpisodeID: "traj:chunk:0001",
			SourceRunID:     "run-a",
		},
		{
			ID:              "a-run-specific-id",
			Title:           "Raw chunk",
			Body:            "Shared tie token.",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s2",
			SourceEpisodeID: "traj:chunk:0002",
			SourceRunID:     "run-b",
		},
	} {
		_, err := d.InsertMemory(m)
		require.NoError(t, err)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "tie token",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   2,
	})
	require.NoError(t, err)
	require.Len(t, page.Memories, 2)
	assert.Equal(t, "traj:chunk:0001", page.Memories[0].SourceEpisodeID)
	assert.Equal(t, "traj:chunk:0002", page.Memories[1].SourceEpisodeID)
}

func TestQueryMemoriesCandidatePreselectionTieBreaksByStableSourceEpisode(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})

	for i := 0; i <= MaxMemoryLimit; i++ {
		id := fmt.Sprintf("m-%04d", MaxMemoryLimit-i)
		_, err := d.InsertMemory(Memory{
			ID:              id,
			Title:           "Raw chunk",
			Body:            "Shared candidate token.",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
			SourceEpisodeID: fmt.Sprintf("traj:chunk:%04d", i),
		})
		require.NoError(t, err)
		_, err = d.getWriter().Exec(
			"UPDATE memories SET updated_at = ? WHERE id = ?",
			fmt.Sprintf("2026-01-01T00:00:%04dZ", i),
			id,
		)
		require.NoError(t, err)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "candidate token",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   1,
	})
	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "traj:chunk:0000", page.Memories[0].SourceEpisodeID)
}

func TestQueryMemoriesWithoutTextUsesUpdatedListOrder(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	for _, memory := range []Memory{
		{
			ID:              "older-source-first",
			Title:           "Older memory",
			Body:            "Generic accepted memory.",
			Project:         "agentsview",
			Agent:           "codex",
			SourceSessionID: "s1",
			SourceEpisodeID: "a-source",
		},
		{
			ID:              "newer-source-second",
			Title:           "Newer memory",
			Body:            "Generic accepted memory.",
			Project:         "agentsview",
			Agent:           "codex",
			SourceSessionID: "s1",
			SourceEpisodeID: "z-source",
		},
	} {
		_, err := d.InsertMemory(memory)
		require.NoError(t, err)
	}
	_, err := d.getWriter().Exec(`
		UPDATE memories SET updated_at = CASE id
			WHEN 'older-source-first' THEN '2024-01-01T00:00:00Z'
			WHEN 'newer-source-second' THEN '2024-02-01T00:00:00Z'
			ELSE updated_at
		END
		WHERE id IN ('older-source-first', 'newer-source-second')`)
	require.NoError(t, err)

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Project: "agentsview",
		Agent:   "codex",
		Limit:   2,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 2)
	assert.Equal(t, "newer-source-second", page.Memories[0].ID)
	assert.Equal(t, "older-source-first", page.Memories[1].ID)
}

func TestQueryMemoriesFiltersBySourceRunID(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	insertSession(t, d, "s2", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})

	_, err := d.InsertMemory(Memory{
		ID:              "run-a",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Raw trajectory run A",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
		SourceRunID:     "smoke-a",
	})
	require.NoError(t, err, "InsertMemory run a")
	_, err = d.InsertMemory(Memory{
		ID:              "run-b",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Raw trajectory run B",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s2",
		SourceRunID:     "smoke-b",
	})
	require.NoError(t, err, "InsertMemory run b")

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:        "wrong cwd files",
		Project:     "test-agent",
		Agent:       "test-agent",
		SourceRunID: "smoke-a",
		Limit:       10,
	})

	require.NoError(t, err, "QueryMemories")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "run-a", page.Memories[0].ID)
}

func TestQueryMemoriesFiltersBySourceSessionID(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	insertSession(t, d, "s2", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	_, err := d.InsertMemory(Memory{
		ID:              "session-a",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Session A cwd lesson",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
	})
	require.NoError(t, err, "InsertMemory session a")
	_, err = d.InsertMemory(Memory{
		ID:              "session-b",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Session B cwd lesson",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s2",
	})
	require.NoError(t, err, "InsertMemory session b")

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:            "wrong cwd files",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
		Limit:           10,
	})

	require.NoError(t, err, "QueryMemories")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "session-a", page.Memories[0].ID)
}

func TestQueryMemoriesFiltersBySourceEpisodeID(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	_, err := d.InsertMemory(Memory{
		ID:              "episode-a",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Episode A cwd lesson",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
		SourceEpisodeID: "s1:chunk:0001",
	})
	require.NoError(t, err, "InsertMemory episode a")
	_, err = d.InsertMemory(Memory{
		ID:              "episode-b",
		Type:            "procedure",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Episode B cwd lesson",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
		SourceEpisodeID: "s1:chunk:0002",
	})
	require.NoError(t, err, "InsertMemory episode b")

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:            "wrong cwd files",
		Project:         "agentsview",
		Agent:           "codex",
		SourceEpisodeID: "s1:chunk:0001",
		Limit:           10,
	})

	require.NoError(t, err, "QueryMemories")
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "episode-a", page.Memories[0].ID)
}

func TestSupersedeMemoryArchivesOldAndLinksReplacement(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	insertSession(t, d, "s2", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "old",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Old retry policy",
		Body:            "Retry flaky command once before escalating.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)

	_, err = d.SupersedeMemory(ctx, "old", Memory{
		ID:              "new",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Current retry policy",
		Body:            "Retry flaky command three times before escalating.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s2",
	})
	require.NoError(t, err)

	oldMemory, err := d.GetMemory(ctx, "old")
	require.NoError(t, err)
	require.NotNil(t, oldMemory)
	assert.Equal(t, "archived", oldMemory.Status)
	assert.Equal(t, "new", oldMemory.SupersededByMemoryID)
	assert.Empty(t, oldMemory.SupersedesMemoryID)
	newMemory, err := d.GetMemory(ctx, "new")
	require.NoError(t, err)
	require.NotNil(t, newMemory)
	assert.Equal(t, "accepted", newMemory.Status)
	assert.Equal(t, "old", newMemory.SupersedesMemoryID)
	assert.Empty(t, newMemory.SupersededByMemoryID)
	replacements, err := d.ListMemories(ctx, MemoryQuery{
		SupersedesMemoryID: "old",
		Limit:              10,
	})
	require.NoError(t, err)
	require.Len(t, replacements, 1)
	assert.Equal(t, "new", replacements[0].ID)

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "retry flaky command",
		Project: "agentsview",
		Agent:   "codex",
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "new", page.Memories[0].ID)

	archived, err := d.ListMemories(ctx, MemoryQuery{
		Status: "archived",
		Limit:  10,
	})
	require.NoError(t, err)
	require.Len(t, archived, 1)
	assert.Equal(t, "old", archived[0].ID)
	archivedByReplacement, err := d.ListMemories(ctx, MemoryQuery{
		Status:               "archived",
		SupersededByMemoryID: "new",
		Limit:                10,
	})
	require.NoError(t, err)
	require.Len(t, archivedByReplacement, 1)
	assert.Equal(t, "old", archivedByReplacement[0].ID)
}

func TestSupersedeMemoryRejectsNonAcceptedReplacement(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview")
	insertSession(t, d, "s2", "agentsview")
	_, err := d.InsertMemory(Memory{
		ID:              "old",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Old retry policy",
		Body:            "Retry flaky command once before escalating.",
		Project:         "agentsview",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)

	_, err = d.SupersedeMemory(ctx, "old", Memory{
		ID:              "new",
		Type:            "fact",
		Scope:           "project",
		Status:          "archived",
		Title:           "Current retry policy",
		Body:            "Retry flaky command three times before escalating.",
		Project:         "agentsview",
		SourceSessionID: "s2",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "replacement memory status must be")
	oldMemory, err := d.GetMemory(ctx, "old")
	require.NoError(t, err)
	require.NotNil(t, oldMemory)
	assert.Equal(t, "accepted", oldMemory.Status)
}

func TestQueryMemoriesRanksBeyondRequestedResultLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "target",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Older target trajectory",
		Body:            "The decisive clue was quartz capacitor drift.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	for i := range 12 {
		_, err := d.InsertMemory(Memory{
			ID:              "filler-" + string(rune('a'+i)),
			Type:            "fact",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Generic filler trajectory",
			Body:            "Generic note without the decisive query terms.",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
		})
		require.NoError(t, err)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "quartz capacitor drift",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   1,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "target", page.Memories[0].ID)
}

func TestQueryMemoriesFindsTextMatchBeyondRecentCandidateCap(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "target",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Older target trajectory",
		Body:            "The decisive clue was heliotrope parser overflow.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	for i := range MaxMemoryLimit + 20 {
		_, err := d.InsertMemory(Memory{
			ID:              "filler-cap-" + testID(i),
			Type:            "fact",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Generic filler trajectory",
			Body:            "Generic note without the decisive clue.",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
		})
		require.NoError(t, err)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "heliotrope parser overflow",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   1,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "target", page.Memories[0].ID)
}

func TestQueryMemoriesIncludesEvidenceOnlyCandidateWhenOtherTermsMatchDirectText(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "target",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Menu investigation",
		Body:            "The dropdown was inspected.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
		Evidence: []MemoryEvidence{
			{
				SessionID:           "s1",
				MessageStartOrdinal: 1,
				MessageEndOrdinal:   1,
				Snippet:             "The hidden answer label was heliotrope overflow.",
			},
		},
	})
	require.NoError(t, err)
	_, err = d.InsertMemory(Memory{
		ID:              "direct-filler",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Portal filter labels",
		Body:            "The portal filter menu was inspected without the answer.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "portal heliotrope overflow",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   1,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "target", page.Memories[0].ID)
	assert.Equal(t, 2, page.Memories[0].ScoreBreakdown.EvidenceKeywordOverlap)
	assert.Greater(t, page.Memories[0].ScoreBreakdown.EvidenceIDFScore, 0.0)
}

func TestQueryMemoriesFindsEvidenceMatchBeyondRecentEvidenceCandidateCap(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "target",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Older evidence target",
		Body:            "The dropdown was inspected.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
		Evidence: []MemoryEvidence{
			{
				SessionID:           "s1",
				MessageStartOrdinal: 1,
				MessageEndOrdinal:   1,
				Snippet:             "The hidden answer label was heliotrope parser overflow.",
			},
		},
	})
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx,
		"UPDATE memories SET updated_at = '2024-01-01T00:00:00Z' WHERE id = 'target'")
	require.NoError(t, err)
	for i := range MaxMemoryLimit + 20 {
		id := "evidence-filler-" + testID(i)
		_, err := d.InsertMemory(Memory{
			ID:              id,
			Type:            "fact",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Newer evidence filler",
			Body:            "The dropdown was inspected.",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
			Evidence: []MemoryEvidence{
				{
					SessionID:           "s1",
					MessageStartOrdinal: 1,
					MessageEndOrdinal:   1,
					Snippet:             "The partial evidence only mentioned heliotrope.",
				},
			},
		})
		require.NoError(t, err)
		_, err = d.getWriter().ExecContext(ctx,
			"UPDATE memories SET updated_at = '2024-02-01T00:00:00Z' WHERE id = ?",
			id)
		require.NoError(t, err)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "heliotrope parser overflow",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   1,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 1)
	assert.Equal(t, "target", page.Memories[0].ID)
	assert.Equal(t, 3, page.Memories[0].ScoreBreakdown.EvidenceKeywordOverlap)
}

func TestQueryMemoriesDiversifiesSourceEpisodesBeforeRepeatingChunks(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	for _, m := range []Memory{
		{
			ID:              "same-a",
			Type:            "fact",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Strong same-source chunk A",
			Body:            "urgent quartz capacitor drift",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
			SourceEpisodeID: "same-trajectory:chunk:0001",
		},
		{
			ID:              "same-b",
			Type:            "fact",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Strong same-source chunk B",
			Body:            "urgent quartz capacitor drift",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
			SourceEpisodeID: "same-trajectory:chunk:0002",
		},
		{
			ID:              "z-other",
			Type:            "fact",
			Scope:           "project",
			Status:          "accepted",
			Title:           "Other source chunk",
			Body:            "quartz capacitor drift",
			Project:         "test-agent",
			Agent:           "test-agent",
			SourceSessionID: "s1",
			SourceEpisodeID: "other-trajectory:chunk:0001",
		},
	} {
		_, err := d.InsertMemory(m)
		require.NoError(t, err)
	}

	page, err := d.QueryMemories(ctx, MemoryQuery{
		Text:    "urgent quartz capacitor drift",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   2,
	})

	require.NoError(t, err)
	require.Len(t, page.Memories, 2)
	assert.Equal(t, "same-a", page.Memories[0].ID)
	assert.Equal(t, "z-other", page.Memories[1].ID)
}

func TestListMemoryTextCandidatesOrdersByLexicalRank(t *testing.T) {
	d := testDB(t)
	requireMemoryFTS(t, d)
	requireMemoryFTS5(t, d)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "rich",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Older rich trajectory",
		Body:            "The decisive clue was heliotrope parser overflow.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	_, err = d.InsertMemory(Memory{
		ID:              "partial",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Newer partial trajectory",
		Body:            "The session mentioned heliotrope once.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `
		UPDATE memories SET updated_at = CASE id
			WHEN 'rich' THEN '2024-01-01T00:00:00Z'
			WHEN 'partial' THEN '2024-02-01T00:00:00Z'
			ELSE updated_at
		END
		WHERE id IN ('rich', 'partial')`)
	require.NoError(t, err)

	candidates, err := d.ListMemoryTextCandidates(ctx, MemoryQuery{
		Text:    "heliotrope parser overflow",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   2,
	})

	require.NoError(t, err)
	require.Len(t, candidates, 2)
	assert.Equal(t, "rich", candidates[0].ID)
	assert.Equal(t, "partial", candidates[1].ID)
}

func TestListMemoryTextCandidatesFallsBackToLikeForFTS4SubstringMatch(t *testing.T) {
	d := testDB(t)
	requireMemoryFTS(t, d)
	requireMemoryFTS4(t, d)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "substring-memory",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Portal substring clue",
		Body:            "The decisive clue was abcdefghij in the portal state.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)

	candidates, err := d.ListMemoryTextCandidates(ctx, MemoryQuery{
		Text:    "cdefg",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   10,
	})

	require.NoError(t, err)
	require.NotEmpty(t, candidates)
	assert.Equal(t, "substring-memory", candidates[0].ID)
}

func TestListMemoryTextCandidatesUsesFTS4RowIDMatchForDirectText(t *testing.T) {
	d := testDB(t)
	requireMemoryFTS(t, d)
	requireMemoryFTS4(t, d)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "fts4-direct-memory",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Portal menu finding",
		Body:            "The dropdown was inspected.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `
		UPDATE memories_fts
		SET body = 'The decisive clue was heliotrope parser overflow.'
		WHERE rowid = (SELECT rowid FROM memories WHERE id = ?)`,
		"fts4-direct-memory",
	)
	require.NoError(t, err)

	candidates, err := d.ListMemoryTextCandidates(ctx, MemoryQuery{
		Text:    "heliotrope parser overflow",
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   10,
	})

	require.NoError(t, err)
	require.NotEmpty(t, candidates)
	assert.Equal(t, "fts4-direct-memory", candidates[0].ID)
}

func TestMemoryEvidenceFTSKindDetectsFTS4(t *testing.T) {
	d := testDB(t)
	requireMemoryFTS4(t, d)

	assert.Equal(t, "fts4", d.memoryEvidenceFTSKind(context.Background()))
}

func TestMemoryQueryTermsRetainsShortCriticalUITerms(t *testing.T) {
	got := memoryQueryTerms(
		`I am working with our ServiceNow portal. On the Incidents list page, ` +
			`when I open the "Filters" dropdown, excluding "Edit personal filters" ` +
			`and "-- None --", which filter option labels contain the substring ` +
			`"Incident"? Mark your final answer (should be one or more short ` +
			`phrases) in \boxed{}.`,
	)

	assert.Contains(t, got, "incident")
	assert.Contains(t, got, "filters")
	assert.Contains(t, got, "portal")
	assert.NotContains(t, got, "final")
	assert.NotContains(t, got, "short")
	assert.NotContains(t, got, "more")
	assert.LessOrEqual(t, len(got), MaxMemorySearchTerms)
}

func TestMemoryQueryTermsDropsAnswerFormatBoilerplate(t *testing.T) {
	got := memoryQueryTerms(
		`I am working with our ServiceNow portal. My boss asked me to rebalance ` +
			`workload between several agents by reassigning problems with a specific ` +
			`tag. What are the two modules that our company's workflow typically use ` +
			`in order to accomplish this task? Tell me the first module's name and ` +
			`the second module's name in order, separated by a semicolon. Mark your ` +
			`final answer (should be two short phrases separated by a semicolon) in ` +
			`\boxed{}.`,
	)

	assert.Contains(t, got, "reassigning")
	assert.Contains(t, got, "workload")
	assert.Contains(t, got, "problems")
	assert.Contains(t, got, "modules")
	assert.Contains(t, got, "workflow")
	assert.Contains(t, got, "portal")
	assert.NotContains(t, got, "accomplish")
	assert.NotContains(t, got, "typically")
	assert.NotContains(t, got, "semicolon")
	assert.NotContains(t, got, "separated")
	assert.NotContains(t, got, "company")
	assert.NotContains(t, got, "specific")
	assert.LessOrEqual(t, len(got), MaxMemorySearchTerms)
}

func TestMemoryQueryTermsDropsActionSpaceBeforeSearchTermLimit(t *testing.T) {
	got := memoryQueryTerms(
		`I am using our magento-based custom shopping admin website. I am on ` +
			`the home page now and would like to filter orders by their ` +
			"`Fraud Suspect Resolution` status. Given the following constrained " +
			"action space, how many actions do I need to perform?\n\n" +
			"Action Space:\n" +
			"scroll(delta_x: float, delta_y: float), keyboard_press(key: str), " +
			"click(bid: str, button='left', modifiers=[]), " +
			"fill(bid: str, value: str, enable_autocomplete_menu: bool = False), " +
			"hover(bid: str), tab_focus(index: int), new_tab(), go_back(), " +
			"go_forward(), tab_close(), select_option(bid: str, " +
			"options: str | list[str])\n\n" +
			`Your final answer should be an English number wrapped in \boxed{}.`,
	)

	assert.Contains(t, got, "resolution")
	assert.Contains(t, got, "suspect")
	assert.Contains(t, got, "fraud")
	assert.Contains(t, got, "magento")
	assert.NotContains(t, got, "enable_autocomplete_menu")
	assert.NotContains(t, got, "keyboard_press")
	assert.NotContains(t, got, "select_option")
	assert.LessOrEqual(t, len(got), MaxMemorySearchTerms)
}

func TestListMemoryTextCandidatesRetainsShortCriticalUITermMatch(t *testing.T) {
	d := testDB(t)
	requireMemoryFTS(t, d)
	ctx := context.Background()
	insertSession(t, d, "s1", "test-agent", func(s *Session) {
		s.Agent = "test-agent"
	})
	_, err := d.InsertMemory(Memory{
		ID:              "incident-filter-labels",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Portal filter labels",
		Body:            "Portal filters menu: Mobile option.",
		Project:         "test-agent",
		Agent:           "test-agent",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)

	candidates, err := d.ListMemoryTextCandidates(ctx, MemoryQuery{
		Text: `I am working with our ServiceNow portal. On the Incidents list page, ` +
			`when I open the "Filters" dropdown, excluding "Edit personal filters" ` +
			`and "-- None --", which filter option labels contain the substring ` +
			`"Incident"? Mark your final answer (should be one or more short ` +
			`phrases) in \boxed{}.`,
		Project: "test-agent",
		Agent:   "test-agent",
		Limit:   10,
	})

	require.NoError(t, err)
	require.NotEmpty(t, candidates)
	assert.Equal(t, "incident-filter-labels", candidates[0].ID)
}

func TestGetMemoryMissingReturnsNil(t *testing.T) {
	d := testDB(t)

	got, err := d.GetMemory(context.Background(), "missing")

	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestToCoreMemoriesPreservesTimestamps(t *testing.T) {
	got := toCoreMemories([]Memory{
		{
			ID:        "m1",
			Status:    "accepted",
			Title:     "Recent note",
			CreatedAt: "2024-01-01T00:00:00Z",
			UpdatedAt: "2024-02-01T00:00:00Z",
		},
	})

	require.Len(t, got, 1)
	assert.Equal(t, "2024-01-01T00:00:00Z", got[0].CreatedAt)
	assert.Equal(t, "2024-02-01T00:00:00Z", got[0].UpdatedAt)
}

func testID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	if n == 0 {
		return "a"
	}
	var out []byte
	for n > 0 {
		out = append(out, chars[n%len(chars)])
		n /= len(chars)
	}
	return string(out)
}

func TestCopyMemoriesFrom(t *testing.T) {
	dir := t.TempDir()

	// Source DB: session s1 (will survive in dest) and s2 (will not).
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "open src")
	insertSession(t, srcDB, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	insertSession(t, srcDB, "s2", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	_, err = srcDB.InsertMemory(Memory{
		ID: "m1", Type: "fact", Scope: "project", Status: "accepted",
		Title: "kept", Body: "heliotrope parser overflow",
		Project: "agentsview", Agent: "codex", SourceSessionID: "s1",
		Evidence: []MemoryEvidence{{
			SessionID: "s1", MessageStartOrdinal: 1, MessageEndOrdinal: 1,
			Snippet: "the decisive clue",
		}},
	})
	require.NoError(t, err, "insert m1")
	_, err = srcDB.InsertMemory(Memory{
		ID: "m2", Type: "fact", Scope: "project", Status: "accepted",
		Title: "dropped", Body: "session is gone",
		Project: "agentsview", Agent: "codex", SourceSessionID: "s2",
	})
	require.NoError(t, err, "insert m2")

	// Pin known timestamps to verify they survive the copy.
	_, err = srcDB.getWriter().Exec(
		`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = 'm1'`,
		"2024-01-02T03:04:05.678Z", "2024-02-03T04:05:06.789Z",
	)
	require.NoError(t, err, "stamp m1")
	srcDB.Close()

	// Destination DB has only s1 (s2 was not preserved by the resync).
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "open dst")
	defer dstDB.Close()
	insertSession(t, dstDB, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	require.NoError(t, dstDB.CopyMemoriesFrom(srcPath), "CopyMemoriesFrom")

	ctx := context.Background()

	// m1 copied with evidence and original timestamps preserved.
	m1, err := dstDB.GetMemory(ctx, "m1")
	require.NoError(t, err, "get m1")
	require.NotNil(t, m1, "m1 should be copied")
	assert.Equal(t, "2024-01-02T03:04:05.678Z", m1.CreatedAt, "created_at")
	assert.Equal(t, "2024-02-03T04:05:06.789Z", m1.UpdatedAt, "updated_at")
	require.Len(t, m1.Evidence, 1, "evidence copied")
	assert.Equal(t, "the decisive clue", m1.Evidence[0].Snippet)

	// m2 skipped because its source session did not survive (FK guard).
	m2, err := dstDB.GetMemory(ctx, "m2")
	require.NoError(t, err, "get m2")
	assert.Nil(t, m2, "m2 skipped: source session not preserved")

	// Copied memory is searchable via FTS in the destination.
	cands, err := dstDB.ListMemoryTextCandidates(
		ctx, MemoryQuery{Text: "heliotrope"},
	)
	require.NoError(t, err, "search copied memory")
	require.Len(t, cands, 1, "fts finds copied memory")
	assert.Equal(t, "m1", cands[0].ID)
}

// TestVacuumPreservesMemoriesFTSSearchable guards the assumption that VACUUM
// keeps the external-content memories_fts index attached. memories has a TEXT
// primary key, so the SQLite docs warn VACUUM "may change" its rowids -- which
// would break the rowid join. The bundled SQLite preserves rowids, so search
// keeps working with no FTS rebuild; if a SQLite bump ever renumbers them, the
// post-vacuum assertion below fails and Vacuum must rebuild memories_fts.
func TestNormalizeMemoryQueryTrimsExactFilters(t *testing.T) {
	q := NormalizeMemoryQuery(MemoryQuery{
		Project:            "  agentsview  ",
		Status:             "  archived  ",
		SourceEpisodeID:    "  ep-1  ",
		SupersedesMemoryID: "  mem-old  ",
	})
	assert.Equal(t, "agentsview", q.Project)
	assert.Equal(t, "archived", q.Status)
	assert.Equal(t, "ep-1", q.SourceEpisodeID)
	assert.Equal(t, "mem-old", q.SupersedesMemoryID)
}

func TestQueryMemoriesHonorsStatusFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	_, err := d.InsertMemory(Memory{
		ID: "acc", Type: "fact", Scope: "project", Status: "accepted",
		Title: "kept", Body: "heliotrope alpha",
		Project: "agentsview", Agent: "codex", SourceSessionID: "s1",
	})
	require.NoError(t, err, "insert accepted")
	_, err = d.InsertMemory(Memory{
		ID: "arc", Type: "fact", Scope: "project", Status: "accepted",
		Title: "old", Body: "heliotrope beta",
		Project: "agentsview", Agent: "codex", SourceSessionID: "s1",
	})
	require.NoError(t, err, "insert to-be-archived")
	_, err = d.getWriter().Exec(
		`UPDATE memories SET status = 'archived' WHERE id = 'arc'`,
	)
	require.NoError(t, err, "archive arc")

	// A text query with no status filter returns only accepted memories.
	page, err := d.QueryMemories(ctx, MemoryQuery{Text: "heliotrope"})
	require.NoError(t, err, "default query")
	require.Len(t, page.Memories, 1, "default query excludes archived")
	assert.Equal(t, "acc", page.Memories[0].ID)

	// The same text query with status=archived returns the archived memory.
	page, err = d.QueryMemories(
		ctx, MemoryQuery{Text: "heliotrope", Status: "archived"},
	)
	require.NoError(t, err, "archived query")
	require.Len(t, page.Memories, 1, "archived status returns archived memory")
	assert.Equal(t, "arc", page.Memories[0].ID)
}

func TestVacuumPreservesMemoriesFTSSearchable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	if d.memoryFTSKind(ctx) != "fts5" {
		t.Skip("requires fts5 runtime support")
	}
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})

	// Insert several memories, then delete the earlier ones so their low
	// rowids are freed -- the scenario in which VACUUM would renumber the
	// survivor's rowid if it renumbered at all.
	bodies := []string{
		"alpha aardvark", "beta barnacle", "gamma heliotrope overflow",
	}
	for i, body := range bodies {
		_, err := d.InsertMemory(Memory{
			ID: fmt.Sprintf("m%d", i+1), Type: "fact", Scope: "project",
			Status: "accepted", Title: "t", Body: body,
			Project: "agentsview", Agent: "codex", SourceSessionID: "s1",
		})
		require.NoError(t, err, "insert memory")
	}
	_, err := d.getWriter().Exec(
		`DELETE FROM memories WHERE id IN ('m1', 'm2')`,
	)
	require.NoError(t, err, "delete earlier memories")

	q := MemoryQuery{Text: "heliotrope"}
	terms := memoryQueryTerms(q.Text)

	pre, err := d.listMemoryFTS5Candidates(ctx, q, terms)
	require.NoError(t, err, "fts5 search before vacuum")
	require.Len(t, pre, 1, "fts join finds survivor before vacuum")

	require.NoError(t, d.Vacuum(), "vacuum")

	post, err := d.listMemoryFTS5Candidates(ctx, q, terms)
	require.NoError(t, err, "fts5 search after vacuum")
	require.Len(t, post, 1, "fts join still finds survivor after vacuum")
	assert.Equal(t, "m3", post[0].ID)
}

func TestMemoryLifecycleBucket(t *testing.T) {
	tests := []struct {
		name         string
		supersedes   string
		supersededBy string
		status       string
		want         string
	}{
		{name: "active", status: "accepted", want: "active"},
		{name: "replacement", supersedes: "old", status: "accepted", want: "replacement"},
		{name: "superseded by link", supersededBy: "new", status: "accepted", want: "superseded"},
		{
			name: "replacement and superseded", supersedes: "old",
			supersededBy: "new", status: "accepted", want: "replacement_superseded",
		},
		{name: "archived without link is superseded", status: "archived", want: "superseded"},
		{
			name: "archived replacement stays replacement", supersedes: "old",
			status: "archived", want: "replacement",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Memory{
				SupersedesMemoryID:   tt.supersedes,
				SupersededByMemoryID: tt.supersededBy,
				Status:               tt.status,
			}
			assert.Equal(t, tt.want, m.LifecycleBucket())
		})
	}
}
