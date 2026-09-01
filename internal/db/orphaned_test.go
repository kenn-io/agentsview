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

func TestCopySyncStatePreservesArtifactImportAuthority(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	work := artifactImportTestWork("peer-a1b2c3", 2)
	require.NoError(t, source.EnqueueArtifactImport(ctx, work))
	attempt, err := source.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	pending, err := source.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	marked, err := source.MarkArtifactImportAttempted(
		ctx, pending[0], attempt,
	)
	require.NoError(t, err)
	require.True(t, marked)
	marked, err = source.MarkArtifactImportQuarantinePending(ctx, pending[0])
	require.NoError(t, err)
	require.True(t, marked)

	head := ArtifactPeerCheckpointHead{
		Origin:           work.Origin,
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("d", 64),
		CheckpointSize:   99,
	}
	_, err = source.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	landing := ArtifactCheckpointLanding(head)
	sessionMap := map[string]string{
		head.Origin + "~one": strings.Repeat("e", 64),
	}
	imported := ArtifactImportedSession{
		Origin:            head.Origin,
		GID:               head.Origin + "~one",
		ManifestHash:      sessionMap[head.Origin+"~one"],
		ImportedSessionID: head.Origin + "~one",
	}
	require.NoError(t, source.RecordArtifactImportedSession(ctx, imported))
	require.NoError(t, source.BeginArtifactCheckpointStage(ctx, landing, 1))
	require.NoError(t, source.StageArtifactCheckpointSessions(
		ctx, landing, []ArtifactCheckpointSession{{
			GID: imported.GID, ManifestHash: imported.ManifestHash,
		}},
	))
	require.NoError(t, source.CompleteArtifactCheckpointStage(
		ctx, landing, 1,
	))
	require.NoError(t, source.RecordArtifactCheckpointLandingFromStage(
		ctx, landing,
	))
	partial := ArtifactCheckpointLanding{
		Origin: "peer-b2c3d4", Sequence: 1,
		CheckpointSHA256: strings.Repeat("f", 64),
		CheckpointSize:   88,
	}
	_, err = source.RecordArtifactPeerCheckpointHead(
		ctx, ArtifactPeerCheckpointHead(partial),
	)
	require.NoError(t, err)
	require.NoError(t, source.BeginArtifactCheckpointStage(ctx, partial, 2))
	require.NoError(t, source.StageArtifactCheckpointSessionPage(
		ctx, partial,
		[]ArtifactCheckpointSession{{
			GID:          partial.Origin + "~partial",
			ManifestHash: strings.Repeat("1", 64),
		}},
		0, 42,
	))
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	require.NoError(t, destination.CopySyncStateFrom(sourcePath))

	nextAttempt, err := destination.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	assert.Greater(t, nextAttempt, attempt)
	pending, err = destination.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		nextAttempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, work.Name, pending[0].Name)
	assert.True(t, pending[0].QuarantinePending)

	gotHead, found, err := destination.GetArtifactPeerCheckpointHead(
		ctx, head.Origin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, head, gotHead)
	gotLanding, gotMap, found, err :=
		destination.GetArtifactCheckpointLanding(ctx, head.Origin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, landing, gotLanding)
	assert.Equal(t, sessionMap, gotMap)
	gotProvenance, err := destination.ArtifactImportedManifestHashes(
		ctx, head.Origin, []string{imported.GID},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		imported.GID: imported.ManifestHash,
	}, gotProvenance)
	require.NoError(t, destination.BeginArtifactCheckpointStage(ctx, partial, 2))
	progress, err := destination.ArtifactCheckpointStageProgress(ctx, partial)
	require.NoError(t, err)
	assert.False(t, progress.Complete)
	assert.Equal(t, 1, progress.DecodedCount)
	assert.Equal(t, int64(42), progress.DecodeOffset)
}

