package server_test

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

type listMemoriesResponse struct {
	Memories    []db.MemoryResult `json:"memories"`
	TrustedOnly bool              `json:"trusted_only"`
}

type queryMemoriesResponse struct {
	Memories        []db.MemoryResult           `json:"memories"`
	TrustedOnly     bool                        `json:"trusted_only"`
	Summary         *service.MemoryQuerySummary `json:"summary,omitempty"`
	Context         string                      `json:"context,omitempty"`
	ContextMeta     *service.MemoryContextMeta  `json:"context_meta,omitempty"`
	ContextMemories []db.MemoryResult           `json:"context_memories,omitempty"`
}

func TestListMemoriesFiltersByProject(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m1",
		Title:           "Check cwd before file reads",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	seedMemory(t, te, db.Memory{
		ID:              "m2",
		Title:           "Other project note",
		Body:            "Unrelated note.",
		Project:         "other",
		Agent:           "codex",
		SourceSessionID: "other-session",
	})

	w := te.get(t, "/api/v1/memories?q=cwd&project=agentsview&agent=codex")
	assertStatus(t, w, http.StatusOK)

	r := decode[listMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "m1", r.Memories[0].ID)
}

func TestListMemoriesFiltersBySourceSessionID(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m-session",
		Title:           "Session scoped cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	seedMemory(t, te, db.Memory{
		ID:              "m-other-session",
		Title:           "Other session cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "other-session",
	})

	w := te.get(t, "/api/v1/memories?q=cwd&source_session_id=memory-session")
	assertStatus(t, w, http.StatusOK)

	r := decode[listMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "m-session", r.Memories[0].ID)
}

func TestListMemoriesFiltersBySourceEpisodeID(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m-episode",
		Title:           "Episode scoped cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		SourceEpisodeID: "memory-session:chunk:0001",
	})
	seedMemory(t, te, db.Memory{
		ID:              "m-other-episode",
		Title:           "Other episode cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		SourceEpisodeID: "memory-session:chunk:0002",
	})

	w := te.get(t, "/api/v1/memories?q=cwd&source_episode_id=memory-session:chunk:0001")
	assertStatus(t, w, http.StatusOK)

	r := decode[listMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "m-episode", r.Memories[0].ID)
}

func TestListMemoriesFiltersTrustedOnly(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "trusted",
		Title:           "Trusted cwd memory",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		Transferable:    true,
		ProvenanceOK:    true,
	})
	seedMemory(t, te, db.Memory{
		ID:              "untrusted",
		Title:           "Untrusted cwd memory",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		Transferable:    true,
		ProvenanceOK:    false,
	})

	w := te.get(t, "/api/v1/memories?q=wrong%20cwd%20files&trusted_only=true")
	assertStatus(t, w, http.StatusOK)

	r := decode[listMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "trusted", r.Memories[0].ID)
	assert.True(t, r.TrustedOnly)
}

