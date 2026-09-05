package main

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestSimulatorParsesAndAppendsBothProviders(t *testing.T) {
	out := t.TempDir()
	data := filepath.Join(out, "data")
	result, err := run(t.Context(), options{Sessions: 4, Turns: 2, Active: 2, Iterations: 2, ReconcileEvery: 1, QueryEvery: 1, Empty: 3, ContentBytes: 64, Output: out}, data)
	require.NoError(t, err)
	assert.Equal(t, 4, result.Initial.SessionCount)
	assert.Equal(t, 16, result.Initial.MessageCount)
	assert.Equal(t, 4, result.Final.SessionCount)
	assert.Equal(t, 24, result.Final.MessageCount)
	archive, err := db.OpenReadOnly(filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	for _, id := range []string{"00000000-0000-4000-8000-000000000005", "00000000-0000-4000-8000-000000000006", "00000000-0000-4000-8000-000000000007"} {
		session, err := archive.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session, "empty sources are archived even though sidebar stats exclude them")
		assert.Zero(t, session.MessageCount)
	}
	skipped, err := archive.LoadSkippedFiles()
	require.NoError(t, err)
	assert.Empty(t, skipped)
	for _, id := range []string{"00000000-0000-4000-8000-000000000001", "codex:00000000-0000-4000-8000-000000000002"} {
		messages, err := archive.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, messages, 8)
		assert.Contains(t, messages[6].Content, "module 3.")
	}
	usage, err := archive.GetDailyUsage(t.Context(), db.UsageFilter{From: "2026-06-01", To: "2026-06-30", Timezone: "UTC"})
	require.NoError(t, err)
	assert.Equal(t, 2400, usage.Totals.OutputTokens)
}

func TestSimulatorCadenceMeasuresCompletedUpdateBatches(t *testing.T) {
	out := t.TempDir()
	result, err := run(t.Context(), options{Sessions: 2, Turns: 2, Active: 1, Iterations: 5, ReconcileEvery: 2, QueryEvery: 3, ContentBytes: 64, Output: out}, filepath.Join(out, "data"))
	require.NoError(t, err)
	var reconciled, queried []int
	for _, observation := range result.Observations {
		if observation.Phase == "warm" && observation.Operation == "reconcile" {
			reconciled = append(reconciled, observation.CompletedUpdates)
		}
		if observation.Phase == "active" && observation.Operation == "stats" {
			queried = append(queried, observation.CompletedUpdates)
		}
	}
	assert.Equal(t, []int{2, 4}, reconciled)
	assert.Equal(t, []int{3}, queried)
}

func TestSimulatorScansSQLiteAndArchivesChildOnlyEdits(t *testing.T) {
	out := t.TempDir()
	data := filepath.Join(out, "data")
	result, err := run(t.Context(), options{SourceFormat: "opencode", Sessions: 4, Turns: 2, ActiveTurns: 3, Active: 2, Iterations: 2, ReconcileEvery: 1, QueryEvery: 1, ContentBytes: 64, Output: out}, data)
	require.NoError(t, err)
	assert.Equal(t, 4, result.Initial.SessionCount)
	assert.Equal(t, 20, result.Initial.MessageCount)
	assert.Equal(t, 28, result.Final.MessageCount)
	archive, err := db.OpenReadOnly(filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	for _, id := range []string{"opencode:ses_000000000001", "opencode:ses_000000000002"} {
		stored, err := archive.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Nil(t, stored.SourceMissingAt, "writer closure must retain the source")
		messages, err := archive.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, messages, 10)
		assert.Contains(t, messages[8].Content, "module 4.")
		assert.Equal(t, "Streaming part finalized after session metadata update.", messages[9].Content)
	}
	usage, err := archive.GetDailyUsage(t.Context(), db.UsageFilter{From: "2026-06-01", To: "2026-06-30", Timezone: "UTC"})
	require.NoError(t, err)
	assert.Equal(t, 2800, usage.Totals.OutputTokens)
	producer, err := sql.Open("sqlite3", filepath.Join(data, "opencode", "opencode.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	var childAhead int
	require.NoError(t, producer.QueryRow(`SELECT p.time_updated - s.time_updated
 FROM part p JOIN session s ON s.id = p.session_id
 WHERE p.id = 'prt_msg_ses_000000000001_00000004_1'`).Scan(&childAhead))
	assert.Positive(t, childAhead, "part edits must exercise a change invisible to session-row-only scans")
}

func TestGenerateOnlyRetainsSQLiteWorkload(t *testing.T) {
	out := filepath.Join(t.TempDir(), "corpus")
	require.NoError(t, execute(t.Context(), options{GenerateOnly: true, SourceFormat: "opencode", Sessions: 2, Turns: 2, Active: 1, Iterations: 1, ContentBytes: 64, Output: out}))
	encoded, err := os.ReadFile(filepath.Join(out, "sources.json"))
	require.NoError(t, err)
	var manifest struct {
		Sources    []source        `json:"sources"`
		Provenance buildProvenance `json:"provenance"`
	}
	require.NoError(t, json.Unmarshal(encoded, &manifest))
	require.Len(t, manifest.Sources, 2)
	store, err := sql.Open("sqlite3", "file:"+manifest.Sources[0].Path+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var messages int
	require.NoError(t, store.QueryRow("SELECT COUNT(*) FROM message").Scan(&messages))
	assert.Equal(t, 8, messages)
	assert.Len(t, manifest.Provenance.ExecutableSHA256, 64)
}