func TestFullResyncPreservesImportedSessionUsage(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	origin := "peer-a1b2c3"
	gid := origin + "~native-session"
	insertSession(t, source, gid, "project", func(session *Session) {
		session.Machine = origin
	})
	ordinal := 2
	require.NoError(t, source.ReplaceSessionUsageEvents(gid, []UsageEvent{{
		SessionID:                gid,
		MessageOrdinal:           &ordinal,
		Source:                   "artifact",
		Model:                    "example-model",
		InputTokens:              101,
		OutputTokens:             29,
		CacheCreationInputTokens: 11,
		CacheReadInputTokens:     7,
		ReasoningTokens:          5,
		CostStatus:               "reported",
		CostSource:               "provider",
		OccurredAt:               "2026-07-29T12:00:00Z",
		DedupKey:                 "imported-usage",
	}}))

	head := ArtifactPeerCheckpointHead{
		Origin:           origin,
		Sequence:         3,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   123,
	}
	_, err := source.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	manifestHash := strings.Repeat("b", 64)
	require.NoError(t, source.RecordArtifactCheckpointLanding(
		ctx,
		ArtifactCheckpointLanding(head),
		map[string]string{gid: manifestHash},
	))
	require.NoError(t, source.RecordArtifactImportedSession(
		ctx,
		ArtifactImportedSession{
			Origin:            origin,
			GID:               gid,
			ManifestHash:      manifestHash,
			ImportedSessionID: gid,
		},
	))
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	require.NoError(t, destination.CopySyncStateFrom(sourcePath))
	copied, err := destination.CopyOrphanedDataFrom(sourcePath)
	require.NoError(t, err)
	require.Equal(t, 1, copied)

	events, err := destination.GetUsageEvents(ctx, gid)
	require.NoError(t, err)
	require.Len(t, events, 1)
	events[0].ID = 0
	assert.Equal(t, UsageEvent{
		SessionID:                gid,
		MessageOrdinal:           &ordinal,
		Source:                   "artifact",
		Model:                    "example-model",
		InputTokens:              101,
		OutputTokens:             29,
		CacheCreationInputTokens: 11,
		CacheReadInputTokens:     7,
		ReasoningTokens:          5,
		CostStatus:               "reported",
		CostSource:               "provider",
		OccurredAt:               "2026-07-29T12:00:00Z",
		DedupKey:                 "imported-usage",
	}, events[0])

	provenance, err := destination.ArtifactImportedManifestHashes(
		ctx, origin, []string{gid},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{gid: manifestHash}, provenance)
}

