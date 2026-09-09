package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStagedToolCallKeyIsSQLiteTextSafeAndUnambiguous(t *testing.T) {
	key := StagedToolCallKey("call_a", 0)
	require.NotContains(t, key, "\x00")
	require.True(t, strings.HasPrefix(key, "6:call_a:"))

	keys := map[string]struct{}{
		StagedToolCallKey("a", 12):   {},
		StagedToolCallKey("a1", 2):   {},
		StagedToolCallKey("a:1", 2):  {},
		StagedToolCallKey("a", 1):    {},
		StagedToolCallKey("a", 2):    {},
		StagedToolCallKey("a:1", 20): {},
	}
	require.Len(t, keys, 6)
}

// TestStagedSessionHasStoredMessagesTx pins the fast-path check a cold
// staged import uses to skip both of stagedSessionContentDigestTx's
// full-table scans: a session with no message rows yet must report false,
// and gains true as soon as any message row exists, regardless of whether
// that row carries a tool call.
func TestStagedSessionHasStoredMessagesTx(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	tx, err := d.getWriter().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	has, err := stagedSessionHasStoredMessagesTx(tx, "s1")
	require.NoError(t, err)
	require.False(t, has, "a session with no stored messages must report false")
	require.NoError(t, tx.Rollback())

	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 0, Role: "user", Content: "hi",
	})

	tx, err = d.getWriter().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	has, err = stagedSessionHasStoredMessagesTx(tx, "s1")
	require.NoError(t, err)
	require.True(t, has, "a session with a stored message must report true")

	has, err = stagedSessionHasStoredMessagesTx(tx, "codex:never-synced")
	require.NoError(t, err)
	require.False(t, has, "an unrelated session id must not report true")
	require.NoError(t, tx.Rollback())
}

// scratchStagedResults is a minimal StagedToolResults backed by a real
// scratch SQLite file, so the publish transaction's ATTACH and
// INSERT..SELECT run against genuine cross-database SQL.
type scratchStagedResults struct {
	path   string
	db     *sql.DB
	seq    int64
	closed bool
}

func newScratchStagedResults(t *testing.T) *scratchStagedResults {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "staged-*.sqlite")
	require.NoError(t, err)
	path := f.Name()
	require.NoError(t, f.Close())
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE stage_events (
		    seq INTEGER PRIMARY KEY,
		    tool_use_id TEXT NOT NULL,
		    agent_id TEXT NOT NULL DEFAULT '',
		    subagent_session_id TEXT NOT NULL DEFAULT '',
		    source TEXT NOT NULL,
		    status TEXT NOT NULL,
		    content TEXT NOT NULL,
		    content_length INTEGER NOT NULL,
		    timestamp TEXT NOT NULL DEFAULT '',
		    blanked INTEGER NOT NULL DEFAULT 0
		)`)
	require.NoError(t, err)
	return &scratchStagedResults{path: path, db: db}
}

func (s *scratchStagedResults) AddEvent(
	t *testing.T, toolUseID, content string,
) {
	t.Helper()
	s.seq++
	_, err := s.db.Exec(
		`INSERT INTO stage_events (
		     seq, tool_use_id, agent_id, subagent_session_id,
		     source, status, content, content_length, timestamp, blanked
		 ) VALUES (?, ?, '', '', 'function_call_output', '',
		           ?, ?, '', 0)`,
		s.seq, toolUseID, content, len(content),
	)
	require.NoError(t, err)
}

func (s *scratchStagedResults) ResolveSummary(
	context.Context, string,
) (string, int, error) {
	return "", 0, nil
}

func (s *scratchStagedResults) InsertEventsTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
	positions map[string]StagedToolCallPosition,
) error {
	for _, pos := range positions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_result_events (
				session_id, tool_call_message_ordinal, call_index,
				tool_use_id, agent_id, subagent_session_id,
				source, status, content, content_length,
				timestamp, event_index
			)
			SELECT ?, ?, ?, tool_use_id,
			       CASE WHEN agent_id = '' THEN NULL ELSE agent_id END,
			       CASE WHEN subagent_session_id = ''
			            THEN NULL ELSE subagent_session_id END,
			       source, status, content, content_length,
			       CASE WHEN timestamp = '' THEN NULL ELSE timestamp END,
			       row_number() OVER (ORDER BY seq) - 1
			FROM codex_staging.stage_events
			WHERE tool_use_id = ?
			ORDER BY seq`,
			sessionID, pos.Ordinal, pos.CallIndex, pos.ToolUseID,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *scratchStagedResults) Path() string { return s.path }

