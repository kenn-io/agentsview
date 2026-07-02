package sync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

const antigravityCLITestSchema = `
	CREATE TABLE steps (
		idx integer,
		step_type integer NOT NULL DEFAULT 0,
		step_payload blob,
		PRIMARY KEY (idx))
`

// agyCLIUnknownSchemaAnomaly is the anomaly a run produces when it writes n
// antigravity-cli sessions from the minimal test schema above. That schema is
// not the recognized agy baseline, so its fingerprint maps to an "agy-schema:"
// source_version and the decode-confidence rule flags it as low confidence.
func agyCLIUnknownSchemaAnomaly(n int) sync.AnomalyStats {
	return sync.AnomalyStats{
		UnknownSchemaSessionsByAgent: map[string]int{"antigravity-cli": n},
		UnknownSchemaSessionsTotal:   n,
	}
}

func TestSyncEngineAntigravityCLI_HappyPath(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "33333333-4444-5555-6666-777777777777"

	// Create subdirectories
	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	// Write history.jsonl to map the project
	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/my-cli-project", "timestamp": 1716244800000, "display": "Initial Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	// Write .pb file
	pbPath := filepath.Join(convDir, uuid+".pb")
	require.NoError(t, os.WriteFile(pbPath, []byte("dummy-pb"), 0o644))

	// Write .trajectory.json
	trajectoryJSON := `{
		"trajectoryId": "` + uuid + `",
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:40:00Z"
				},
				"userInput": {
					"userResponse": "Check workspace status"
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:41:00Z"
				},
				"plannerResponse": {
					"thinking": "I should list files",
					"response": "listing files now",
					"toolCalls": [
						{
							"id": "tc-123",
							"name": "run_command",
							"argumentsJson": "{\"CommandLine\":\"ls\"}"
						}
					]
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_RUN_COMMAND",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:42:00Z",
					"executionId": "tc-123"
				},
				"runCommand": {
					"commandLine": "ls",
					"combinedOutput": "\"fileA.go\""
				}
			}
		]
	}`
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte(trajectoryJSON), 0o644))

	// First Sync: should ingest 1 session
	stats := runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, 1, stats.Synced)

	// Verify database ingestion
	assertSessionProject(t, env.db, "antigravity-cli:"+uuid, "/home/user/my-cli-project")
	// Expected messages:
	// 1. User: "Check workspace status"
	// 2. Assistant: "listing files now" (with tool calls and thoughts)
	// (Note: synthetic empty-content User message with tool results is paired and filtered out by the engine)
	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 2)

	msgs := fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 2)

	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "Check workspace status", msgs[0].Content)

	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "listing files now", msgs[1].Content)
}