func TestCopySyncStateAcceptsDatabaseWithoutArtifactImportTables(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "old.db")
	source, err := sql.Open("sqlite3", makeDSN(sourcePath, false))
	require.NoError(t, err)
	_, err = source.Exec(`CREATE TABLE pg_sync_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	require.NoError(t, destination.CopySyncStateFrom(sourcePath))
}

func TestCopySyncStateRejectsEqualLandingWithDifferentMap(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   42,
	}
	_, err := source.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	require.NoError(t, source.RecordArtifactCheckpointLanding(
		ctx,
		ArtifactCheckpointLanding(head),
		map[string]string{head.Origin + "~source": strings.Repeat("b", 64)},
	))
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	_, err = destination.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	require.NoError(t, destination.RecordArtifactCheckpointLanding(
		ctx,
		ArtifactCheckpointLanding(head),
		map[string]string{head.Origin + "~destination": strings.Repeat("c", 64)},
	))

	err = destination.CopySyncStateFrom(sourcePath)
	require.ErrorIs(t, err, ErrArtifactImportConflict)
}

func TestCopySyncStateRejectsIncompatibleCheckpointStages(t *testing.T) {
	tests := []struct {
		name               string
		sourceEntries      []ArtifactCheckpointSession
		destinationEntries []ArtifactCheckpointSession
		sourceOffset       int64
		destinationOffset  int64
		complete           bool
	}{
		{
			name: "complete stages have different session sets",
			sourceEntries: []ArtifactCheckpointSession{
				{GID: "peer-a1b2c3~one", ManifestHash: strings.Repeat("1", 64)},
				{GID: "peer-a1b2c3~two", ManifestHash: strings.Repeat("2", 64)},
			},
			destinationEntries: []ArtifactCheckpointSession{
				{GID: "peer-a1b2c3~one", ManifestHash: strings.Repeat("1", 64)},
				{GID: "peer-a1b2c3~three", ManifestHash: strings.Repeat("3", 64)},
			},
			complete: true,
		},
		{
			name: "partial stages have different decoded prefixes",
			sourceEntries: []ArtifactCheckpointSession{
				{GID: "peer-a1b2c3~one", ManifestHash: strings.Repeat("1", 64)},
			},
			destinationEntries: []ArtifactCheckpointSession{
				{GID: "peer-a1b2c3~two", ManifestHash: strings.Repeat("2", 64)},
			},
			sourceOffset:      10,
			destinationOffset: 10,
		},
		{
			name: "partial stage count and cursor progress cross",
			sourceEntries: []ArtifactCheckpointSession{
				{GID: "peer-a1b2c3~one", ManifestHash: strings.Repeat("1", 64)},
				{GID: "peer-a1b2c3~two", ManifestHash: strings.Repeat("2", 64)},
			},
			destinationEntries: []ArtifactCheckpointSession{
				{GID: "peer-a1b2c3~one", ManifestHash: strings.Repeat("1", 64)},
			},
			sourceOffset:      10,
			destinationOffset: 20,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.db")
			source := testDBAtPath(t, sourcePath, "source")
			landing := ArtifactCheckpointLanding{
				Origin:           "peer-a1b2c3",
				Sequence:         4,
				CheckpointSHA256: strings.Repeat("a", 64),
				CheckpointSize:   91,
			}
			stageCheckpointForCopyTest(
				t, source, landing, tc.sourceEntries,
				tc.sourceOffset, tc.complete,
			)
			require.NoError(t, source.Close())

			destination := testDBAtPath(
				t, filepath.Join(dir, "destination.db"), "destination",
			)
			defer destination.Close()
			stageCheckpointForCopyTest(
				t, destination, landing, tc.destinationEntries,
				tc.destinationOffset, tc.complete,
			)

			err := destination.CopySyncStateFrom(sourcePath)
			require.ErrorIs(t, err, ErrArtifactImportConflict)
		})
	}
}

func TestCopySyncStateMergesCompatibleCheckpointStagePrefix(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	landing := ArtifactCheckpointLanding{
		Origin:           "peer-a1b2c3",
		Sequence:         4,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   91,
	}
	one := ArtifactCheckpointSession{
		GID: landing.Origin + "~one", ManifestHash: strings.Repeat("1", 64),
	}
	two := ArtifactCheckpointSession{
		GID: landing.Origin + "~two", ManifestHash: strings.Repeat("2", 64),
	}
	stageCheckpointForCopyTest(
		t, source, landing, []ArtifactCheckpointSession{one, two}, 20, false,
	)
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	stageCheckpointForCopyTest(
		t, destination, landing, []ArtifactCheckpointSession{one}, 10, false,
	)

	require.NoError(t, destination.CopySyncStateFrom(sourcePath))
	progress, err := destination.ArtifactCheckpointStageProgress(ctx, landing)
	require.NoError(t, err)
	assert.False(t, progress.Complete)
	assert.Equal(t, 2, progress.DecodedCount)
	assert.Equal(t, int64(20), progress.DecodeOffset)
	var stagedCount int
	require.NoError(t, destination.getReader().QueryRowContext(ctx, `
		SELECT count(*)
		FROM artifact_checkpoint_stage_sessions
		WHERE origin = ? AND sequence = ?`,
		landing.Origin, landing.Sequence,
	).Scan(&stagedCount))
	assert.Equal(t, 2, stagedCount)
}

func stageCheckpointForCopyTest(
	t *testing.T,
	database *DB,
	landing ArtifactCheckpointLanding,
	entries []ArtifactCheckpointSession,
	offset int64,
	complete bool,
) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, database.BeginArtifactCheckpointStage(ctx, landing, 1))
	if complete {
		require.NoError(t, database.StageArtifactCheckpointSessions(
			ctx, landing, entries,
		))
		require.NoError(t, database.CompleteArtifactCheckpointStage(
			ctx, landing, len(entries),
		))
		return
	}
	require.NoError(t, database.StageArtifactCheckpointSessionPage(
		ctx, landing, entries, 0, offset,
	))
}

func TestExecWithoutCancelDropsTempTableWithCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open sqlite")
	defer pool.Close()

	baseCtx := context.Background()
	conn, err := pool.Conn(baseCtx)
	require.NoError(t, err, "pin sqlite connection")
	defer conn.Close()

	_, err = conn.ExecContext(baseCtx, `
		CREATE TEMP TABLE _test_cleanup (
			id TEXT PRIMARY KEY
		)`)
	require.NoError(t, err, "create temp table")

	ctx, cancel := context.WithCancel(baseCtx)
	cancel()

	_, err = execWithoutCancel(ctx, conn,
		"DROP TABLE IF EXISTS _test_cleanup")
	require.NoError(t, err, "drop with canceled context")

	_, err = conn.ExecContext(baseCtx, `
		CREATE TEMP TABLE _test_cleanup (
			id TEXT PRIMARY KEY
		)`)
	require.NoError(t, err, "recreate temp table after cleanup")
}

func TestCopyOrphanedDataPreservesCanonicalRowsWithFreshPhysicalIDs(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	const (
		sessionID   = "canonical-orphan"
		searchToken = "xylophoniccopytoken"
		createdAt   = "2020-01-02T03:04:05.000Z"
	)
	terminationStatus := "clean"
	fileInode := int64(4242)
	fileDevice := int64(84)

	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	insertSession(t, source, sessionID, "project", func(session *Session) {
		session.MessageCount = 1
		session.TerminationStatus = &terminationStatus
		session.FileInode = &fileInode
		session.FileDevice = &fileDevice
	})
	insertMessages(t, source, Message{
		SessionID: sessionID, Ordinal: 0, Role: "assistant",
		Content: searchToken, SourceUUID: "canonical-source-message",
		ToolCalls: []ToolCall{{
			ToolUseID: "canonical-tool", ToolName: "Read", Category: "file",
			ResultEvents: []ToolResultEvent{{
				EventIndex: 7, Source: "tool", Status: "completed",
				Content: "canonical result",
			}},
		}},
	})
	ordinal := 0
	require.NoError(t, source.ReplaceSessionUsageEvents(sessionID, []UsageEvent{{
		SessionID: sessionID, MessageOrdinal: &ordinal, Source: "session",
		Model: "canonical-model", InputTokens: 11, OutputTokens: 5,
		DedupKey: "canonical-usage",
	}}))
	require.NoError(t, source.ReplaceSessionSecretFindings(
		sessionID, []SecretFinding{{
			SessionID: sessionID, RuleName: "canonical-rule",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: 1, MatchEnd: 3,
			RedactedMatch: "xy…ic",
		}}, 1, "canonical-rules",
	))
	_, err := source.getWriter().ExecContext(ctx,
		`UPDATE secret_findings SET created_at = ? WHERE session_id = ?`,
		createdAt, sessionID,
	)
	require.NoError(t, err)
	sourceMessages, err := source.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, sourceMessages, 1)
	_, err = source.PinMessage(sessionID, sourceMessages[0].ID, Ptr("keep"))
	require.NoError(t, err)
	sourceIDs := canonicalCopyPhysicalIDs(t, source, sessionID)
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	seedCanonicalCopyCollision(t, destination)
	destinationIDs := canonicalCopyPhysicalIDs(t, destination, "occupied")
	assert.Equal(t, sourceIDs, destinationIDs,
		"fixture must collide on every source-assigned physical child ID")

	copied, err := destination.CopyOrphanedDataFrom(sourcePath)
	require.NoError(t, err)
	require.Equal(t, 1, copied)

	session, err := destination.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.TerminationStatus)
	assert.Equal(t, terminationStatus, *session.TerminationStatus)
	require.NotNil(t, session.FileInode)
	assert.Equal(t, fileInode, *session.FileInode)
	require.NotNil(t, session.FileDevice)
	assert.Equal(t, fileDevice, *session.FileDevice)

	copiedIDs := canonicalCopyPhysicalIDs(t, destination, sessionID)
	assert.NotEqual(t, sourceIDs, copiedIDs,
		"destination must assign fresh physical child IDs")
	messages, err := destination.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, 7, messages[0].ToolCalls[0].ResultEvents[0].EventIndex)
	usage, err := destination.GetUsageEvents(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "canonical-usage", usage[0].DedupKey)
	findings, err := destination.SessionSecretFindings(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "canonical-rule", findings[0].RuleName)
	var copiedCreatedAt string
	require.NoError(t, destination.getReader().QueryRowContext(ctx,
		`SELECT created_at FROM secret_findings WHERE session_id = ?`, sessionID,
	).Scan(&copiedCreatedAt))
	assert.Equal(t, createdAt, copiedCreatedAt)
	pins, err := destination.ListPinnedMessages(ctx, sessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 0, pins[0].Ordinal)
	ordinals, err := destination.SearchSession(ctx, sessionID, searchToken)
	require.NoError(t, err)
	assert.Equal(t, []int{0}, ordinals)
}

type canonicalCopyIDs struct {
	message int64
	usage   int64
	result  int64
	finding int64
}

func canonicalCopyPhysicalIDs(
	t *testing.T, database *DB, sessionID string,
) canonicalCopyIDs {
	t.Helper()
	var ids canonicalCopyIDs
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT
			(SELECT id FROM messages WHERE session_id = ?),
			(SELECT id FROM usage_events WHERE session_id = ?),
			(SELECT id FROM tool_result_events WHERE session_id = ?),
			(SELECT id FROM secret_findings WHERE session_id = ?)`,
		sessionID, sessionID, sessionID, sessionID,
	).Scan(&ids.message, &ids.usage, &ids.result, &ids.finding))
	return ids
}