func (s *scratchStagedResults) Close() error {
	s.closed = true
	return nil
}

// TestReplaceSessionContentStagedAttachLifecycle pins the ATTACH/DETACH
// contract: two consecutive staged publishes on the single-connection
// writer pool must both succeed, and after each one the writer connection
// must be free of the codex_staging schema.
func TestReplaceSessionContentStagedAttachLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "staged.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	sessionID := "codex:test-session"
	require.NoError(t, database.UpsertSession(Session{
		ID:               sessionID,
		Agent:            "codex",
		Project:          "project",
		Machine:          "local",
		MessageCount:     1,
		UserMessageCount: 1,
	}))

	publish := func(toolUseID, content string) {
		t.Helper()
		staged := newScratchStagedResults(t)
		staged.AddEvent(t, toolUseID, content)
		msgs := []Message{{
			SessionID:  sessionID,
			Ordinal:    0,
			Role:       "assistant",
			Content:    "running",
			HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolUseID: toolUseID,
				ToolName:  "exec_command",
				Category:  "Bash",
				CallIndex: 0,
			}},
		}}
		require.NoError(t, database.ReplaceSessionContentStaged(
			context.Background(), sessionID, msgs, staged,
			map[string]bool{},
			func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
				return SessionSignalUpdate{}, nil, nil
			},
		))
	}

	// Two consecutive publishes on the same single-connection writer.
	publish("call_a", "first output")
	publish("call_b", "second output")

	// The writer connection must be clean after each publish: a leftover
	// codex_staging attachment is what made the second publish fail.
	rows, err := database.getWriter().Query("PRAGMA database_list")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		require.NoError(t, rows.Scan(&seq, &name, &file))
		require.NotEqual(t, "codex_staging", name,
			"staging schema must be detached after publish")
	}
	require.NoError(t, rows.Err())

	msgs, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "call_b", msgs[0].ToolCalls[0].ToolUseID)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	require.Equal(t, "second output",
		msgs[0].ToolCalls[0].ResultEvents[0].Content)
}

func TestReplaceSessionContentStagedIdenticalPublishKeepsRevision(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "staged-idempotent.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	const sessionID = "codex:idempotent"
	require.NoError(t, database.UpsertSession(Session{
		ID: sessionID, Agent: "codex", Project: "project", Machine: "local",
		MessageCount: 1,
	}))
	publish := func() {
		t.Helper()
		staged := newScratchStagedResults(t)
		staged.AddEvent(t, "call_1", "same output")
		require.NoError(t, database.ReplaceSessionContentStaged(
			context.Background(), sessionID, []Message{{
				SessionID: sessionID, Ordinal: 0, Role: "assistant",
				Content: "running", HasToolUse: true,
				ToolCalls: []ToolCall{{
					SessionID: sessionID, ToolUseID: "call_1",
					ToolName: "exec_command", Category: "Bash", CallIndex: 0,
				}},
			}}, staged, map[string]bool{},
			func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
				return SessionSignalUpdate{}, nil, nil
			},
		))
	}

	publish()
	first, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.TranscriptRevision)
	firstRevision := *first.TranscriptRevision

	publish()
	second, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NotNil(t, second.TranscriptRevision)
	require.Equal(t, firstRevision, *second.TranscriptRevision,
		"an identical staged verification must not bump transcript revision")
}