func TestSyncEngineAntigravityCLI_SidecarUpdates(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	syncUUID := "44444444-5555-6666-7777-888888888888"
	sinceUUID := "77777777-8888-9999-0000-111111111111"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	history := `{"conversationId": "` + syncUUID + `", "workspace": "/home/user/workspace-abc", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n" +
		`{"conversationId": "` + sinceUUID + `", "workspace": "/home/user/workspace-since", "timestamp": 1716244800000, "display": "History Prompt Since"}` + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(env.antigravityCLIDir, "history.jsonl"),
		[]byte(history), 0o644,
	))

	syncPBPath := filepath.Join(convDir, syncUUID+".pb")
	require.NoError(t, os.WriteFile(syncPBPath, []byte("dummy-pb"), 0o644))
	sincePBPath := filepath.Join(convDir, sinceUUID+".pb")
	require.NoError(t, os.WriteFile(sincePBPath, []byte("dummy-pb"), 0o644))
	oldTime := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(sincePBPath, oldTime, oldTime))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        2,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, "antigravity-cli:"+syncUUID, 1)
	msgs := fetchMessages(t, env.db, "antigravity-cli:"+syncUUID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assertSessionMessageCount(t, env.db, "antigravity-cli:"+sinceUUID, 1)
	msgs = fetchMessages(t, env.db, "antigravity-cli:"+sinceUUID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt Since", msgs[0].Content)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        0,
		Skipped:       2,
	})

	time.Sleep(10 * time.Millisecond)
	trajPath := filepath.Join(convDir, syncUUID+".trajectory.json")
	trajectoryJSON := antigravityCLISingleUserTrajectory(
		syncUUID, "New Prompt from Trajectory",
	)
	require.NoError(t, os.WriteFile(trajPath, []byte(trajectoryJSON), 0o644))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        1,
		Skipped:       1,
	})

	assertSessionMessageCount(t, env.db, "antigravity-cli:"+syncUUID, 1)
	msgs = fetchMessages(t, env.db, "antigravity-cli:"+syncUUID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "New Prompt from Trajectory", msgs[0].Content)

	time.Sleep(10 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)

	sinceTrajPath := filepath.Join(convDir, sinceUUID+".trajectory.json")
	sinceTrajectoryJSON := antigravityCLISingleUserTrajectory(
		sinceUUID, "Prompt from SyncAllSince Trajectory",
	)
	require.NoError(t, os.WriteFile(
		sinceTrajPath, []byte(sinceTrajectoryJSON), 0o644,
	))

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	msgs = fetchMessages(t, env.db, "antigravity-cli:"+sinceUUID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Prompt from SyncAllSince Trajectory", msgs[0].Content)
}

func TestSyncEngineAntigravityCLI_SyncAllSinceReSyncsDBWalUpdate(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "22222222-3333-4444-5555-777777777777"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-wal", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, "Initial database prompt text")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(1),
	})

	baseInfo, err := os.Stat(dbPath)
	require.NoError(t, err)
	baseMtime := baseInfo.ModTime()

	time.Sleep(10 * time.Millisecond)

	writer := openAntigravityCLITestWALDB(t, dbPath)
	defer writer.Close()
	insertAntigravityCLIStep(t, writer, 1, 17, "Assistant response from WAL text")

	walInfo, err := os.Stat(dbPath + "-wal")
	require.NoError(t, err, "expected WAL sidecar after uncheckpointed write")
	require.Greater(t, walInfo.ModTime().UnixNano(), baseMtime.UnixNano())

	require.NoError(t, os.Chtimes(dbPath, baseMtime, baseMtime))
	baseAfter, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Equal(t, baseInfo.Size(), baseAfter.Size(), "base DB size changed")
	require.Equal(t, baseMtime.UnixNano(), baseAfter.ModTime().UnixNano(),
		"base DB mtime should not reveal WAL-only update")

	stats := env.engine.SyncAllSince(context.Background(), baseMtime.Add(time.Nanosecond), nil)
	assert.Equal(t, 1, stats.TotalSessions)
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 0, stats.Skipped)

	assertSessionMessageCount(t, env.db, sessionID, 2)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Assistant response from WAL text", msgs[0].Content)
	assert.Equal(t, "History Prompt", msgs[1].Content)
}

func TestSyncEngineAntigravityCLI_MalformedSidecarFallback(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "55555555-6666-7777-8888-999999999999"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-xyz", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	pbPath := filepath.Join(convDir, uuid+".pb")
	require.NoError(t, os.WriteFile(pbPath, []byte("dummy-pb"), 0o644))

	// Malformed sidecar
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte("invalid-json{"), 0o644))

	// Ingest: Should fall back to reading history.jsonl safely
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 1)
	msgs := fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
}

func TestSyncEngineAntigravityCLI_DBFallbackRetries(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	malformedUUID := "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	filteredUUID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	malformedSessionID := "antigravity-cli:" + malformedUUID
	filteredSessionID := "antigravity-cli:" + filteredUUID

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	history := `{"conversationId": "` + malformedUUID + `", "workspace": "/home/user/workspace-db", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n" +
		`{"conversationId": "` + filteredUUID + `", "workspace": "/home/user/workspace-db-filtered", "timestamp": 1716244800000, "display": "Filtered History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(env.antigravityCLIDir, "history.jsonl"),
		[]byte(history), 0o644,
	))

	malformedDBPath := filepath.Join(convDir, malformedUUID+".db")
	require.NoError(t, os.WriteFile(malformedDBPath, []byte("not a sqlite database"), 0o644))
	filteredDBPath := filepath.Join(convDir, filteredUUID+".db")
	createAntigravityCLIUndisplayableStepDB(t, filteredDBPath)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        2,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(1),
	})

	assertSessionMessageCount(t, env.db, malformedSessionID, 1)
	msgs := fetchMessages(t, env.db, malformedSessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(malformedSessionID), db.CurrentDataVersion(),
		"degraded DB fallback should stay stale so unchanged syncs retry")
	assertSessionMessageCount(t, env.db, filteredSessionID, 1)
	msgs = fetchMessages(t, env.db, filteredSessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Filtered History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(filteredSessionID), db.CurrentDataVersion(),
		"DB fallback after dropping all raw steps should stay stale so unchanged syncs retry")

	require.NoError(t, env.db.SetSessionDataVersion(malformedSessionID, db.CurrentDataVersion()))
	require.NoError(t, env.db.SetSessionDataVersion(filteredSessionID, db.CurrentDataVersion()))
	require.NoError(t, env.db.ResetAllMtimes(), "force fallback rewrite")
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        2,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(1),
	})
	assert.Less(t, env.db.GetSessionDataVersion(malformedSessionID), db.CurrentDataVersion(),
		"DB decode fallback should demote previously current rows")
	assert.Less(t, env.db.GetSessionDataVersion(filteredSessionID), db.CurrentDataVersion(),
		"DB fallback after dropping all raw steps should demote previously current rows")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        2,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(1),
	})
	assert.Less(t, env.db.GetSessionDataVersion(malformedSessionID), db.CurrentDataVersion(),
		"unchanged DB decode fallback should keep retrying")
	assert.Less(t, env.db.GetSessionDataVersion(filteredSessionID), db.CurrentDataVersion(),
		"unchanged DB fallback after dropping all raw steps should keep retrying")
}

func TestSyncEngineAntigravityCLI_NeedsRetryReplacesCurrentMessages(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-retry", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, "Initial database prompt text")
	conn := openAntigravityCLITestDB(t, dbPath)
	insertAntigravityCLIStep(t, conn, 1, 17, "Initial assistant response text")
	require.NoError(t, conn.Close())

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(1),
	})
	assertSessionMessageCount(t, env.db, sessionID, 2)
	assert.Equal(t, db.CurrentDataVersion(), env.db.GetSessionDataVersion(sessionID))

	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, sessionID, 1)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"retry fallback should be written before the row is demoted")
}

func TestSyncSingleSessionAntigravityCLI_DBDecodeFallbackRetries(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "99999999-aaaa-bbbb-cccc-dddddddddddd"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-single", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644))

	// An explicit single-session sync (the file-watcher path) of a .db
	// whose decode fails must store the fallback at a stale data_version
	// so a later sync retries the high-resolution source.
	require.NoError(t, env.engine.SyncSingleSession(sessionID))

	assertSessionMessageCount(t, env.db, sessionID, 1)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"single-session DB fallback should stay stale so later syncs retry")

	// A previously current row must be demoted when an explicit re-sync
	// hits the same decode failure, otherwise the high-resolution DB is
	// never retried once it has been stamped current.
	require.NoError(t, env.db.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(t, env.db.ResetAllMtimes(), "force fallback rewrite")
	require.NoError(t, env.engine.SyncSingleSession(sessionID))
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"single-session DB decode fallback should demote previously current rows")
}

// writeAntigravityCLIInferredProjectFixture writes a .db session whose
// history.jsonl row omits conversationId, so the workspace can only reach
// the session row through the GH #579 prompt/timestamp inference fallback.
// The fixture step carries no timestamp, so the parser falls back to the
// .db mtime; pinning it with Chtimes keeps the 60s window deterministic.
func writeAntigravityCLIInferredProjectFixture(
	t *testing.T, env *testEnv, uuid, display, workspace string,
	dbMtime, rowTime time.Time,
) {
	t.Helper()

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := fmt.Sprintf(
		`{"workspace": %q, "timestamp": %d, "display": %q}`+"\n",
		workspace, rowTime.UnixMilli(), display,
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.antigravityCLIDir, "history.jsonl"),
		[]byte(historyLine), 0o644,
	))

	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, "Fix the flux capacitor BUG")
	require.NoError(t, os.Chtimes(dbPath, dbMtime, dbMtime))
}

func TestSyncEngineAntigravityCLI_InferredProjectWithoutConversationID(t *testing.T) {
	base := time.UnixMilli(1716244800000)
	// Display differs from the stored prompt in case and extra
	// leading/trailing/internal whitespace; only the normalized
	// (lowercased, whitespace-collapsed) forms match.
	const display = "  fix  THE Flux   Capacitor Bug "
	const workspace = "/home/user/inferred-project"

	tests := []struct {
		name        string
		rowTime     time.Time
		wantProject string
	}{
		{
			name:        "normalized match within window infers project",
			rowTime:     base.Add(10 * time.Second),
			wantProject: workspace,
		},
		{
			name:        "match outside 60s window leaves project empty",
			rowTime:     base.Add(2 * time.Minute),
			wantProject: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
			uuid := "ab12cd34-1111-2222-3333-444455556666"
			sessionID := "antigravity-cli:" + uuid

			writeAntigravityCLIInferredProjectFixture(
				t, env, uuid, display, workspace, base, tt.rowTime,
			)

			runSyncAndAssert(t, env.engine, sync.SyncStats{
				TotalSessions: 1,
				Synced:        1,
				Skipped:       0,
				Anomalies:     agyCLIUnknownSchemaAnomaly(1),
			})

			assertSessionProject(t, env.db, sessionID, tt.wantProject)
			assertSessionMessageCount(t, env.db, sessionID, 1)
			msgs := fetchMessages(t, env.db, sessionID)
			require.Len(t, msgs, 1)
			assert.Equal(t, "Fix the flux capacitor BUG", msgs[0].Content)
		})
	}
}

func TestSyncSingleSessionAntigravityCLI_InferredProjectWithoutConversationID(t *testing.T) {
	base := time.UnixMilli(1716244800000)
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "cd34ef56-7777-8888-9999-aaaabbbbcccc"
	sessionID := "antigravity-cli:" + uuid

	writeAntigravityCLIInferredProjectFixture(
		t, env, uuid, "  fix  THE Flux   Capacitor Bug ",
		"/home/user/inferred-project-single",
		base, base.Add(10*time.Second),
	)

	// The file-watcher path must persist the inferred project too.
	require.NoError(t, env.engine.SyncSingleSession(sessionID))

	assertSessionProject(t, env.db, sessionID, "/home/user/inferred-project-single")
	assertSessionMessageCount(t, env.db, sessionID, 1)
}

func TestSyncPathsAntigravityCLIHistoryOnlyUpdateRefreshesProject(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "de45fa67-8888-9999-aaaa-bbbbccccdddd"
	sessionID := "antigravity-cli:" + uuid
	early := time.UnixMilli(1716244800000)
	late := early.Add(time.Minute)

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, "History arrives later")
	require.NoError(t, os.Chtimes(dbPath, early, early))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(1),
	})
	assertSessionProject(t, env.db, sessionID, "")

	historyPath := filepath.Join(env.antigravityCLIDir, "history.jsonl")
	historyLine := fmt.Sprintf(
		`{"conversationId": %q, "workspace": "/home/user/history-arrived", "timestamp": %d, "display": "History arrives later"}`+"\n",
		uuid, late.UnixMilli(),
	)
	require.NoError(t, os.WriteFile(historyPath, []byte(historyLine), 0o644))
	require.NoError(t, os.Chtimes(historyPath, late, late))

	env.engine.SyncPaths([]string{historyPath})

	assertSessionProject(t, env.db, sessionID, "/home/user/history-arrived")
	assertSessionMessageCount(t, env.db, sessionID, 1)
}

func TestSyncPathsAntigravityCLIHistoryRetagClearsRemovedProject(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	removedID := "ee45fa67-8888-9999-aaaa-bbbbccccdddd"
	retaggedID := "ff45fa67-8888-9999-aaaa-bbbbccccdddd"
	removedSessionID := "antigravity-cli:" + removedID
	retaggedSessionID := "antigravity-cli:" + retaggedID
	base := time.UnixMilli(1716244800000)

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	for id, prompt := range map[string]string{
		removedID:  "Original history prompt",
		retaggedID: "Retagged history prompt",
	} {
		dbPath := filepath.Join(convDir, id+".db")
		createAntigravityCLIDisplayStepDB(t, dbPath, prompt)
		require.NoError(t, os.Chtimes(dbPath, base, base))
	}
	historyPath := filepath.Join(env.antigravityCLIDir, "history.jsonl")
	initialHistory := fmt.Sprintf(
		`{"conversationId": %q, "workspace": "/home/user/removed", "timestamp": %d, "display": "Original history prompt"}`+"\n",
		removedID, base.UnixMilli(),
	) + fmt.Sprintf(
		`{"conversationId": %q, "workspace": "/home/user/retagged", "timestamp": %d, "display": "Retagged history prompt"}`+"\n",
		retaggedID, base.UnixMilli(),
	)
	require.NoError(t, os.WriteFile(historyPath, []byte(initialHistory), 0o644))
	require.NoError(t, os.Chtimes(historyPath, base, base))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        2,
		Skipped:       0,
		Anomalies:     agyCLIUnknownSchemaAnomaly(2),
	})
	assertSessionProject(t, env.db, removedSessionID, "/home/user/removed")
	assertSessionProject(t, env.db, retaggedSessionID, "/home/user/retagged")

	updated := base.Add(time.Minute)
	retaggedHistory := fmt.Sprintf(
		`{"conversationId": %q, "workspace": "/home/user/retagged-now", "timestamp": %d, "display": "Retagged history prompt"}`+"\n",
		retaggedID, updated.UnixMilli(),
	)
	require.NoError(t, os.WriteFile(historyPath, []byte(retaggedHistory), 0o644))
	require.NoError(t, os.Chtimes(historyPath, updated, updated))

	env.engine.SyncPaths([]string{historyPath})

	assertSessionProject(t, env.db, removedSessionID, "")
	assertSessionProject(t, env.db, retaggedSessionID, "/home/user/retagged-now")
	assertSessionMessageCount(t, env.db, removedSessionID, 1)
	assertSessionMessageCount(t, env.db, retaggedSessionID, 1)
}

func TestSyncEngineAntigravityCLI_MissingPbOrphanSidecar(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentAntigravityCLI)
	uuid := "66666666-7777-8888-9999-000000000000"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	// Write ONLY .trajectory.json, no .pb
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte("{}"), 0o644))

	// Sync: should discover no sessions
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 0,
		Synced:        0,
		Skipped:       0,
	})
}

func createAntigravityCLIUndisplayableStepDB(t *testing.T, path string) {
	t.Helper()
	copyAntigravityCLITestSchemaTemplate(t, path)
	conn := openAntigravityCLITestDB(t, path)
	defer conn.Close()

	insertAntigravityCLIStep(t, conn, 0, 14, "MODEL_PLACEHOLDER_0")
}

func createAntigravityCLIDisplayStepDB(t *testing.T, path, prompt string) {
	t.Helper()
	copyAntigravityCLITestSchemaTemplate(t, path)
	conn := openAntigravityCLITestDB(t, path)
	defer conn.Close()

	insertAntigravityCLIStep(t, conn, 0, 14, prompt)
}

func antigravityCLISingleUserTrajectory(uuid, prompt string) string {
	return fmt.Sprintf(`{
		"trajectoryId": %q,
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:45:00Z"
				},
				"userInput": {
					"userResponse": %q
				}
			}
		]
	}`, uuid, prompt)
}

func copyAntigravityCLITestSchemaTemplate(t *testing.T, path string) {
	t.Helper()
	copySQLiteSchemaTemplate(
		t, path, "antigravity cli", &antigravityCLISchemaOnce,
		&antigravityCLISchemaBytes, &antigravityCLISchemaErr,
		antigravityCLITestSchema,
	)
}

func openAntigravityCLITestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open antigravity cli test db")

	return conn
}

func openAntigravityCLITestWALDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn := openAntigravityCLITestDB(t, path)

	_, err := conn.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err, "enable WAL mode")
	_, err = conn.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err, "disable WAL autocheckpoint")

	return conn
}

func insertAntigravityCLIStep(
	t *testing.T, conn *sql.DB, idx, stepType int, content string,
) {
	t.Helper()
	payload := antigravityCLIStringPayload(content)
	_, err := conn.Exec(
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		idx, stepType, payload,
	)
	require.NoError(t, err, "insert antigravity cli step")
}

func antigravityCLIStringPayload(s string) []byte {
	content := []byte(s)
	return append([]byte{0x8a, 0x01, byte(len(content))}, content...)
}