func TestListMemoriesFiltersBySupersessionLinks(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "old-a",
		Title:           "Old retry policy A",
		Body:            "Retry flaky command once.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	seedMemory(t, te, db.Memory{
		ID:              "old-b",
		Title:           "Old retry policy B",
		Body:            "Retry flaky command twice.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	_, err := te.db.SupersedeMemory(t.Context(), "old-a", db.Memory{
		ID:              "new-a",
		Type:            "procedure",
		Scope:           "project",
		Title:           "Current retry policy A",
		Body:            "Retry flaky command three times.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	require.NoError(t, err)
	_, err = te.db.SupersedeMemory(t.Context(), "old-b", db.Memory{
		ID:              "new-b",
		Type:            "procedure",
		Scope:           "project",
		Title:           "Current retry policy B",
		Body:            "Retry flaky command four times.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	require.NoError(t, err)

	w := te.get(t, "/api/v1/memories?supersedes_memory_id=old-a")
	assertStatus(t, w, http.StatusOK)

	replacements := decode[listMemoriesResponse](t, w)
	require.Len(t, replacements.Memories, 1)
	assert.Equal(t, "new-a", replacements.Memories[0].ID)

	w = te.get(t, "/api/v1/memories?status=archived&superseded_by_memory_id=new-a")
	assertStatus(t, w, http.StatusOK)

	archived := decode[listMemoriesResponse](t, w)
	require.Len(t, archived.Memories, 1)
	assert.Equal(t, "old-a", archived.Memories[0].ID)
}

func TestListMemoriesWithoutQueryUsesUpdatedOrder(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "older-source-first",
		Title:           "Older memory",
		Body:            "Generic accepted memory.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		SourceEpisodeID: "a-source",
	})
	seedMemory(t, te, db.Memory{
		ID:              "newer-source-second",
		Title:           "Newer memory",
		Body:            "Generic accepted memory.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		SourceEpisodeID: "z-source",
	})
	raw, err := sql.Open("sqlite3", filepath.Join(te.dataDir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { raw.Close() })
	_, err = raw.Exec(`
		UPDATE memories SET updated_at = CASE id
			WHEN 'older-source-first' THEN '2024-01-01T00:00:00Z'
			WHEN 'newer-source-second' THEN '2024-02-01T00:00:00Z'
			ELSE updated_at
		END
		WHERE id IN ('older-source-first', 'newer-source-second')`)
	require.NoError(t, err)

	w := te.get(t, "/api/v1/memories?project=agentsview&agent=codex&limit=2")
	assertStatus(t, w, http.StatusOK)

	r := decode[listMemoriesResponse](t, w)
	require.Len(t, r.Memories, 2)
	assert.Equal(t, "newer-source-second", r.Memories[0].ID)
	assert.Equal(t, "older-source-first", r.Memories[1].ID)
}

func TestGetMemoryFoundAndMissing(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m1",
		Title:           "Check cwd before file reads",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		Evidence: []db.MemoryEvidence{
			{
				SessionID:           "memory-session",
				MessageStartOrdinal: 3,
				MessageEndOrdinal:   7,
				ToolUseID:           "toolu_1",
			},
		},
	})

	w := te.get(t, "/api/v1/memories/m1")
	assertStatus(t, w, http.StatusOK)

	got := decode[db.Memory](t, w)
	assert.Equal(t, "m1", got.ID)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)

	w = te.get(t, "/api/v1/memories/missing")
	assertStatus(t, w, http.StatusNotFound)
}

func TestQueryMemoriesReturnsContext(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m1",
		Title:           "Check cwd before file reads",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		Evidence: []db.MemoryEvidence{
			{
				SessionID:           "memory-session",
				MessageStartOrdinal: 3,
				MessageEndOrdinal:   7,
				ToolUseID:           "toolu_1",
			},
		},
	})

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "cwd failed reads",
		"project": "agentsview",
		"agent": "codex",
		"limit": 5,
		"include_context": true,
		"context_max_bytes": 300
	}`)
	assertStatus(t, w, http.StatusOK)

	r := decode[queryMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "m1", r.Memories[0].ID)
	require.NotNil(t, r.Summary)
	assert.Equal(t, 1, r.Summary.Count)
	assert.Equal(t, 1, r.Summary.ByType["procedure"])
	assert.Equal(t, 1, r.Summary.ByScope["project"])
	assert.Equal(t, 1, r.Summary.ByMatchReason["keyword"])
	assert.Equal(t, 1, r.Summary.BySourceSession["memory-session"])
	assert.Contains(t, r.Context, "Check cwd before file reads")
	assert.Contains(t, r.Context, "memory-session:3-7")
	require.NotNil(t, r.ContextMeta)
	assert.Equal(t, 1, r.ContextMeta.MemoryCount)
	assert.Equal(t, []string{"m1"}, r.ContextMeta.IncludedIDs)
}

func TestQueryMemoriesFiltersBySourceSessionID(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m-session",
		Title:           "Session scoped cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	seedMemory(t, te, db.Memory{
		ID:              "m-other-session",
		Title:           "Other session cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "other-session",
	})

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "cwd failed reads",
		"source_session_id": "memory-session",
		"limit": 5
	}`)
	assertStatus(t, w, http.StatusOK)

	r := decode[queryMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "m-session", r.Memories[0].ID)
}

