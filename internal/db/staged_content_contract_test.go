package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestStagedPublishWithinSQLiteVariableLimit(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
		return nil
	}))
	require.NoError(t, conn.Close())

	staged := newScratchStagedResults(t)
	calls := make([]ToolCall, 501)
	for i := range calls {
		calls[i] = ToolCall{
			ToolUseID: fmt.Sprintf("call-%d", i), ToolName: "exec_command", Category: "Bash",
		}
	}
	msgs := []Message{{SessionID: "s1", Ordinal: 0, Role: "assistant", ToolCalls: calls}}
	require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), "s1", msgs, staged, nil, nil))
	stored, err := d.GetAllMessages(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Len(t, stored[0].ToolCalls, len(calls))
	for i, call := range stored[0].ToolCalls {
		require.Equal(t, calls[i].ToolUseID, call.ToolUseID)
	}
}

type cancellingStagedResults struct {
	*scratchStagedResults
	cancel      context.CancelFunc
	phase       string
	receivedErr error
}

func (s *cancellingStagedResults) ResolveSummary(
	ctx context.Context, key string,
) (string, int, error) {
	if s.phase == "summary" {
		s.cancel()
		s.receivedErr = ctx.Err()
		return "", 0, ctx.Err()
	}
	return s.scratchStagedResults.ResolveSummary(ctx, key)
}

func (s *cancellingStagedResults) InsertEventsTx(
	ctx context.Context, tx *sql.Tx, id string,
	positions map[string]StagedToolCallPosition,
) error {
	if s.phase == "events" {
		s.cancel()
		s.receivedErr = ctx.Err()
		if s.receivedErr != nil {
			return s.receivedErr
		}
	}
	return s.scratchStagedResults.InsertEventsTx(ctx, tx, id, positions)
}

func TestStagedPublishCancellationPreservesTranscript(t *testing.T) {
	for _, phase := range []string{"summary", "events"} {
		t.Run(phase, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project")
			insertMessages(t, d, Message{
				SessionID: "s1", Ordinal: 0, Role: "user", Content: "original",
			})
			scratch := newScratchStagedResults(t)
			scratch.AddEvent(t, "call", "replacement output")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			staged := &cancellingStagedResults{
				scratchStagedResults: scratch, cancel: cancel, phase: phase,
			}
			msgs := []Message{{
				SessionID: "s1", Ordinal: 0, Role: "assistant", Content: "replacement",
				ToolCalls: []ToolCall{{
					ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
				}},
			}}
			err := d.ReplaceSessionContentStaged(ctx, "s1", msgs, staged, nil, func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
				return SessionSignalUpdate{}, nil, nil
			})
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, staged.receivedErr, context.Canceled, "cancellation must reach the staging operation")
			stored, err := d.GetAllMessages(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, "original", stored[0].Content)
		})
	}
}

func TestDetachStagedConnAfterCancellationKeepsWriter(t *testing.T) {
	d := testDB(t)
	scratch := newScratchStagedResults(t)
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `
		CREATE TEMP TABLE writer_probe(value TEXT);
		INSERT INTO writer_probe VALUES ('same connection')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "ATTACH DATABASE ? AS "+stagedAttachName, scratch.Path())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	detachStagedConn(ctx, conn)
	var value string
	require.NoError(t, conn.QueryRowContext(t.Context(), "SELECT value FROM writer_probe").Scan(&value))
	require.Equal(t, "same connection", value)
	// A new attachment succeeds only if cancellation did not leave the old one attached.
	_, err = conn.ExecContext(t.Context(), "ATTACH DATABASE ? AS "+stagedAttachName, scratch.Path())
	require.NoError(t, err)
	detachStagedConn(t.Context(), conn)
}

func TestStagedPublishWithoutSignalsInvalidatesChangedTranscript(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	staged := newScratchStagedResults(t)
	staged.AddEvent(t, "call", "output")
	msgs := []Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant", Content: "original",
		ToolCalls: []ToolCall{{ToolUseID: "call", ToolName: "exec_command", Category: "Bash"}},
	}}
	require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), "s1", msgs, staged, nil,
		func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
			return SessionSignalUpdate{
				QualitySignals:  QualitySignals{Version: CurrentQualitySignalVersion},
				SecretLeakCount: 1, SecretsRulesVersion: "test-rules",
			}, []SecretFinding{{
				SessionID: "s1", RuleName: "test-secret", Confidence: "definite",
				LocationKind: "message", MessageOrdinal: 0, MatchEnd: 4,
				RedactedMatch: "****", RulesVersion: "test-rules",
			}}, nil
		}))
	originalRevision, err := d.TranscriptRevision("s1")
	require.NoError(t, err)

	// A no-op publish can retain findings for the same transcript even when
	// recomputation is disabled. A changed transcript must become stale.
	for _, content := range []string{"original", "replacement"} {
		msgs[0].Content = content
		require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), "s1", msgs, staged, nil, nil))
		stored, err := d.GetAllMessages(t.Context(), "s1")
		require.NoError(t, err)
		require.Len(t, stored, 1)
		require.Equal(t, content, stored[0].Content)
		session, err := d.GetSessionFull(t.Context(), "s1")
		require.NoError(t, err)
		require.NotNil(t, session)
		findings, err := d.SessionSecretFindings(t.Context(), "s1")
		require.NoError(t, err)
		revision, err := d.TranscriptRevision("s1")
		require.NoError(t, err)
		if content == "original" {
			require.Equal(t, originalRevision, revision)
			require.Len(t, findings, 1)
			require.Equal(t, 1, session.SecretLeakCount)
			require.Equal(t, "test-rules", session.SecretsRulesVersion)
			require.Equal(t, CurrentQualitySignalVersion, session.QualitySignalVersion)
		} else {
			require.NotEqual(t, originalRevision, revision)
			require.Empty(t, findings)
			require.Zero(t, session.SecretLeakCount)
			require.Empty(t, session.SecretsRulesVersion)
			require.Zero(t, session.QualitySignalVersion)
		}
	}
}
