package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/money"
)

func TestCanonicalBunWriteReplacesDependentRowsAtomically(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "canonical-write", "alpha")
	ctx := t.Context()
	old := "old"
	oldTime := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC))
	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			require.NoError(t, ReplaceMessageRows(ctx, tx, "canonical-write", []bunmodel.Message{{
				SessionID: "canonical-write", Ordinal: 0, Role: "user",
				Content: "old message", Timestamp: &oldTime,
			}}))
			require.NoError(t, ReplaceToolRows(ctx, tx, "canonical-write", []bunmodel.ToolCall{{
				SessionID: "canonical-write", MessageOrdinal: 0,
				CallIndex: 0, ToolName: "Old", Category: "Other",
				InputJSON: &old,
			}}, nil))
			return nil
		},
	))

	newTime := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC))
	injected := errors.New("before commit")
	err := database.bunWriter.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		require.NoError(t, ReplaceMessageRows(ctx, tx, "canonical-write", []bunmodel.Message{{
			SessionID: "canonical-write", Ordinal: 2, Role: "assistant",
			Content: "new message", Timestamp: &newTime,
		}}))
		require.NoError(t, ReplaceToolRows(ctx, tx, "canonical-write", []bunmodel.ToolCall{{
			SessionID: "canonical-write", MessageOrdinal: 2,
			CallIndex: 1, ToolName: "New", Category: "Read",
		}}, []bunmodel.ToolResultEvent{{
			SessionID: "canonical-write", ToolCallMessageOrdinal: 2,
			CallIndex: 1, EventIndex: 3, Source: "tool",
			Status: "completed", Content: "new result", Timestamp: &newTime,
		}}))
		require.NoError(t, ReplaceUsageEventRows(ctx, tx, "canonical-write", []bunmodel.UsageEvent{{
			SessionID: "canonical-write", Source: "message", Model: "model",
			OutputTokens: 7, OccurredAt: &newTime, DedupKey: "usage-new",
		}}))
		require.NoError(t, ReplaceSecretFindingRows(ctx, tx, "canonical-write", []bunmodel.SecretFinding{{
			SessionID: "canonical-write", RuleName: "token", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 2, MatchStart: 0,
			MatchEnd: 3, RedactedMatch: "ne…", RulesVersion: "rules-v1",
			CreatedAt: newTime,
		}}))
		return injected
	})
	require.ErrorIs(t, err, injected)

	var messages []bunmodel.Message
	require.NoError(t, database.bunReader.NewSelect().Model(&messages).
		Where("session_id = ?", "canonical-write").Order("ordinal").Scan(ctx))
	require.Len(t, messages, 1)
	assert.Equal(t, "old message", messages[0].Content)
	var tools []bunmodel.ToolCall
	require.NoError(t, database.bunReader.NewSelect().Model(&tools).
		Where("session_id = ?", "canonical-write").Scan(ctx))
	require.Len(t, tools, 1)
	assert.Equal(t, "Old", tools[0].ToolName)
	usageCount, err := database.bunReader.NewSelect().Model((*bunmodel.UsageEvent)(nil)).
		Where("session_id = ?", "canonical-write").Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, usageCount)
	findingCount, err := database.bunReader.NewSelect().Model((*bunmodel.SecretFinding)(nil)).
		Where("session_id = ?", "canonical-write").Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, findingCount)
}

func TestCanonicalToolRowsRejectMissingLogicalParents(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "canonical-parent", "alpha")
	ctx := t.Context()

	err := database.bunWriter.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return ReplaceToolRows(ctx, tx, "canonical-parent", []bunmodel.ToolCall{{
			SessionID: "canonical-parent", MessageOrdinal: 7,
			CallIndex: 0, ToolName: "Read", Category: "Read",
		}}, nil)
	})
	require.ErrorContains(t, err, "missing message ordinal 7")

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			return ReplaceMessageRows(ctx, tx, "canonical-parent", []bunmodel.Message{{
				SessionID: "canonical-parent", Ordinal: 7, Role: "assistant",
				Content: "tool parent",
			}})
		}))
	err = database.bunWriter.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return ReplaceToolRows(ctx, tx, "canonical-parent", nil,
			[]bunmodel.ToolResultEvent{{
				SessionID: "canonical-parent", ToolCallMessageOrdinal: 7,
				CallIndex: 0, EventIndex: 0, Source: "tool", Status: "ok",
			}})
	})
	require.ErrorContains(t, err, "missing tool call (7, 0)")
}