func TestQueryMemoriesFiltersBySourceEpisodeID(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "m-episode",
		Title:           "Episode scoped cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		SourceEpisodeID: "memory-session:chunk:0001",
	})
	seedMemory(t, te, db.Memory{
		ID:              "m-other-episode",
		Title:           "Other episode cwd memory",
		Body:            "Verify cwd before retrying failed reads.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		SourceEpisodeID: "memory-session:chunk:0002",
	})

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "cwd failed reads",
		"source_episode_id": "memory-session:chunk:0001",
		"limit": 5
	}`)
	assertStatus(t, w, http.StatusOK)

	r := decode[queryMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "m-episode", r.Memories[0].ID)
}

func TestQueryMemoriesFiltersTrustedOnly(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "trusted",
		Title:           "Trusted cwd memory",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		Transferable:    true,
		ProvenanceOK:    true,
	})
	seedMemory(t, te, db.Memory{
		ID:              "untrusted",
		Title:           "Untrusted cwd memory",
		Body:            "Recover from wrong cwd before reading files.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
		Transferable:    false,
		ProvenanceOK:    true,
	})

	w := te.post(t, "/api/v1/memories/query", `{
		"query":"wrong cwd files",
		"trusted_only":true,
		"limit":5
	}`)
	assertStatus(t, w, http.StatusOK)

	r := decode[queryMemoriesResponse](t, w)
	require.Len(t, r.Memories, 1)
	assert.Equal(t, "trusted", r.Memories[0].ID)
	assert.True(t, r.TrustedOnly)
}

func TestQueryMemoriesPacksMultipleFocusedContextEntries(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:    "m1",
		Title: "Incident filters labels overview",
		Body: "Incident filters labels summary. " +
			strings.Repeat("long unrelated filler ", 80),
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	seedMemory(t, te, db.Memory{
		ID:              "m2",
		Title:           "Incident label details",
		Body:            "Incident Mobile and Incident Portal are the useful labels.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "Incident filters labels",
		"project": "agentsview",
		"agent": "codex",
		"limit": 2,
		"include_context": true,
		"context_max_bytes": 900
	}`)
	assertStatus(t, w, http.StatusOK)

	r := decode[queryMemoriesResponse](t, w)
	require.NotNil(t, r.ContextMeta)
	assert.Equal(t, 2, r.ContextMeta.MemoryCount)
	assert.Equal(t, []string{"m1", "m2"}, r.ContextMeta.IncludedIDs)
	assert.Contains(t, r.Context, "Incident Mobile")
	assert.LessOrEqual(t, len([]byte(r.Context)), 900)
}

func TestQueryMemoriesReturnsOnlyPackedContextMemories(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemory(t, te, db.Memory{
		ID:              "packed",
		Title:           "Needle packed cwd memory",
		Body:            "Short needle cwd note.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})
	seedMemory(t, te, db.Memory{
		ID:              "omitted",
		Title:           "Omitted cwd memory",
		Body:            strings.Repeat("Long cwd detail ", 120),
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "memory-session",
	})

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "needle cwd memory",
		"project": "agentsview",
		"agent": "codex",
		"limit": 2,
		"include_context": true,
		"context_max_bytes": 340
	}`)
	assertStatus(t, w, http.StatusOK)

	r := decode[queryMemoriesResponse](t, w)
	require.Len(t, r.Memories, 2)
	require.NotNil(t, r.ContextMeta)
	assert.Equal(t, []string{"packed"}, r.ContextMeta.IncludedIDs)
	require.Len(t, r.ContextMemories, 1)
	assert.Equal(t, "packed", r.ContextMemories[0].ID)
}

func TestQueryMemoriesRejectsNegativeContextMaxBytes(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "cwd",
		"include_context": true,
		"context_max_bytes": -1
	}`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "context_max_bytes")
}