func seedCanonicalCopyCollision(t *testing.T, database *DB) {
	t.Helper()
	const sessionID = "occupied"
	insertSession(t, database, sessionID, "project")
	insertMessages(t, database, Message{
		SessionID: sessionID, Ordinal: 0, Role: "assistant", Content: "occupied",
		ToolCalls: []ToolCall{{
			ToolUseID: "occupied-tool", ToolName: "Read", Category: "file",
			ResultEvents: []ToolResultEvent{{
				Source: "tool", Status: "completed", Content: "occupied result",
			}},
		}},
	})
	require.NoError(t, database.ReplaceSessionUsageEvents(sessionID, []UsageEvent{{
		SessionID: sessionID, Source: "session", DedupKey: "occupied-usage",
	}}))
	require.NoError(t, database.ReplaceSessionSecretFindings(
		sessionID, []SecretFinding{{
			SessionID: sessionID, RuleName: "occupied-rule",
			Confidence: "definite", LocationKind: "message",
		}}, 1, "occupied-rules",
	))
}

func TestCopyOrphanedDataLateDependentFailureRollsBackEverything(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	const orphanID = "late-orphan"

	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	insertSession(t, source, "existing", "project")
	insertMessages(t, source, userMsg("existing", 0, "source transcript"))
	_, err := source.getWriter().ExecContext(ctx,
		`UPDATE sessions SET transcript_revision = '8' WHERE id = 'existing'`)
	require.NoError(t, err)
	insertSession(t, source, orphanID, "project")
	insertMessages(t, source, Message{
		SessionID: orphanID, Ordinal: 0, Role: "assistant",
		Content: "late rollback token", SourceUUID: "late-message",
		ToolCalls: []ToolCall{{
			ToolUseID: "late-tool", ToolName: "Read", Category: "file",
			ResultEvents: []ToolResultEvent{{
				Source: "tool", Status: "completed", Content: "late result",
			}},
		}},
	})
	orphanMessages, err := source.GetAllMessages(ctx, orphanID)
	require.NoError(t, err)
	require.Len(t, orphanMessages, 1)
	_, err = source.PinMessage(orphanID, orphanMessages[0].ID, Ptr("late pin"))
	require.NoError(t, err)
	require.NoError(t, source.ReplaceSessionSecretFindings(
		orphanID, []SecretFinding{{
			SessionID: orphanID, RuleName: "late-rule",
			Confidence: "definite", LocationKind: "message",
		}}, 1, "late-rules",
	))
	require.NoError(t, source.ReplaceSessionUsageEvents(orphanID, []UsageEvent{{
		SessionID: orphanID, Source: "session", DedupKey: "late-usage",
	}}))
	_, err = source.getWriter().ExecContext(ctx, `
		DROP INDEX idx_usage_events_dedup;
		INSERT INTO usage_events (
			session_id, message_ordinal, source, model,
			input_tokens, output_tokens, cache_creation_input_tokens,
			cache_read_input_tokens, reasoning_tokens, cost_microdollars,
			cost_status, cost_source, occurred_at, dedup_key
		)
		SELECT
			session_id, message_ordinal, source, model,
			input_tokens, output_tokens, cache_creation_input_tokens,
			cache_read_input_tokens, reasoning_tokens, cost_microdollars,
			cost_status, cost_source, occurred_at, dedup_key
		FROM usage_events WHERE session_id = 'late-orphan'`)
	require.NoError(t, err, "plant conflicting legacy usage rows")
	require.NoError(t, source.Close())

	destination := testDBAtPath(
		t, filepath.Join(dir, "destination.db"), "destination",
	)
	defer destination.Close()
	insertSession(t, destination, "existing", "project")
	insertMessages(t, destination, userMsg("existing", 0, "destination transcript"))
	_, err = destination.getWriter().ExecContext(ctx,
		`UPDATE sessions SET transcript_revision = '3' WHERE id = 'existing'`)
	require.NoError(t, err)
	_, err = destination.CopyOrphanedDataFrom(sourcePath)
	require.ErrorContains(t, err, "copying attached usage_events")

	session, err := destination.GetSession(ctx, orphanID)
	require.NoError(t, err)
	assert.Nil(t, session)
	for _, table := range []string{
		"messages", "tool_calls", "tool_result_events", "pinned_messages",
		"usage_events", "secret_findings",
	} {
		var count int
		require.NoError(t, destination.getReader().QueryRowContext(ctx,
			`SELECT count(*) FROM `+quoteCommonIdentifier(table)+` WHERE session_id = ?`,
			orphanID,
		).Scan(&count))
		assert.Zero(t, count, table)
	}
	ordinals, err := destination.SearchSession(ctx, orphanID, "rollback")
	require.NoError(t, err)
	assert.Empty(t, ordinals)
	var revision string
	require.NoError(t, destination.getReader().QueryRowContext(ctx,
		`SELECT transcript_revision FROM sessions WHERE id = 'existing'`,
	).Scan(&revision))
	assert.Equal(t, "3", revision,
		"revision reconciliation must roll back with the failed orphan copy")
}