func TestReplaceToolRowsUpdatesLogicalConflictAndDeletesStaleRows(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "canonical-tool-upsert", "alpha")
	ctx := t.Context()
	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			if err := ReplaceMessageRows(ctx, tx, "canonical-tool-upsert", []bunmodel.Message{{
				SessionID: "canonical-tool-upsert", Ordinal: 4,
				Role: "assistant", Content: "tool parent",
			}}); err != nil {
				return err
			}
			return ReplaceToolRows(ctx, tx, "canonical-tool-upsert", []bunmodel.ToolCall{
				{SessionID: "canonical-tool-upsert", MessageOrdinal: 4,
					CallIndex: 0, ToolName: "Old", Category: "Read"},
				{SessionID: "canonical-tool-upsert", MessageOrdinal: 4,
					CallIndex: 1, ToolName: "Stale", Category: "Read"},
			}, []bunmodel.ToolResultEvent{
				{SessionID: "canonical-tool-upsert", ToolCallMessageOrdinal: 4,
					CallIndex: 0, EventIndex: 0, Source: "tool", Status: "old"},
				{SessionID: "canonical-tool-upsert", ToolCallMessageOrdinal: 4,
					CallIndex: 1, EventIndex: 0, Source: "tool", Status: "stale"},
			})
		}))

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			return ReplaceToolRows(ctx, tx, "canonical-tool-upsert", []bunmodel.ToolCall{{
				SessionID: "canonical-tool-upsert", MessageOrdinal: 4,
				CallIndex: 0, ToolName: "Updated", Category: "Read",
			}}, []bunmodel.ToolResultEvent{{
				SessionID: "canonical-tool-upsert", ToolCallMessageOrdinal: 4,
				CallIndex: 0, EventIndex: 0, Source: "tool", Status: "updated",
			}})
		}))

	var calls []bunmodel.ToolCall
	require.NoError(t, database.bunReader.NewSelect().Model(&calls).
		Where("session_id = ?", "canonical-tool-upsert").Scan(ctx))
	require.Len(t, calls, 1)
	assert.Equal(t, "Updated", calls[0].ToolName)
	var results []bunmodel.ToolResultEvent
	require.NoError(t, database.bunReader.NewSelect().Model(&results).
		Where("session_id = ?", "canonical-tool-upsert").Scan(ctx))
	require.Len(t, results, 1)
	assert.Equal(t, "updated", results[0].Status)
}

func TestCanonicalBunRowsPreservePortableCoordinates(t *testing.T) {
	cost := money.Money{Microdollars: 42}
	messages, calls, results, err := CanonicalMessageRows([]Message{{
		ID: 99, SessionID: "portable", Ordinal: 4, Role: "assistant",
		Content: "answer", Timestamp: "2026-08-03T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Read", Category: "Read", InputJSON: `{"path":"file"}`,
			ResultEvents: []ToolResultEvent{{
				EventIndex: 7, Source: "tool", Status: "completed",
				Content: "result", Timestamp: "2026-08-03T12:00:01Z",
			}},
		}},
	}})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Nil(t, messages[0].ID, "replication uses the portable message key")
	require.Len(t, calls, 1)
	assert.Equal(t, 4, calls[0].MessageOrdinal)
	assert.Equal(t, 0, calls[0].CallIndex)
	require.Len(t, results, 1)
	assert.Equal(t, 7, results[0].EventIndex)

	usage, err := CanonicalUsageEventRows([]UsageEvent{{
		ID: 12, SessionID: "portable", Source: "message", Model: "model",
		Cost: &cost, OccurredAt: "2026-08-03T12:00:02Z", DedupKey: "dedup",
	}})
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, int64(12), usage[0].ID, "mirrors can preserve source row ids")
	require.NotNil(t, usage[0].CostMicrodollars)
	assert.Equal(t, int64(42), *usage[0].CostMicrodollars)
}

func TestUpsertSessionRowUsesCanonicalReplacementColumns(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	_, err := database.bunWriter.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive-a", SourceArchiveSalt: "salt-a",
	}).Exec(ctx)
	require.NoError(t, err)
	row, err := CanonicalSessionRow(Session{
		ID: "canonical-session", Project: "alpha", Machine: "machine-a",
		Agent: "codex", CreatedAt: "2026-08-04T10:00:00Z",
		SourceArchiveID: "archive-a", SourceDatabaseGeneration: "generation-a",
	})
	require.NoError(t, err)
	require.NoError(t, UpsertSessionRow(ctx, database.bunWriter, row))

	row.Project = "beta"
	row.AgentLabel = "reviewer"
	row.ParserParentSessionID = Ptr("parser-parent")
	row.SourceDatabaseGeneration = "generation-b"
	require.NoError(t, UpsertSessionRow(ctx, database.bunWriter, row))

	var stored bunmodel.Session
	require.NoError(t, database.bunReader.NewSelect().Model(&stored).
		Where("id = ?", row.ID).Scan(ctx))
	assert.Equal(t, "beta", stored.Project)
	assert.Equal(t, "reviewer", stored.AgentLabel)
	assert.Equal(t, "parser-parent", *stored.ParserParentSessionID)
	assert.Equal(t, "generation-b", stored.SourceDatabaseGeneration)
}