func TestQueryMemoriesRejectsNegativeLimit(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/memories/query", `{
		"query": "cwd",
		"limit": -1
	}`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "limit must be non-negative")
}

func TestListMemoriesInvalidLimit(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/memories?limit=bad")

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "invalid limit")
}

func TestListMemoriesRejectsNegativeLimit(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/memories?limit=-1")

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "limit must be non-negative")
}

func TestListMemoriesRejectsInvalidTrustedOnly(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/memories?trusted_only=yes")

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "invalid trusted_only parameter")
}

func TestImportMemoriesJSONL(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import", `
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)
	assertStatus(t, w, http.StatusOK)

	got := decode[db.MemoryImportResult](t, w)
	assert.Equal(t, 1, got.Imported)

	w = te.get(t, "/api/v1/memories/m1")
	assertStatus(t, w, http.StatusOK)
	memory := decode[db.Memory](t, w)
	assert.Equal(t, "Check cwd before file reads", memory.Title)
}

func TestImportMemoriesRefusesDefaultDataDirWithoutOverride(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	te := setup(t, func(c *config.Config) {
		c.DataDir = defaultDataDir
		c.DBPath = filepath.Join(defaultDataDir, "test.db")
	})
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import", `
{"candidate_id":"m-default-import","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusForbidden)
	assertBodyContains(t, w, "default agentsview data directory")
	assertBodyContains(t, w, "allow_production_import")

	w = te.get(t, "/api/v1/memories/m-default-import")
	assertStatus(t, w, http.StatusNotFound)
}

func TestImportMemoriesDryRunRefusesDefaultDataDirWithoutOverride(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	te := setup(t, func(c *config.Config) {
		c.DataDir = defaultDataDir
		c.DBPath = filepath.Join(defaultDataDir, "test.db")
	})
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import?dry_run=true", `
{"candidate_id":"m-default-import","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusForbidden)
	assertBodyContains(t, w, "default agentsview data directory")
	assertBodyContains(t, w, "allow_production_import")
}

func TestImportMemoriesRefusesSymlinkedDefaultDataDirWithoutOverride(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	require.NoError(t, os.MkdirAll(defaultDataDir, 0o700))
	link := filepath.Join(t.TempDir(), "memory-lab-data")
	require.NoError(t, os.Symlink(defaultDataDir, link))
	te := setup(t, func(c *config.Config) {
		c.DataDir = link
		c.DBPath = filepath.Join(link, "test.db")
	})
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import", `
{"candidate_id":"m-symlink-import","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusForbidden)
	assertBodyContains(t, w, "default agentsview data directory")
	assertBodyContains(t, w, "allow_production_import")
}

func TestImportMemoriesRefusesDefaultDBPathWithoutOverride(t *testing.T) {
	home := t.TempDir()
	defaultDB := filepath.Join(home, ".agentsview", "sessions.db")
	// DataDir is an ordinary lab directory, but DBPath points into the
	// production ~/.agentsview archive. The server must consult the DB path,
	// not just DataDir, and still refuse.
	labDataDir := filepath.Join(t.TempDir(), "memory-lab-data")
	te := setup(t, func(c *config.Config) {
		c.DataDir = labDataDir
		c.DBPath = defaultDB
	})
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import", `
{"candidate_id":"m-default-dbpath-import","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusForbidden)
	assertBodyContains(t, w, "default agentsview data directory")
	assertBodyContains(t, w, "allow_production_import")
}

func TestImportMemoriesRejectsInvalidDryRunBeforeMutation(t *testing.T) {
	te := setup(t)
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import?dry_run=yes", `
{"candidate_id":"m-invalid-dry-run","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "invalid dry_run parameter")

	got := te.get(t, "/api/v1/memories/m-invalid-dry-run")
	assertStatus(t, got, http.StatusNotFound)
}

func TestImportMemoriesAllowsDefaultDataDirWithOverride(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	te := setup(t, func(c *config.Config) {
		c.DataDir = defaultDataDir
		c.DBPath = filepath.Join(defaultDataDir, "test.db")
	})
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import?allow_production_import=true", `
{"candidate_id":"m-default-import","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusOK)
	got := decode[db.MemoryImportResult](t, w)
	assert.Equal(t, 1, got.Imported)
}

func TestImportMemoriesRejectsNumericProductionOverride(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	te := setup(t, func(c *config.Config) {
		c.DataDir = defaultDataDir
		c.DBPath = filepath.Join(defaultDataDir, "test.db")
	})
	seedMemorySession(t, te)
	seedMemoryImportEvidence(t, te)

	w := te.post(t, "/api/v1/memories/import?allow_production_import=1", `
{"candidate_id":"m-numeric-override","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"memory-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "invalid allow_production_import parameter")

	got := te.get(t, "/api/v1/memories/m-numeric-override")
	assertStatus(t, got, http.StatusNotFound)
}

func TestImportMemoriesRequiresExistingEvidenceByDefault(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/memories/import", `
{"candidate_id":"m-missing-session","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"s-not-imported","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "source session s-not-imported not found")

	w = te.get(t, "/api/v1/memories/m-missing-session")
	assertStatus(t, w, http.StatusNotFound)
}

func TestImportMemoriesAllowsPlaceholderSessionsWhenExplicit(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/memories/import?allow_placeholder_sessions=true", `
{"candidate_id":"m-placeholder","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"s-placeholder","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)
	assertStatus(t, w, http.StatusOK)

	got := decode[db.MemoryImportResult](t, w)
	assert.Equal(t, 1, got.Imported)

	w = te.get(t, "/api/v1/memories/m-placeholder")
	assertStatus(t, w, http.StatusOK)
	memory := decode[db.Memory](t, w)
	assert.Equal(t, "s-placeholder", memory.SourceSessionID)
}

func seedMemorySession(t *testing.T, te *testEnv) {
	t.Helper()
	te.seedSession(t, "memory-session", "agentsview", 3, func(s *db.Session) {
		s.Agent = "codex"
		s.Cwd = "/repo/agentsview"
		s.GitBranch = "main"
	})
	te.seedSession(t, "other-session", "other", 3, func(s *db.Session) {
		s.Agent = "codex"
	})
}

func seedMemoryImportEvidence(t *testing.T, te *testEnv) {
	t.Helper()
	messages := []db.Message{
		dbtest.UserMsg("memory-session", 3, "File reads failed from the wrong cwd."),
		dbtest.AsstMsg("memory-session", 4, "I will inspect cwd."),
		dbtest.UserMsg("memory-session", 5, "Retry failed."),
		dbtest.AsstMsg("memory-session", 6, "[Read: main.go]"),
		dbtest.UserMsg("memory-session", 7, "That fixed it."),
	}
	messages[3].HasToolUse = true
	messages[3].ToolCalls = []db.ToolCall{
		{
			SessionID: "memory-session",
			ToolName:  "Read",
			Category:  "Read",
			ToolUseID: "toolu_1",
		},
	}
	dbtest.SeedMessages(t, te.db, messages...)
}

func seedMemory(t *testing.T, te *testEnv, m db.Memory) {
	t.Helper()
	if m.Type == "" {
		m.Type = "procedure"
	}
	if m.Scope == "" {
		m.Scope = "project"
	}
	if m.Status == "" {
		m.Status = "accepted"
	}
	_, err := te.db.InsertMemory(m)
	require.NoError(t, err, "InsertMemory")
}