func TestCopyOrphanedDataPreservesSessionKindAndPromptSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "kind-orphan", "proj", func(s *Session) {
		s.SessionKind = "bg"
		s.MessageCount = 2
	})
	insertMessages(t, srcDB,
		Message{
			SessionID: "kind-orphan", Ordinal: 0, Role: "user",
			Content: "first", PromptSource: "typed",
		},
		Message{
			SessionID: "kind-orphan", Ordinal: 1, Role: "user",
			Content: "second", PromptSource: "queued",
		},
	)
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected one orphan")

	session, err := dstDB.GetSession(ctx, "kind-orphan")
	require.NoError(t, err, "get copied session")
	assert.Equal(t, "bg", session.SessionKind)

	msgs, err := dstDB.GetMessages(ctx, "kind-orphan", 0, 10, true)
	require.NoError(t, err, "get copied messages")
	require.Len(t, msgs, 2)
	assert.Equal(t, "typed", msgs[0].PromptSource)
	assert.Equal(t, "queued", msgs[1].PromptSource)
}

func TestCopyOrphanedDataSanitizesCopiedContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "poison-orphan", "proj")
	insertMessages(t, srcDB, userMsg("poison-orphan", 0, "clean"))
	var messageID int64
	require.NoError(t, srcDB.getWriter().QueryRowContext(ctx,
		`SELECT id FROM messages WHERE session_id = ? AND ordinal = 0`,
		"poison-orphan",
	).Scan(&messageID), "query source message id")

	messageContent := "message\x00body\x01\nkept"
	toolInput := "{\"cmd\":\"tool\x00input\x04\"}"
	emptyToolInput := "\x00\x04"
	toolResult := "tool\x00result\x02"
	emptyToolResult := "\x00\x04"
	eventContent := "event\x00content\x03"
	const (
		messageLengthExcess = 7
		toolLengthExcess    = 11
		eventLengthExcess   = 5
		emptyResultLength   = 7
	)
	_, err := srcDB.rawWriter().ExecContext(ctx,
		`UPDATE messages
		 SET content = ?, content_length = ?
		 WHERE id = ?`,
		messageContent, len(messageContent)+messageLengthExcess, messageID,
	)
	require.NoError(t, err, "plant poisoned message")
	_, err = srcDB.rawWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, input_json, result_content_length,
			result_content, call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-1",
		toolInput, len(toolResult)+toolLengthExcess, toolResult, 0,
	)
	require.NoError(t, err, "plant poisoned tool call")
	_, err = srcDB.rawWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, input_json, call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-empty",
		emptyToolInput, 1,
	)
	require.NoError(t, err, "plant empty-sanitized tool input")
	_, err = srcDB.rawWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, result_content_length, result_content,
			call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-empty-result",
		emptyResultLength, emptyToolResult, 2,
	)
	require.NoError(t, err, "plant empty-sanitized tool result")
	_, err = srcDB.rawWriter().ExecContext(ctx,
		`INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, source, status, content, content_length,
			event_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"poison-orphan", 0, 0, "tool-1", "tool_result", "ok",
		eventContent, len(eventContent)+eventLengthExcess, 0,
	)
	require.NoError(t, err, "plant poisoned tool result event")
	// Dirty content only exists in archives written before
	// sanitizedSourceDataVersion; sources at or above it skip the
	// sanitize pass entirely.
	_, err = srcDB.rawWriter().ExecContext(ctx, fmt.Sprintf(
		"PRAGMA user_version = %d", sanitizedSourceDataVersion-1,
	))
	require.NoError(t, err, "downgrade source data version")
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected one orphan")

	var gotMessage string
	var gotMessageLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"poison-orphan",
	).Scan(&gotMessage, &gotMessageLength), "query copied message")
	wantMessage := SanitizeUTF8(messageContent)
	assert.Equal(t, wantMessage, gotMessage)
	assert.Equal(t, len(wantMessage)+messageLengthExcess, gotMessageLength)

	var gotToolInput string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"poison-orphan",
	).Scan(&gotToolInput), "query copied tool input")
	assert.Equal(t, SanitizeUTF8(toolInput), gotToolInput)

	var gotEmptyToolInput sql.NullString
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 1`,
		"poison-orphan",
	).Scan(&gotEmptyToolInput), "query empty copied tool input")
	assert.False(t, gotEmptyToolInput.Valid)

	var gotToolResult string
	var gotToolResultLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT result_content, result_content_length
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"poison-orphan",
	).Scan(&gotToolResult, &gotToolResultLength), "query copied tool call")
	wantToolResult := SanitizeUTF8(toolResult)
	assert.Equal(t, wantToolResult, gotToolResult)
	assert.Equal(t, len(wantToolResult)+toolLengthExcess, gotToolResultLength)

	var gotEmptyToolResult sql.NullString
	var gotEmptyToolResultLength sql.NullInt64
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT result_content, result_content_length
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 2`,
		"poison-orphan",
	).Scan(
		&gotEmptyToolResult,
		&gotEmptyToolResultLength,
	), "query empty copied tool call result")
	assert.False(t, gotEmptyToolResult.Valid)
	require.True(t, gotEmptyToolResultLength.Valid)
	assert.Equal(t,
		int64(emptyResultLength-len(emptyToolResult)),
		gotEmptyToolResultLength.Int64,
	)

	var gotEventContent string
	var gotEventLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM tool_result_events
		 WHERE session_id = ? AND event_index = 0`,
		"poison-orphan",
	).Scan(&gotEventContent, &gotEventLength), "query copied tool result event")
	wantEventContent := SanitizeUTF8(eventContent)
	assert.Equal(t, wantEventContent, gotEventContent)
	assert.Equal(t, len(wantEventContent)+eventLengthExcess, gotEventLength)
}