func TestCanonicalSessionRowSanitizesPortableText(t *testing.T) {
	firstMessage := "first\x00message"
	displayName := "display\x00name"
	deletionCause := "source\x00missing"
	filePath := "/tmp/session\x00.jsonl"
	row, err := CanonicalSessionRow(Session{
		ID: "session-text", Project: "pro\x00ject", Machine: "ma\x00chine",
		Agent: "co\x00dex", AgentLabel: "Co\x00dex", Entrypoint: "c\x00li",
		SessionKind: "inter\x00active", FirstMessage: &firstMessage,
		DisplayName: &displayName, RelationshipType: "sub\x00agent",
		Outcome: "suc\x00cess", OutcomeConfidence: "h\x00igh",
		EndedWithRole: "assis\x00tant", Cwd: "/work\x00space",
		GitBranch: "ma\x00in", SourceSessionID: "source\x00-session",
		SourceVersion: "1.\x002", TranscriptFidelity: "ex\x00act",
		DeletionCause: &deletionCause, FilePath: &filePath,
		CreatedAt: "2026-08-04T10:00:00Z",
	})
	require.NoError(t, err)

	assert.Equal(t, "project", row.Project)
	assert.Equal(t, "machine", row.Machine)
	assert.Equal(t, "codex", row.Agent)
	assert.Equal(t, "Codex", row.AgentLabel)
	assert.Equal(t, "cli", row.Entrypoint)
	assert.Equal(t, "interactive", row.SessionKind)
	require.NotNil(t, row.FirstMessage)
	assert.Equal(t, "firstmessage", *row.FirstMessage)
	require.NotNil(t, row.DisplayName)
	assert.Equal(t, "displayname", *row.DisplayName)
	assert.Equal(t, "subagent", row.RelationshipType)
	assert.Equal(t, "success", row.Outcome)
	assert.Equal(t, "high", row.OutcomeConfidence)
	assert.Equal(t, "assistant", row.EndedWithRole)
	assert.Equal(t, "/workspace", row.Cwd)
	assert.Equal(t, "main", row.GitBranch)
	assert.Equal(t, "source-session", row.SourceSessionID)
	assert.Equal(t, "1.2", row.SourceVersion)
	assert.Equal(t, "exact", row.TranscriptFidelity)
	require.NotNil(t, row.DeletionCause)
	assert.Equal(t, "sourcemissing", *row.DeletionCause)
	require.NotNil(t, row.FilePath)
	assert.Equal(t, "/tmp/session.jsonl", *row.FilePath)
}

func TestCanonicalBunWriteSessionBatchUsesPortableTimestampPrecision(t *testing.T) {
	database := testDB(t)
	const (
		sessionID  = "canonical-batch-precision"
		sourceTime = "2026-08-03T12:00:00.123456789Z"
		wantTime   = "2026-08-03T12:00:00.123456Z"
	)

	result, err := database.WriteSessionBatchAtomic([]SessionBatchWrite{{
		Session: Session{
			ID: sessionID, Project: "alpha", Machine: defaultMachine,
			Agent: defaultAgent, StartedAt: Ptr(sourceTime), MessageCount: 1,
		},
		Messages: []Message{{
			SessionID: sessionID, Ordinal: 0, Role: "assistant",
			Content: "answer", ContentLength: 6, Timestamp: sourceTime,
			ToolCalls: []ToolCall{{
				ToolName: "Read", Category: "Read",
				ResultEvents: []ToolResultEvent{{
					Source: "tool", Status: "completed", Content: "result",
					ContentLength: 6, Timestamp: sourceTime,
				}},
			}},
		}},
		UsageEvents: []UsageEvent{{
			SessionID: sessionID, Source: "message", Model: "model",
			OccurredAt: sourceTime, DedupKey: "usage",
		}},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)
	session, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session.StartedAt)
	assert.Equal(t, wantTime, *session.StartedAt)

	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, wantTime, messages[0].Timestamp)
	require.Len(t, messages[0].ToolCalls, 1)
	require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, wantTime, messages[0].ToolCalls[0].ResultEvents[0].Timestamp)

	usage, err := database.GetUsageEvents(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, wantTime, usage[0].OccurredAt)
}

func TestCanonicalBunWriteSessionPlaceholderUsesPortableTimestampPrecision(
	t *testing.T,
) {
	database := testDB(t)
	const (
		sessionID  = "canonical-placeholder-precision"
		sourceTime = "2026-08-03T12:00:00.123456789Z"
		wantTime   = "2026-08-03T12:00:00.123456Z"
	)
	firstMessage := "placeholder"
	displayName := "must remain target-owned"
	require.NoError(t, database.insertSessionIfAbsent(t.Context(), Session{
		ID: sessionID, Project: "alpha", Machine: "recall-import",
		Agent: "codex", FirstMessage: &firstMessage, DisplayName: &displayName,
		StartedAt: Ptr(sourceTime), EndedAt: Ptr(sourceTime),
		SourceVersion: "recall-import-placeholder",
	}))

	got, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got.StartedAt)
	require.NotNil(t, got.EndedAt)
	assert.Equal(t, wantTime, *got.StartedAt)
	assert.Equal(t, wantTime, *got.EndedAt)
	assert.Nil(t, got.DisplayName,
		"archive ingestion must not write target-owned display names")

	require.NoError(t, database.insertSessionIfAbsent(t.Context(), Session{
		ID: sessionID, Project: "replacement", Machine: "replacement",
		Agent: "replacement", StartedAt: Ptr("2026-08-04T00:00:00Z"),
	}))
	got, err = database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Project,
		"placeholder insertion must not overwrite an existing session")
}
