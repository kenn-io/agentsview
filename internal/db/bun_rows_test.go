package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBunRowSessionRoundTripPreservesCanonicalFields(t *testing.T) {
	first := "first"
	display := "display"
	sessionName := "source name"
	started := "2026-08-02T12:00:00Z"
	ended := "2026-08-02T12:30:00Z"
	parent := "parent"
	parserParent := "parser-parent"
	pending := "2026-08-02T12:29:00Z"
	pressure := 0.82
	healthScore := 91
	healthGrade := "A"
	deleted := "2026-08-03T09:00:00Z"
	deletionCause := "source_missing"
	termination := "clean"
	filePath := "/fixture/session.jsonl"
	fileSize := int64(4096)
	fileMtime := int64(1_754_131_800)
	fileInode := int64(22)
	fileDevice := int64(7)
	fileHash := "sha256:fixture"
	localModified := "2026-08-02T12:31:00Z"
	revision := "revision-4"
	want := Session{
		ID: "session-1", Project: "project", Machine: "machine", Agent: "claude",
		AgentLabel: "Claude", Entrypoint: "cli", SessionKind: "interactive",
		FirstMessage: &first, DisplayName: &display, SessionName: &sessionName,
		StartedAt: &started, EndedAt: &ended, MessageCount: 12, UserMessageCount: 5,
		ParentSessionID: &parent, ParserParentSessionID: &parserParent,
		RelationshipType: "subagent", TotalOutputTokens: 900, PeakContextTokens: 1800,
		HasTotalOutputTokens: true, HasPeakContextTokens: true, IsAutomated: true,
		ToolFailureSignalCount: 2, ToolRetryCount: 3, EditChurnCount: 4,
		ConsecutiveFailureMax: 5, Outcome: "success", OutcomeConfidence: "high",
		EndedWithRole: "assistant", FinalFailureStreak: 1, SignalsPendingSince: &pending,
		CompactionCount: 6, MidTaskCompactionCount: 2, ContextPressureMax: &pressure,
		HealthScore: &healthScore, HealthGrade: &healthGrade, HasToolCalls: true,
		HasContextData: true, SecretLeakCount: 1, SecretsRulesVersion: "rules-3",
		QualitySignalVersion: 3, ShortPromptCount: 2, UnstructuredStart: true,
		MissingSuccessCriteriaCount: 1, MissingVerificationCount: 2,
		DuplicatePromptCount: 3, NoCodeContextCount: 4, RunawayToolLoopCount: 5,
		DataVersion: 8, Cwd: "/fixture/project", GitBranch: "feature/test",
		SourceSessionID: "source-session", SourceVersion: "1.2",
		TranscriptFidelity: "exact", ParserMalformedLines: 2, IsTruncated: true,
		DeletedAt: &deleted, DeletionCause: &deletionCause, TerminationStatus: &termination,
		FilePath: &filePath, FileSize: &fileSize, FileMtime: &fileMtime,
		FileInode: &fileInode, FileDevice: &fileDevice, FileHash: &fileHash,
		LocalModifiedAt: &localModified, TranscriptRevision: &revision,
		CreatedAt:       "2026-08-02T11:59:00Z",
		SourceArchiveID: "archive-1", SourceDatabaseGeneration: "database-7",
	}

	row, err := sessionToBunRow(want)
	require.NoError(t, err)
	got := sessionFromBunRow(row)
	assert.Equal(t, want, got)
}

func TestBunRowSessionConversionRejectsMalformedOptionalTimestamp(t *testing.T) {
	malformed := "not-a-timestamp"
	_, err := sessionToBunRow(Session{
		ID: "session-1", CreatedAt: "2026-08-02T11:59:00Z",
		StartedAt: &malformed,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "started_at")
	assert.Contains(t, err.Error(), malformed)
}

func TestBunRowSessionConversionRejectsMalformedRequiredTimestamp(t *testing.T) {
	_, err := sessionToBunRow(Session{
		ID: "session-1", CreatedAt: "not-a-created-at",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "created_at")
	assert.Contains(t, err.Error(), "not-a-created-at")
}

func TestBunRowMessageRoundTripPreservesJSONAndOptionalID(t *testing.T) {
	for name, id := range map[string]int64{"source id": 41, "missing source id": 0} {
		t.Run(name, func(t *testing.T) {
			want := Message{
				ID: id, SessionID: "session-1", Ordinal: 7, Role: "assistant",
				Content: "answer", ThinkingText: "reasoning",
				Timestamp: "2026-08-02T12:30:00Z", HasThinking: true,
				HasToolUse: true, ContentLength: 6, Model: "model-1",
				TokenUsage:    json.RawMessage(`{"input_tokens":12,"output_tokens":7}`),
				ContextTokens: 1200, OutputTokens: 7, HasContextTokens: true,
				HasOutputTokens: true, ClaudeMessageID: "message-id",
				ClaudeRequestID: "request-id", IsSystem: true, SourceType: "event",
				SourceSubtype: "assistant", PromptSource: "user", SourceUUID: "uuid-1",
				SourceParentUUID: "uuid-0", IsSidechain: true, IsCompactBoundary: true,
			}

			row, err := messageToBunRow(want)
			require.NoError(t, err)
			if id == 0 {
				assert.Nil(t, row.ID)
			} else {
				require.NotNil(t, row.ID)
				assert.Equal(t, id, *row.ID)
			}
			assert.Equal(t, want, messageFromBunRow(row))
		})
	}
}

func TestBunRowMessageConversionRejectsMalformedTimestamp(t *testing.T) {
	_, err := messageToBunRow(Message{
		SessionID: "session-1", Ordinal: 7, Timestamp: "malformed",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timestamp")
	assert.Contains(t, err.Error(), "malformed")
}

func TestBunRowPriorSQLiteMessageKeyAliasRemainsReadable(t *testing.T) {
	d := testDB(t)

	rows, err := d.getReader().Query(`PRAGMA table_info(messages)`)
	require.NoError(t, err)
	defer rows.Close()
	primaryKeys := make(map[string]int)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		require.NoError(t, rows.Scan(
			&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey,
		))
		if primaryKey != 0 {
			primaryKeys[name] = primaryKey
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]int{"id": 1}, primaryKeys)

	indexRows, err := d.getReader().Query(`PRAGMA index_list(messages)`)
	require.NoError(t, err)
	defer indexRows.Close()
	foundCompositeAlias := false
	for indexRows.Next() {
		var seq, unique, partial int
		var name, origin string
		require.NoError(t, indexRows.Scan(&seq, &name, &unique, &origin, &partial))
		if unique == 0 {
			continue
		}
		columnRows, queryErr := d.getReader().Query(
			`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name,
		)
		require.NoError(t, queryErr)
		var columns []string
		for columnRows.Next() {
			var column string
			require.NoError(t, columnRows.Scan(&column))
			columns = append(columns, column)
		}
		require.NoError(t, columnRows.Err())
		require.NoError(t, columnRows.Close())
		if assert.ObjectsAreEqual([]string{"session_id", "ordinal"}, columns) {
			foundCompositeAlias = true
		}
	}
	require.NoError(t, indexRows.Err())
	assert.True(t, foundCompositeAlias)
}