func TestReplaceSessionContentStagedIdenticalPublishRefreshesDerivedState(
	t *testing.T,
) {
	database, err := Open(filepath.Join(t.TempDir(), "staged-derived.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	const sessionID = "codex:derived"
	firstMessage := "Warmup"
	require.NoError(t, database.UpsertSession(Session{
		ID: sessionID, Agent: "codex", Project: "project", Machine: "local",
		FirstMessage: &firstMessage, MessageCount: 1, UserMessageCount: 1,
	}))

	publish := func(outcome, rules string, finding bool) {
		t.Helper()
		staged := newScratchStagedResults(t)
		staged.AddEvent(t, "call_1", "same output")
		require.NoError(t, database.ReplaceSessionContentStaged(
			context.Background(), sessionID, []Message{{
				SessionID: sessionID, Ordinal: 0, Role: "assistant",
				Content: "running", HasToolUse: true,
				ToolCalls: []ToolCall{{
					SessionID: sessionID, ToolUseID: "call_1",
					ToolName: "exec_command", Category: "Bash", CallIndex: 0,
				}},
			}}, staged, map[string]bool{},
			func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
				update := SessionSignalUpdate{
					Outcome: outcome, OutcomeConfidence: "high",
					SecretsRulesVersion: rules,
					QualitySignals:      QualitySignals{Version: CurrentQualitySignalVersion},
				}
				if !finding {
					return update, nil, nil
				}
				update.SecretLeakCount = 1
				return update, []SecretFinding{{
					SessionID: sessionID, RuleName: "refreshed-secret",
					Confidence: "definite", LocationKind: "message",
					MessageOrdinal: 0, MatchEnd: 4,
					RedactedMatch: "****", RulesVersion: rules,
				}}, nil
			},
		))
	}

	publish("old", "old-rules", false)
	first, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.TranscriptRevision)
	firstRevision := *first.TranscriptRevision

	var firstMessageID, firstCallID, firstEventID int64
	require.NoError(t, database.getReader().QueryRow(`
		SELECT m.id, tc.id, tre.id
		FROM messages m
		JOIN tool_calls tc ON tc.message_id = m.id
		JOIN tool_result_events tre
		  ON tre.session_id = tc.session_id
		 AND tre.tool_call_message_ordinal = m.ordinal
		 AND tre.call_index = tc.call_index
		WHERE m.session_id = ?`, sessionID,
	).Scan(&firstMessageID, &firstCallID, &firstEventID))

	// Simulate metadata-only drift and stale post-processing while preserving
	// the normalized transcript and its revision.
	_, err = database.getWriter().Exec(`
		UPDATE sessions
		SET outcome = 'stale', quality_signal_version = 0,
		    secrets_rules_version = 'stale-rules',
		    last_write_incremental = 1, is_automated = 0
		WHERE id = ?`, sessionID)
	require.NoError(t, err)
	_, err = database.getWriter().Exec(`
		INSERT INTO secret_findings (
			session_id, rule_name, confidence, location_kind,
			message_ordinal, match_start, match_end, match_index,
			redacted_match, rules_version
		) VALUES (?, 'stale-secret', 'definite', 'message', 0, 0, 1, 0,
		          '*', 'stale-rules')`, sessionID)
	require.NoError(t, err)

	publish("completed", "fresh-rules", true)

	after, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.TranscriptRevision)
	require.Equal(t, firstRevision, *after.TranscriptRevision,
		"metadata-only staged repair must preserve transcript revision")
	require.Equal(t, "completed", after.Outcome)
	require.Equal(t, CurrentQualitySignalVersion, after.QualitySignalVersion)
	require.Equal(t, "fresh-rules", after.SecretsRulesVersion)
	require.Equal(t, 1, after.SecretLeakCount)
	require.True(t, after.IsAutomated,
		"unchanged staged publish must refresh automation from stored messages")
	require.False(t, after.LastWriteIncremental,
		"unchanged staged publish must clear the incremental marker")

	var messageID, callID, eventID int64
	require.NoError(t, database.getReader().QueryRow(`
		SELECT m.id, tc.id, tre.id
		FROM messages m
		JOIN tool_calls tc ON tc.message_id = m.id
		JOIN tool_result_events tre
		  ON tre.session_id = tc.session_id
		 AND tre.tool_call_message_ordinal = m.ordinal
		 AND tre.call_index = tc.call_index
		WHERE m.session_id = ?`, sessionID,
	).Scan(&messageID, &callID, &eventID))
	require.Equal(t, firstMessageID, messageID)
	require.Equal(t, firstCallID, callID)
	require.Equal(t, firstEventID, eventID)

	var findingName, findingRules string
	require.NoError(t, database.getReader().QueryRow(`
		SELECT rule_name, rules_version FROM secret_findings
		WHERE session_id = ?`, sessionID,
	).Scan(&findingName, &findingRules))
	require.Equal(t, "refreshed-secret", findingName)
	require.Equal(t, "fresh-rules", findingRules)
}