// TestCopySkipsSanitizeForSanitizedSource guards the resync fast
// path: each sanitize pass is skipped once the source data version
// proves ingest already sanitized that field — content/results at
// sanitizedSourceDataVersion, input_json at the later
// sanitizedInputSourceDataVersion — and skipped rows survive the
// copy verbatim.
func TestCopySkipsSanitizeForSanitizedSource(t *testing.T) {
	const rawContent = "nul\x00byte\x01kept"
	const rawToolInput = "{\"cmd\":\"a\x00b\x01\"}"
	const rawToolResult = "result\x00kept\x01"
	copies := []struct {
		name  string
		trash bool
		copy  func(dst *DB, srcPath string) (int, error)
	}{
		{
			name: "orphaned",
			copy: func(dst *DB, srcPath string) (int, error) {
				return dst.CopyOrphanedDataFrom(srcPath)
			},
		},
		{
			name:  "trashed",
			trash: true,
			copy: func(dst *DB, srcPath string) (int, error) {
				return dst.CopyTrashedDataFrom(srcPath)
			},
		},
	}
	versions := []struct {
		name          string
		sourceVersion int
		wantInput     string
	}{
		{
			// Ingest at v58 sanitized content and results but not
			// input_json, so only the input pass runs for it.
			name:          "content-sanitized source pays input pass",
			sourceVersion: sanitizedSourceDataVersion,
			wantInput:     SanitizeUTF8(rawToolInput),
		},
		{
			// A source at the input watermark is fully clean; every
			// pass is skipped and rows copy verbatim.
			name:          "fully sanitized source copies verbatim",
			sourceVersion: sanitizedInputSourceDataVersion,
			wantInput:     rawToolInput,
		},
	}
	for _, cp := range copies {
		for _, ver := range versions {
			t.Run(cp.name+"/"+ver.name, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				srcPath := filepath.Join(dir, "old.db")
				srcDB := testDBAtPath(t, srcPath, "src")
				insertSession(t, srcDB, "sess", "proj")
				insertMessages(t, srcDB, userMsg("sess", 0, "clean"))
				_, err := srcDB.rawWriter().ExecContext(ctx,
					`UPDATE messages SET content = ? WHERE session_id = ?`,
					rawContent, "sess",
				)
				require.NoError(t, err, "plant raw content")
				var messageID int64
				require.NoError(t, srcDB.getWriter().QueryRowContext(ctx,
					`SELECT id FROM messages WHERE session_id = ?`, "sess",
				).Scan(&messageID), "read message id")
				_, err = srcDB.rawWriter().ExecContext(ctx,
					`INSERT INTO tool_calls (
						message_id, session_id, tool_name, category,
						tool_use_id, input_json, result_content_length,
						result_content, call_index
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					messageID, "sess", "Bash", "execution", "tool-1",
					rawToolInput, len(rawToolResult), rawToolResult, 0,
				)
				require.NoError(t, err, "plant raw tool call")
				_, err = srcDB.rawWriter().ExecContext(ctx, fmt.Sprintf(
					"PRAGMA user_version = %d", ver.sourceVersion,
				))
				require.NoError(t, err, "set source data version")
				if cp.trash {
					require.NoError(t, srcDB.SoftDeleteSession("sess"),
						"soft delete source session")
				}
				require.NoError(t, srcDB.Close(), "close source")

				dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
				defer dstDB.Close()
				count, err := cp.copy(dstDB, srcPath)
				require.NoError(t, err, "copy from source")
				require.Equal(t, 1, count, "copied sessions")

				var got string
				require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
					`SELECT content FROM messages WHERE session_id = ?`,
					"sess",
				).Scan(&got), "query copied message")
				assert.Equal(t, rawContent, got,
					"content-sanitized source must copy content verbatim")

				var gotInput, gotResult string
				require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
					`SELECT input_json, result_content
					 FROM tool_calls WHERE session_id = ?`,
					"sess",
				).Scan(&gotInput, &gotResult), "query copied tool call")
				assert.Equal(t, ver.wantInput, gotInput)
				assert.Equal(t, rawToolResult, gotResult,
					"tool result must copy verbatim for sanitized sources")
			})
		}
	}
}

func TestArchiveRecoveryRepairsMalformedMessageTimestamp(t *testing.T) {
	copies := []struct {
		name  string
		trash bool
		copy  func(*DB, string) (int, error)
	}{
		{
			name: "orphaned",
			copy: func(destination *DB, sourcePath string) (int, error) {
				return destination.CopyOrphanedDataFrom(sourcePath)
			},
		},
		{
			name:  "trashed",
			trash: true,
			copy: func(destination *DB, sourcePath string) (int, error) {
				return destination.CopyTrashedDataFrom(sourcePath)
			},
		},
	}
	for _, test := range copies {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.db")
			source := testDBAtPath(t, sourcePath, "source")
			insertSession(t, source, "malformed-timestamp", "proj")
			insertMessages(t, source, Message{
				SessionID: "malformed-timestamp",
				Ordinal:   0,
				Role:      "assistant",
				Content:   "archived",
				Timestamp: tsZero,
			})
			_, err := source.rawWriter().ExecContext(ctx,
				`UPDATE messages SET timestamp = ? WHERE session_id = ?`,
				"not-a-timestamp", "malformed-timestamp",
			)
			require.NoError(t, err, "seed malformed timestamp")
			if test.trash {
				require.NoError(t, source.SoftDeleteSession("malformed-timestamp"))
			}
			_, err = source.rawWriter().ExecContext(ctx, fmt.Sprintf(
				"PRAGMA user_version = %d", canonicalTimestampDataVersion-1,
			))
			require.NoError(t, err, "mark source before timestamp canonicalization")
			require.NoError(t, source.Close(), "close source")

			destination := testDBAtPath(
				t, filepath.Join(dir, "destination.db"), "destination",
			)
			defer destination.Close()
			copied, err := test.copy(destination, sourcePath)
			require.NoError(t, err, "copy archived data")
			require.Equal(t, 1, copied, "copied sessions")

			var timestamp string
			require.NoError(t, destination.getReader().QueryRowContext(ctx,
				`SELECT COALESCE(timestamp, '') FROM messages WHERE session_id = ?`,
				"malformed-timestamp",
			).Scan(&timestamp), "read repaired timestamp")
			assert.Empty(t, timestamp)
		})
	}
}
