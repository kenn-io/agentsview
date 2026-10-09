//go:build chtest

package clickhouse_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func TestToolSequencesHTTPParity(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	sessionIDs := dbtest.SeedToolSequencesParity(t, local)
	const boundaryID = "tool-sequences-parity-boundary"
	dbtest.SeedToolSequencesExample(t, local, boundaryID, func(s *db.Session) { s.MessageCount = 103 })
	example := dbtest.ToolSequencesExampleMessages(boundaryID)
	messages := make([]db.Message, 99, 103)
	for i := range messages {
		messages[i] = db.Message{SessionID: boundaryID, Ordinal: i, Role: "user", Content: "search", ContentLength: 6}
	}
	repeat := example[2]
	repeat.ToolCalls = slices.Clone(repeat.ToolCalls)
	repeat.ToolCalls[0].ToolUseID = "grep-3"
	repeat.ToolCalls[0].ResultEvents = slices.Clone(repeat.ToolCalls[0].ResultEvents)
	for i := range repeat.ToolCalls[0].ResultEvents {
		repeat.ToolCalls[0].ResultEvents[i].ToolUseID = "grep-3"
	}
	for i, message := range []db.Message{example[1], example[2], repeat, example[3]} {
		message.Ordinal = 99 + i
		messages = append(messages, message)
	}
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), boundaryID, messages))
	sessionIDs = append(sessionIDs, boundaryID)
	for _, id := range sessionIDs {
		session, err := local.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		messages, err := local.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		update, _ := ingest.ComputeSignalsAndSecrets(*session, messages)
		require.NoError(t, local.UpdateSessionSignals(t.Context(), id, update))
	}
	localHandler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "local"}, local, nil).Handler()

	dsn, database := chtest.FreshDatabase(t)
	target := clickhouse.Target{URL: dsn, Database: database}
	syncer, err := clickhouse.New(t.Context(), target, local, "tool-sequences-parity-host", storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)

	store, err := clickhouse.NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	filter := db.AnalyticsFilter{From: "2026-04-26", To: "2026-04-26"}
	wantRates, err := local.GetAnalyticsTools(t.Context(), filter)
	require.NoError(t, err)
	gotRates, err := store.GetAnalyticsTools(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, wantRates, gotRates)
	evidenceFilter := filter
	evidenceFilter.ToolName, evidenceFilter.ToolCategory = "Grep", "Grep"
	for _, signal := range []string{"tool_empty_rate", "tool_repeat_rate", "tool_recovery_rate"} {
		wantEvidence, err := local.GetAnalyticsSignalSessions(t.Context(), evidenceFilter, signal, 10)
		require.NoError(t, err)
		require.NotNil(t, wantEvidence.Total)
		if signal == "tool_empty_rate" {
			require.Positive(t, *wantEvidence.Total)
		}
		gotEvidence, err := store.GetAnalyticsSignalSessions(t.Context(), evidenceFilter, signal, 10)
		require.NoError(t, err)
		assert.Equal(t, wantEvidence, gotEvidence, signal)
	}
	remoteHandler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "remote"}, store, nil).Handler()
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err = duckdb.Push(t.Context(), path, local, "tool-sequences-boundary", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	mirror, err := duckdb.NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mirror.Close()) })
	mirrorHandler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "mirror"}, mirror, nil).Handler()
	for _, sessionID := range sessionIDs {
		localJSON := getToolSequencesDocument(t, localHandler, sessionID)
		remoteJSON := getToolSequencesDocument(t, remoteHandler, sessionID)
		assert.Equal(t, localJSON, remoteJSON, sessionID)
		if sessionID == boundaryID {
			assert.Equal(t, localJSON, getToolSequencesDocument(t, mirrorHandler, sessionID))
			assert.Equal(t, float64(4), localJSON["total_tool_calls"])
			assert.Equal(t, float64(4), localJSON["total_sequence_calls"])
			assert.Equal(t, float64(1), localJSON["total_sequences"])
			assert.Equal(t, float64(0), localJSON["omitted_calls"])
			assert.Equal(t, float64(0), localJSON["omitted_sequences"])
			sequences := localJSON["sequences"].([]any)
			require.Len(t, sequences, 1)
			sequence := sequences[0].(map[string]any)
			assert.Equal(t, "recovered", sequence["ending"])
			assert.Equal(t, true, sequence["tool_changed"])
			calls := sequence["calls"].([]any)
			require.Len(t, calls, 4)
			for i, call := range calls {
				row := call.(map[string]any)
				assert.Equal(t, float64(99+i), row["ordinal"])
				if i == 1 || i == 2 {
					assert.Equal(t, "identical", row["repeat"])
				}
			}
			assert.Equal(t, true, calls[3].(map[string]any)["tool_changed"])
			t.Log("boundary sequence ordinals=[99 100 101 102], repeats=2, switch/recovery=true, calls=4, omissions=0; SQLite/DuckDB/ClickHouse HTTP responses equal")
		}
	}
}

func getToolSequencesDocument(t *testing.T, handler http.Handler, sessionID string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+sessionID+"/tool-sequences", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var document map[string]any
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &document))
	return document
}

// A push that fails after deleting a shortened transcript's messages can leave
// their tool calls behind. Evidence must not link to the missing message.
func TestToolEvidenceIgnoresOrdinalOfMissingMessage(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	const sessionID = "tool-evidence-missing-message"
	dbtest.SeedToolSequencesExample(t, local, sessionID)
	session, err := local.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	update, _ := ingest.ComputeSignalsAndSecrets(*session, messages)
	require.NoError(t, local.UpdateSessionSignals(t.Context(), sessionID, update))

	dsn, database := chtest.FreshDatabase(t)
	target := clickhouse.Target{URL: dsn, Database: database}
	syncer, err := clickhouse.New(t.Context(), target, local, "tool-evidence-missing-host", storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := clickhouse.NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	filter := db.AnalyticsFilter{From: "2026-04-26", To: "2026-04-26", ToolName: "Grep", ToolCategory: "Grep"}
	before, err := store.GetAnalyticsSignalSessions(t.Context(), filter, "tool_empty_rate", 10)
	require.NoError(t, err)
	require.Len(t, before.Sessions, 1)
	require.NotNil(t, before.Sessions[0].MessageOrdinal)
	missing := *before.Sessions[0].MessageOrdinal

	conn := chtest.Open(t, dsn, database)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id = ? AND ordinal = ?`, sessionID, missing)
	require.NoError(t, err)
	require.Equal(t, 1, chtest.Count(t, conn, "tool_calls", "session_id = ? AND message_ordinal = ?", sessionID, missing))

	after, err := store.GetAnalyticsSignalSessions(t.Context(), filter, "tool_empty_rate", 10)
	require.NoError(t, err)
	for _, example := range after.Sessions {
		require.NotNil(t, example.MessageOrdinal)
		assert.NotEqual(t, missing, *example.MessageOrdinal)
	}
}