func TestReplaceSessionContentStagedWithCheckpointUsesPrefixedSessionID(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "staged-prefix.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	const storedID = "host:codex:native"
	require.NoError(t, database.UpsertSession(Session{
		ID:               storedID,
		Agent:            "codex",
		Project:          "project",
		Machine:          "local",
		MessageCount:     1,
		UserMessageCount: 1,
	}))

	staged := newScratchStagedResults(t)
	staged.AddEvent(t, "call_1", "output")
	msgs := []Message{{
		SessionID:  storedID,
		Ordinal:    0,
		Role:       "assistant",
		Content:    "running",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			SessionID: storedID,
			ToolUseID: "call_1",
			ToolName:  "exec_command",
			Category:  "Bash",
			CallIndex: 0,
		}},
	}}
	cp := &ParserCheckpoint{
		SessionID:        "codex:native",
		Agent:            "codex",
		FilePath:         "/sessions/rollout.jsonl",
		FileInode:        1,
		FileDevice:       1,
		FileMTime:        1,
		FileChangeTime:   1,
		Offset:           8,
		TailAnchorDigest: "anchor",
		Hash:             "hash",
		NextOrdinal:      0,
		Version:          ParserCheckpointVersion,
	}
	blobs := &ParserCheckpointBlobs{
		SessionID: "codex:native",
		Cursor:    []byte("cursor"),
		HashState: []byte("state"),
	}

	err = database.ReplaceSessionContentStagedWithCheckpoint(
		context.Background(), storedID, msgs, staged,
		map[string]bool{},
		func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
			return SessionSignalUpdate{}, nil, nil
		},
		cp, blobs,
	)
	require.NoError(t, err)

	var nativeCount, prefixedCount int
	require.NoError(t, database.Reader().QueryRow(
		`SELECT COUNT(*) FROM parser_checkpoints WHERE session_id = ?`,
		"codex:native",
	).Scan(&nativeCount))
	require.NoError(t, database.Reader().QueryRow(
		`SELECT COUNT(*) FROM parser_checkpoints WHERE session_id = ?`,
		storedID,
	).Scan(&prefixedCount))
	require.Zero(t, nativeCount,
		"the checkpoint must not be stored under the parser-native id")
	require.Equal(t, 1, prefixedCount,
		"the checkpoint must be stored under the rewritten session id")
}

// TestReplaceSessionContentStagedRollbackDetaches pins the failure path:
// an aborted publish must still detach the scratch schema so the next
// publish on the same writer connection can attach again.
func TestReplaceSessionContentStagedRollbackDetaches(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "staged.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	sessionID := "codex:test-session"
	require.NoError(t, database.UpsertSession(Session{
		ID:               sessionID,
		Agent:            "codex",
		Project:          "project",
		Machine:          "local",
		MessageCount:     1,
		UserMessageCount: 1,
	}))

	failing := &failInsertStagedResults{scratchStagedResults: newScratchStagedResults(t)}
	failing.AddEvent(t, "call_a", "content")
	msgs := []Message{{
		SessionID:  sessionID,
		Ordinal:    0,
		Role:       "assistant",
		Content:    "running",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolUseID: "call_a",
			ToolName:  "exec_command",
			Category:  "Bash",
			CallIndex: 0,
		}},
	}}
	err = database.ReplaceSessionContentStaged(
		context.Background(), sessionID, msgs, failing,
		map[string]bool{},
		func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
			return SessionSignalUpdate{}, nil, nil
		},
	)
	require.Error(t, err)

	// The aborted publish must have detached the staging schema.
	rows, err := database.getWriter().Query("PRAGMA database_list")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		require.NoError(t, rows.Scan(&seq, &name, &file))
		require.NotEqual(t, "codex_staging", name)
	}
	require.NoError(t, rows.Err())

	// And a follow-up publish must succeed on the same writer pool.
	staged := newScratchStagedResults(t)
	staged.AddEvent(t, "call_b", "second")
	require.NoError(t, database.ReplaceSessionContentStaged(
		context.Background(), sessionID, msgs, staged,
		map[string]bool{},
		func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
			return SessionSignalUpdate{}, nil, nil
		},
	))
}

type failInsertStagedResults struct {
	*scratchStagedResults
}

func (f *failInsertStagedResults) InsertEventsTx(
	context.Context, *sql.Tx, string, map[string]StagedToolCallPosition,
) error {
	return errors.New("injected staged failure")
}
