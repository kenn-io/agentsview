package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteSession_LargeSessionFTSDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in -short mode")
	}

	t.Parallel()
	d := openLargeSessionFixtureDB(t, true)
	requireFTS(t, d)

	start := time.Now()
	require.NoError(t, d.DeleteSession(largeSessionFixtureID), "DeleteSession")
	elapsed := time.Since(start)
	require.LessOrEqual(t, elapsed, largeSessionPerfCeiling,
		"DeleteSession took %s, want < 10s (per-row FTS trigger regression?)",
		elapsed.Round(time.Millisecond))

	requireSessionGone(t, d, largeSessionFixtureID)
	assertNoFTSLeak(t, d, largeSessionFixtureToken)
	requireMessagesDeleteTriggerRestored(t, d)

	var neighborPins int
	err := d.getReader().QueryRow(
		"SELECT count(*) FROM pinned_messages WHERE session_id LIKE ?",
		largeSessionNeighborPrefix+"-%",
	).Scan(&neighborPins)
	require.NoError(t, err, "neighbor pins count")
	assert.Equal(t, crossSessionNeighborCount, neighborPins,
		"neighbor pins count")
}

func TestSupersedeSessionIdentitiesPreservesDependentData(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const (
		oldID     = "old-machine~session"
		currentID = "new-machine~session"
	)
	for _, sess := range []Session{
		{ID: oldID, Project: "project", Machine: "old-machine", Agent: "claude"},
		{ID: currentID, Project: "project", Machine: "new-machine", Agent: "claude"},
		{ID: "child", Project: "project", Machine: "viewer", Agent: "claude",
			ParentSessionID: Ptr(oldID), SourceSessionID: oldID},
		{ID: "parent", Project: "project", Machine: "viewer", Agent: "claude"},
	} {
		require.NoError(t, d.UpsertSession(sess))
	}
	require.NoError(t, d.InsertMessages([]Message{
		{SessionID: oldID, Ordinal: 0, Role: "user", Content: "stable",
			SourceUUID: "stable-source"},
		{SessionID: oldID, Ordinal: 1, Role: "assistant", Content: "legacy"},
		{SessionID: currentID, Ordinal: 1, Role: "assistant", Content: "legacy"},
		{SessionID: currentID, Ordinal: 2, Role: "user", Content: "stable",
			SourceUUID: "stable-source"},
		{SessionID: "parent", Ordinal: 0, Role: "assistant", Content: "spawn",
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "agent", ToolUseID: "spawn",
				SubagentSessionID: oldID,
				ResultEvents: []ToolResultEvent{{
					ToolUseID: "spawn", SubagentSessionID: oldID,
					Source: "progress", Status: "completed",
				}},
			}}},
	}))
	oldMessages, err := d.GetAllMessages(ctx, oldID)
	require.NoError(t, err)
	require.Len(t, oldMessages, 2)
	firstNote, secondNote := "stable note", "legacy note"
	_, err = d.PinMessage(oldID, oldMessages[0].ID, &firstNote)
	require.NoError(t, err)
	_, err = d.PinMessage(oldID, oldMessages[1].ID, &secondNote)
	require.NoError(t, err)
	starred, err := d.StarSession(oldID)
	require.NoError(t, err)
	require.True(t, starred)

	require.NoError(t, d.ReplaceSessionUsageEvents(oldID, []UsageEvent{
		{Source: "session", Model: "model", InputTokens: 10,
			OutputTokens: 2, DedupKey: "same"},
		{Source: "session", Model: "legacy", InputTokens: 5,
			OutputTokens: 1},
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents(currentID, []UsageEvent{{
		Source: "session", Model: "model", InputTokens: 10,
		OutputTokens: 2, DedupKey: "same",
	}}))
	_, err = d.InsertRecallEntry(RecallEntry{
		ID: "recall", Type: "fact", Scope: "project", Title: "title", Body: "body",
		SourceSessionID: oldID,
		Evidence: []RecallEvidence{{
			SessionID: oldID, MessageStartOrdinal: 0, MessageEndOrdinal: 1,
			MessageStartSourceUUID: "stable-source",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, d.ReplaceSessionSecretFindings(oldID, []SecretFinding{{
		RuleName: "rule", Confidence: "high", LocationKind: "message",
		MessageOrdinal: 0, MatchStart: 0, MatchEnd: 6,
		RedactedMatch: "******", RulesVersion: "v1",
	}}, 1, "v1"))
	require.NoError(t, d.ReplaceSessionSecretFindings(currentID, []SecretFinding{{
		RuleName: "rule", Confidence: "high", LocationKind: "message",
		MessageOrdinal: 0, MatchStart: 0, MatchEnd: 6,
		RedactedMatch: "******", RulesVersion: "v1",
	}}, 1, "v1"))
	_, err = d.getWriter().Exec(`
		UPDATE session_project_identity_snapshots
		SET git_remote = 'https://example.invalid/repo.git',
			remote_resolution = 'resolved', key = 'repository:repo'
		WHERE session_id = ?`, oldID)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		UPDATE sessions SET local_modified_at = '2000-01-01T00:00:00.000Z'
		WHERE id IN (?, ?, ?, ?)`, oldID, currentID, "child", "parent")
	require.NoError(t, err)

	require.NoError(t, d.SupersedeSessionIdentities(currentID, []string{oldID}))

	old, err := d.GetSessionFull(ctx, oldID)
	require.NoError(t, err)
	assert.Nil(t, old)
	pins, err := d.ListPinnedMessages(ctx, currentID, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	pinsByOrdinal := map[int]PinnedMessage{}
	for _, pin := range pins {
		pinsByOrdinal[pin.Ordinal] = pin
	}
	require.NotNil(t, pinsByOrdinal[2].Note)
	assert.Equal(t, firstNote, *pinsByOrdinal[2].Note)
	require.NotNil(t, pinsByOrdinal[1].Note)
	assert.Equal(t, secondNote, *pinsByOrdinal[1].Note)

	stars, err := d.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Contains(t, stars, currentID)
	assert.NotContains(t, stars, oldID)
	usage, err := d.GetUsageEvents(ctx, currentID)
	require.NoError(t, err)
	require.Len(t, usage, 2)

	recall, err := d.GetRecallEntry(ctx, "recall")
	require.NoError(t, err)
	require.NotNil(t, recall)
	assert.Equal(t, currentID, recall.SourceSessionID)
	require.Len(t, recall.Evidence, 1)
	assert.Equal(t, currentID, recall.Evidence[0].SessionID)

	findings, err := d.SessionSecretFindings(ctx, currentID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, currentID, findings[0].SessionID)

	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	snapshotByID := make(map[string]string, len(snapshots))
	for _, snapshot := range snapshots {
		snapshotByID[snapshot.SessionID] = snapshot.GitRemote
		if snapshot.SessionID == currentID {
			assert.Equal(t, "new-machine", snapshot.Machine)
		}
	}
	assert.Equal(t, "https://example.invalid/repo.git", snapshotByID[currentID])
	assert.NotContains(t, snapshotByID, oldID)

	child, err := d.GetSessionFull(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, currentID, *child.ParentSessionID)
	assert.Equal(t, currentID, child.SourceSessionID)
	parentMessages, err := d.GetAllMessages(ctx, "parent")
	require.NoError(t, err)
	require.Len(t, parentMessages, 1)
	require.Len(t, parentMessages[0].ToolCalls, 1)
	assert.Equal(t, currentID, parentMessages[0].ToolCalls[0].SubagentSessionID)
	require.Len(t, parentMessages[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(
		t, currentID,
		parentMessages[0].ToolCalls[0].ResultEvents[0].SubagentSessionID,
	)

	aliases, err := d.ListSessionIdentityAliases(ctx, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []SessionIdentityAlias{{
		AliasID: oldID, SessionID: currentID,
		Project: "project", Machine: "new-machine",
	}}, aliases)
	assert.False(t, d.IsSessionExcluded(oldID))

	modified, err := d.ListSessionsModifiedBetween(
		ctx, "2020-01-01T00:00:00Z", "2100-01-01T00:00:00Z", nil, nil,
	)
	require.NoError(t, err)
	modifiedIDs := make([]string, 0, len(modified))
	for _, session := range modified {
		modifiedIDs = append(modifiedIDs, session.ID)
	}
	assert.ElementsMatch(t, []string{currentID, "child", "parent"}, modifiedIDs)
}

func TestSupersedeSessionIdentitiesPreservesCanonicalPinMetadata(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const (
		oldID     = "old-machine~pin-conflict"
		currentID = "new-machine~pin-conflict"
	)
	for _, id := range []string{oldID, currentID} {
		require.NoError(t, d.UpsertSession(Session{
			ID: id, Project: "project", Machine: "machine", Agent: "claude",
		}))
	}
	require.NoError(t, d.InsertMessages([]Message{
		{SessionID: oldID, Ordinal: 0, Role: "user", Content: "old",
			SourceUUID: "stable-source"},
		{SessionID: currentID, Ordinal: 0, Role: "user", Content: "unrelated",
			SourceUUID: "unrelated-source"},
		{SessionID: currentID, Ordinal: 2, Role: "user", Content: "current",
			SourceUUID: "stable-source"},
	}))
	oldMessages, err := d.GetAllMessages(ctx, oldID)
	require.NoError(t, err)
	currentMessages, err := d.GetAllMessages(ctx, currentID)
	require.NoError(t, err)
	require.Len(t, currentMessages, 2)
	oldNote, currentNote := "superseded note", "canonical note"
	_, err = d.PinMessage(oldID, oldMessages[0].ID, &oldNote)
	require.NoError(t, err)
	_, err = d.PinMessage(currentID, currentMessages[1].ID, &currentNote)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		UPDATE pinned_messages
		SET created_at = CASE session_id
			WHEN ? THEN '2026-07-17T10:00:00.000Z'
			ELSE '2026-07-17T11:00:00.000Z'
		END
		WHERE session_id IN (?, ?)`,
		oldID, oldID, currentID,
	)
	require.NoError(t, err)

	require.NoError(t, d.SupersedeSessionIdentities(currentID, []string{oldID}))

	pins, err := d.ListPinnedMessages(ctx, currentID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 2, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, currentNote, *pins[0].Note)
	assert.Equal(t, "2026-07-17T11:00:00.000Z", pins[0].CreatedAt)
}

func TestDeletingReplacementExcludesIdentityAliasChain(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"old", "middle", "current"} {
		require.NoError(t, d.UpsertSession(Session{
			ID: id, Project: "project", Machine: "machine", Agent: "claude",
		}))
	}

	require.NoError(t, d.SupersedeSessionIdentities("middle", []string{"old"}))
	require.NoError(t, d.SupersedeSessionIdentities("current", []string{"middle"}))

	require.NoError(t, d.DeleteSession("current"))
	for _, id := range []string{"old", "middle", "current"} {
		assert.True(t, d.IsSessionExcluded(id), id)
	}
}

func TestSessionIdentityAliasPublicationRevisionTracksTombstones(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, id := range []string{"old-session", "current-session"} {
		require.NoError(t, d.UpsertSession(Session{
			ID: id, Project: "app", Machine: "machine", Agent: "claude",
		}))
	}
	before, err := d.SessionIdentityAliasPublicationRevision(ctx)
	require.NoError(t, err)

	require.NoError(t, d.SupersedeSessionIdentities(
		"current-session", []string{"old-session"},
	))
	afterInsert, err := d.SessionIdentityAliasPublicationRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, afterInsert, before)
	inserted, err := d.ListSessionIdentityAliasChanges(
		ctx, before, afterInsert, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, inserted, 1)
	assert.Equal(t, "old-session", inserted[0].AliasID)
	assert.False(t, inserted[0].Deleted)

	require.NoError(t, d.DeleteSession("current-session"))
	afterDelete, err := d.SessionIdentityAliasPublicationRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, afterDelete, afterInsert)
	deleted, err := d.ListSessionIdentityAliasChanges(
		ctx, afterInsert, afterDelete, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, "old-session", deleted[0].AliasID)
	assert.Equal(t, "app", deleted[0].Project)
	assert.True(t, deleted[0].Deleted)
}

func TestCopyExcludedSessionsPreservesSourceIdentityTombstones(t *testing.T) {
	sourcePath := t.TempDir() + "/source.db"
	source, err := Open(sourcePath)
	require.NoError(t, err)
	const objectPath = "s3://bucket/old/raw/claude/project/session.jsonl"
	storedPath := objectPath
	require.NoError(t, source.UpsertSession(Session{
		ID: "old~session", Project: "project", Machine: "old", Agent: "claude",
		FilePath: &storedPath,
	}))
	suppressed, err := source.PrepareSessionSourceIdentity(
		objectPath, "claude", "old~session",
	)
	require.NoError(t, err)
	assert.False(t, suppressed)
	require.NoError(t, source.DeleteSession("old~session"))
	require.NoError(t, source.Close())

	destination, err := Open(t.TempDir() + "/destination.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, destination.Close()) })
	require.NoError(t, destination.CopyExcludedSessionsFrom(sourcePath))
	suppressed, err = destination.PrepareSessionSourceIdentity(
		objectPath, "claude", "new~session",
	)
	require.NoError(t, err)
	assert.True(t, suppressed)
	assert.True(t, destination.IsSessionExcluded("old~session"))
	assert.True(t, destination.IsSessionExcluded("new~session"))
}

func TestSupersedeSessionIdentitiesRollsBackOnError(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const (
		oldID     = "old-machine~rollback"
		currentID = "new-machine~rollback"
	)
	for _, id := range []string{oldID, currentID} {
		require.NoError(t, d.UpsertSession(Session{
			ID: id, Project: "project", Machine: "machine", Agent: "claude",
		}))
		require.NoError(t, d.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "user", Content: "message",
			SourceUUID: "stable-source",
		}}))
	}
	oldMessages, err := d.GetAllMessages(ctx, oldID)
	require.NoError(t, err)
	require.Len(t, oldMessages, 1)
	_, err = d.PinMessage(oldID, oldMessages[0].ID, nil)
	require.NoError(t, err)
	_, err = d.InsertRecallEntry(RecallEntry{
		ID: "rollback-recall", Type: "fact", Scope: "project",
		Title: "title", Body: "body", SourceSessionID: oldID,
	})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		CREATE TRIGGER reject_recall_identity_update
		BEFORE UPDATE OF source_session_id ON recall_entries
		BEGIN
			SELECT RAISE(ABORT, 'forced rollback');
		END`)
	require.NoError(t, err)

	err = d.SupersedeSessionIdentities(currentID, []string{oldID})
	require.ErrorContains(t, err, "forced rollback")

	old, getErr := d.GetSessionFull(ctx, oldID)
	require.NoError(t, getErr)
	assert.NotNil(t, old)
	oldPins, getErr := d.ListPinnedMessages(ctx, oldID, "")
	require.NoError(t, getErr)
	assert.Len(t, oldPins, 1)
	currentPins, getErr := d.ListPinnedMessages(ctx, currentID, "")
	require.NoError(t, getErr)
	assert.Empty(t, currentPins)
	recall, getErr := d.GetRecallEntry(ctx, "rollback-recall")
	require.NoError(t, getErr)
	require.NotNil(t, recall)
	assert.Equal(t, oldID, recall.SourceSessionID)
}

func TestSupersedeSessionIdentitiesCoversForeignKeyTables(t *testing.T) {
	d := testDB(t)
	rows, err := d.getReader().Query(`
		SELECT schema_table.name, foreign_key."from"
		FROM sqlite_schema AS schema_table
		JOIN pragma_foreign_key_list(schema_table.name) AS foreign_key
		WHERE schema_table.type = 'table'
		  AND foreign_key."table" IN ('sessions', 'messages')
		ORDER BY schema_table.name, foreign_key."from"`)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var table, column string
		require.NoError(t, rows.Scan(&table, &column))
		got = append(got, table+"."+column)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"messages.session_id",
		"pinned_messages.message_id",
		"pinned_messages.session_id",
		"recall_entries.source_session_id",
		"recall_evidence.session_id",
		"secret_findings.session_id",
		"session_identity_aliases.session_id",
		"session_project_identity_snapshots.session_id",
		"starred_sessions.session_id",
		"tool_calls.message_id",
		"tool_calls.session_id",
		"tool_result_events.session_id",
		"usage_events.session_id",
	}, got)
}

func TestCopySessionMetadataPreservesIdentityAliases(t *testing.T) {
	ctx := context.Background()
	sourcePath := t.TempDir() + "/source.db"
	source, err := Open(sourcePath)
	require.NoError(t, err)
	for _, id := range []string{"old-machine~session", "new-machine~session"} {
		require.NoError(t, source.UpsertSession(Session{
			ID: id, Project: "project", Machine: "machine", Agent: "claude",
		}))
	}
	require.NoError(t, source.SupersedeSessionIdentities(
		"new-machine~session", []string{"old-machine~session"},
	))
	require.NoError(t, source.Close())

	destination := testDB(t)
	require.NoError(t, destination.UpsertSession(Session{
		ID: "new-machine~session", Project: "project",
		Machine: "machine", Agent: "claude",
	}))
	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))

	aliases, err := destination.ListSessionIdentityAliases(ctx, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []SessionIdentityAlias{{
		AliasID: "old-machine~session", SessionID: "new-machine~session",
		Project: "project", Machine: "machine",
	}}, aliases)
}

func TestFindSessionIDsByPartial(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "abcdef-1111-2222", "proj")
	insertSession(t, d, "abcdef-3333-4444", "proj")
	insertSession(t, d, "fedcba-5555", "proj")

	ctx := context.Background()

	got, err := d.FindSessionIDsByPartial(ctx, "abcdef", 5)
	require.NoError(t, err, "FindSessionIDsByPartial")
	assert.Len(t, got, 2, "abcdef matches")

	got, err = d.FindSessionIDsByPartial(ctx, "fedcba", 5)
	require.NoError(t, err, "FindSessionIDsByPartial")
	assert.Equal(t, []string{"fedcba-5555"}, got, "fedcba matches")

	got, err = d.FindSessionIDsByPartial(ctx, "nope", 5)
	require.NoError(t, err, "FindSessionIDsByPartial")
	assert.Empty(t, got, "nope matches")

	got, err = d.FindSessionIDsByPartial(ctx, "", 5)
	require.NoError(t, err, "FindSessionIDsByPartial")
	assert.Nil(t, got, "empty input")
}

func TestFindSessionIDsByPartialLiteralCaseSensitive(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "abc_def", "proj")
	insertSession(t, d, "abcXdef", "proj")
	insertSession(t, d, "abc%def", "proj")
	insertSession(t, d, "ABCdef", "proj")

	ctx := context.Background()

	got, err := d.FindSessionIDsByPartial(ctx, "c_d", 10)
	require.NoError(t, err, "underscore lookup")
	assert.Equal(t, []string{"abc_def"}, got)

	got, err = d.FindSessionIDsByPartial(ctx, "c%d", 10)
	require.NoError(t, err, "percent lookup")
	assert.Equal(t, []string{"abc%def"}, got)

	got, err = d.FindSessionIDsByPartial(ctx, "abc", 10)
	require.NoError(t, err, "case-sensitive lookup")
	assert.ElementsMatch(t, []string{"abc_def", "abcXdef", "abc%def"}, got)
	assert.NotContains(t, got, "ABCdef")
}

func TestListSessions_OutcomeFilter(t *testing.T) {
	d := testDB(t)

	// Insert sessions then set signals with different outcomes.
	for _, tc := range []struct {
		id      string
		outcome string
	}{
		{"out-1", "completed"},
		{"out-2", "abandoned"},
		{"out-3", "errored"},
		{"out-4", "completed"},
	} {
		insertSession(t, d, tc.id, "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.EndedAt = new("2024-06-01T11:00:00Z")
			s.MessageCount = 5
			s.UserMessageCount = 3
		})
		err := d.UpdateSessionSignals(tc.id, SessionSignalUpdate{
			Outcome: tc.outcome,
		})
		require.NoError(t, err, "UpdateSessionSignals %s", tc.id)
	}

	// Single outcome.
	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.Outcome = []string{"abandoned"}
	}), []string{"out-2"})

	// Multiple outcomes.
	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.Outcome = []string{"completed", "errored"}
	}), []string{"out-1", "out-3", "out-4"})
}

func TestListSessions_HealthGradeFilter(t *testing.T) {
	d := testDB(t)

	for _, tc := range []struct {
		id    string
		grade string
		score int
	}{
		{"hg-1", "A", 95},
		{"hg-2", "C", 60},
		{"hg-3", "F", 20},
		{"hg-4", "A", 90},
	} {
		insertSession(t, d, tc.id, "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.EndedAt = new("2024-06-01T11:00:00Z")
			s.MessageCount = 5
			s.UserMessageCount = 3
		})
		err := d.UpdateSessionSignals(tc.id, SessionSignalUpdate{
			HealthGrade: new(tc.grade),
			HealthScore: new(tc.score),
		})
		require.NoError(t, err, "UpdateSessionSignals %s", tc.id)
	}

	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.HealthGrade = []string{"A"}
	}), []string{"hg-1", "hg-4"})

	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.HealthGrade = []string{"C", "F"}
	}), []string{"hg-2", "hg-3"})
}

func TestListSessions_MinToolFailuresFilter(t *testing.T) {
	d := testDB(t)

	for _, tc := range []struct {
		id       string
		failures int
	}{
		{"tf-1", 0},
		{"tf-2", 3},
		{"tf-3", 7},
	} {
		insertSession(t, d, tc.id, "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.EndedAt = new("2024-06-01T11:00:00Z")
			s.MessageCount = 5
			s.UserMessageCount = 3
		})
		err := d.UpdateSessionSignals(tc.id, SessionSignalUpdate{
			ToolFailureSignalCount: tc.failures,
		})
		require.NoError(t, err, "UpdateSessionSignals %s", tc.id)
	}

	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.MinToolFailures = new(3)
	}), []string{"tf-2", "tf-3"})

	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.MinToolFailures = new(5)
	}), []string{"tf-3"})

	// Zero threshold returns all.
	requireSessions(t, d, filterWith(func(f *SessionFilter) {
		f.MinToolFailures = new(0)
	}), []string{"tf-1", "tf-2", "tf-3"})
}

func TestUpsertSession_DisplayNameUpdateBehavior(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	sessionName := "My Chat Title"
	err := d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		SessionName:  &sessionName,
		MessageCount: 1,
	})
	require.NoError(t, err, "UpsertSession insert")

	// Verify session_name is visible via COALESCE in GetSession.
	s, err := d.GetSession(ctx, "claude-ai:dn-test")
	require.NoError(t, err, "GetSession after insert")
	require.NotNil(t, s, "GetSession returned nil after insert")
	require.NotNil(t, s.DisplayName, "DisplayName is nil after insert")
	assert.Equal(t, "My Chat Title", *s.DisplayName, "DisplayName")

	// Re-upsert with a different session_name: should overwrite (agent names
	// are always refreshed on re-parse; only display_name is user-protected).
	newName := "Updated Title"
	err = d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		SessionName:  &newName,
		MessageCount: 2,
	})
	require.NoError(t, err, "UpsertSession update")

	// session_name should be updated on re-upsert.
	s, err = d.GetSession(ctx, "claude-ai:dn-test")
	require.NoError(t, err, "GetSession after re-upsert")
	require.NotNil(t, s, "GetSession returned nil after re-upsert")
	require.NotNil(t, s.DisplayName, "DisplayName is nil after re-upsert")
	assert.Equal(t, "Updated Title", *s.DisplayName,
		"session_name should be updated on re-upsert")
	// Other fields should also update.
	assert.Equal(t, 2, s.MessageCount, "MessageCount")
}

// TestUpsertSessionDoesNotAdvanceDataVersion guards the
// invariant that data_version is never touched by
// UpsertSession -- it must only advance via
// SetSessionDataVersion after a successful message rewrite,
// so a transient write failure cannot leave a session row
// stamped at the current parser version with stale
// messages.
func TestUpsertSessionDoesNotAdvanceDataVersion(t *testing.T) {
	d := testDB(t)

	// New session: data_version stays 0 even when the
	// caller passes a non-zero value on the struct.
	require.NoError(t, d.UpsertSession(Session{
		ID:           "dv-1",
		Project:      "p",
		Machine:      "m",
		Agent:        "claude",
		MessageCount: 1,
		DataVersion:  CurrentDataVersion(),
	}), "UpsertSession (insert)")
	assert.Equal(t, 0, d.GetSessionDataVersion("dv-1"),
		"after insert, data_version")

	// Stamp a current value to simulate a successful write.
	require.NoError(t, d.SetSessionDataVersion(
		"dv-1", CurrentDataVersion(),
	), "SetSessionDataVersion")
	assert.Equal(t, CurrentDataVersion(), d.GetSessionDataVersion("dv-1"),
		"after Set, data_version")

	// Re-upserting (e.g. as part of an incremental sync)
	// must NOT clobber the stamped version with the
	// struct's value (here 0), and must NOT replace it
	// with a future "current" value before the rewrite
	// succeeds.
	require.NoError(t, d.UpsertSession(Session{
		ID:           "dv-1",
		Project:      "p",
		Machine:      "m",
		Agent:        "claude",
		MessageCount: 5,
		DataVersion:  0,
	}), "UpsertSession (update)")
	assert.Equal(t, CurrentDataVersion(), d.GetSessionDataVersion("dv-1"),
		"after re-upsert, data_version (must be preserved across UpsertSession)")
}

func TestSessionTranscriptFidelityRoundTrips(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	s := Session{
		ID:                 "antigravity-cli:fidelity-rt",
		Agent:              "antigravity-cli",
		TranscriptFidelity: "summary",
	}
	require.NoError(t, d.UpsertSession(s))

	got, err := d.GetSession(ctx, s.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "summary", got.TranscriptFidelity)
}

func TestUpsertSessionTerminationStatus(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	clean := "clean"
	pending := "tool_call_pending"

	tests := []struct {
		name string
		val  *string
	}{
		{name: "null", val: nil},
		{name: "clean", val: &clean},
		{name: "tool_call_pending", val: &pending},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := "session_" + tc.name
			s := Session{
				ID:                id,
				Project:           "p",
				Machine:           "local",
				Agent:             "claude",
				MessageCount:      1,
				UserMessageCount:  1,
				TerminationStatus: tc.val,
			}
			require.NoError(t, d.UpsertSession(s), "upsert")

			got, err := d.GetSession(ctx, id)
			require.NoError(t, err, "get")
			require.NotNil(t, got, "session not found")

			if tc.val == nil {
				assert.Nil(t, got.TerminationStatus, "nil mismatch")
			} else {
				require.NotNil(t, got.TerminationStatus, "nil mismatch")
				assert.Equal(t, *tc.val, *got.TerminationStatus, "value mismatch")
			}
		})
	}
}

func TestListSessionsTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	clean := "clean"
	pending := "tool_call_pending"
	truncated := "truncated"

	now := time.Now().UTC()
	mkTS := func(d time.Duration) string {
		return now.Add(-d).Format("2006-01-02T15:04:05.000Z")
	}

	insertAt := func(id string, age time.Duration, term *string) {
		ts := mkTS(age)
		s := Session{
			ID:                id,
			Project:           "p",
			Machine:           "local",
			Agent:             "claude",
			StartedAt:         &ts,
			EndedAt:           &ts,
			MessageCount:      1,
			UserMessageCount:  2,
			TerminationStatus: term,
		}
		require.NoError(t, d.UpsertSession(s), "upsert %s", id)
	}

	// Active (< 10 min idle): regardless of termination_status,
	// these are surfaced by ?termination=active.
	insertAt("active-clean", 1*time.Minute, &clean)
	insertAt("active-pending", 2*time.Minute, &pending)

	// Stale (10–60 min idle): surfaced by ?termination=stale.
	insertAt("stale-clean", 30*time.Minute, &clean)
	insertAt("stale-pending", 40*time.Minute, &pending)

	// Idle > 60 min: surfaced by ?termination=unclean only when
	// termination_status flags an issue.
	insertAt("old-clean", 2*time.Hour, &clean)
	insertAt("old-pending", 2*time.Hour, &pending)
	insertAt("old-truncated", 3*time.Hour, &truncated)
	insertAt("old-null", 2*time.Hour, nil)

	collect := func(f SessionFilter) []string {
		page, err := d.ListSessions(ctx, f)
		require.NoError(t, err, "list")
		ids := make([]string, len(page.Sessions))
		for i, s := range page.Sessions {
			ids[i] = s.ID
		}
		return ids
	}

	tests := []struct {
		name        string
		termination string
		wantIDs     []string
	}{
		{
			name:        "all (default)",
			termination: "",
			wantIDs: []string{
				"active-clean", "active-pending",
				"stale-clean", "stale-pending",
				"old-clean", "old-pending",
				"old-truncated", "old-null",
			},
		},
		{
			name:        "active",
			termination: "active",
			wantIDs:     []string{"active-clean", "active-pending"},
		},
		{
			// Yellow only fires for parser-flagged sessions —
			// stale-clean stays quiet, no false positive for
			// sessions that ended normally.
			name:        "stale",
			termination: "stale",
			wantIDs:     []string{"stale-pending"},
		},
		{
			name:        "unclean",
			termination: "unclean",
			wantIDs:     []string{"old-pending", "old-truncated"},
		},
		{
			// Multi-select: comma-separated values OR together,
			// so "stale,unclean" surfaces every parser-flagged
			// session past the active window.
			name:        "stale or unclean",
			termination: "stale,unclean",
			wantIDs:     []string{"stale-pending", "old-pending", "old-truncated"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(SessionFilter{Termination: tc.termination})
			assertStringSetsEqual(t, got, tc.wantIDs)
		})
	}
}

// assertStringSetsEqual checks that two slices contain the same
// elements regardless of order.
func assertStringSetsEqual(t *testing.T, got, want []string) {
	t.Helper()
	assert.ElementsMatch(t, want, got)
}

// getSessionRow reads display_name and session_name directly from the
// sessions table without going through scanSessionRow.
func getSessionRow(t *testing.T, d *DB, id string) Session {
	t.Helper()
	var s Session
	s.ID = id
	requireNoError(t, d.getWriter().QueryRow(
		"SELECT display_name, session_name FROM sessions WHERE id = ?", id).
		Scan(&s.DisplayName, &s.SessionName), "get session row")
	return s
}

func TestUpsertSessionNameOwnership(t *testing.T) {
	d := testDB(t)

	// Agent name lands on a fresh row via session_name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("agent-one"),
	}), "insert agent name")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.SessionName)
	assert.Equal(t, "agent-one", *got.SessionName)
	assert.Nil(t, got.DisplayName, "display_name not set by upsert")

	// A newer agent name overwrites session_name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("agent-two"),
	}), "update agent name")
	got = getSessionRow(t, d, "s1")
	assert.Equal(t, "agent-two", *got.SessionName, "session_name updated on re-upsert")
	assert.Nil(t, got.DisplayName, "display_name still not set by upsert")

	// A manual rename sets display_name.
	requireNoError(t, d.RenameSession("s1", Ptr("user-name")), "rename")

	// A subsequent agent name must NOT overwrite the user's display_name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("agent-three"),
	}), "agent after user")
	got = getSessionRow(t, d, "s1")
	assert.Equal(t, "user-name", *got.DisplayName, "user display_name must survive re-parse")
	assert.Equal(t, "agent-three", *got.SessionName, "session_name always updated")
}

// TestGetSessionFullPopulatesSessionName verifies that GetSessionFull
// keeps the raw session_name on the Go struct while DisplayName carries
// the visible name (user rename, else session_name), matching the PG and
// DuckDB GetSessionFull implementations. session_name is a backend-only
// field: it is NOT serialised in JSON responses (json:"-" on
// Session.SessionName). Push paths that need display_name unmerged read
// via ListSessionsModifiedBetween, not GetSessionFull.
func TestGetSessionFullPopulatesSessionName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Insert a session with an agent-provided session_name.
	requireNoError(t, d.UpsertSession(Session{
		ID:           "s-ns",
		Project:      "p",
		Machine:      "local",
		Agent:        "claude",
		SessionName:  Ptr("Agent Title"),
		MessageCount: 1,
	}), "upsert agent-named session")

	// GetSessionFull populates Session.SessionName from the DB for internal use.
	s, err := d.GetSessionFull(ctx, "s-ns")
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, s, "session not found")
	require.NotNil(t, s.SessionName, "SessionName is nil after GetSessionFull")
	assert.Equal(t, "Agent Title", *s.SessionName, "SessionName round-trips")
	// No user rename yet: DisplayName falls back to session_name.
	require.NotNil(t, s.DisplayName, "DisplayName coalesces to session_name")
	assert.Equal(t, "Agent Title", *s.DisplayName, "visible name before rename")

	// After a manual rename, display_name is set; session_name is unchanged.
	requireNoError(t, d.RenameSession("s-ns", Ptr("User Title")), "rename")
	s, err = d.GetSessionFull(ctx, "s-ns")
	require.NoError(t, err, "GetSessionFull after rename")
	require.NotNil(t, s, "session not found after rename")
	require.NotNil(t, s.DisplayName, "DisplayName should be set after rename")
	assert.Equal(t, "User Title", *s.DisplayName, "display_name after rename")
	require.NotNil(t, s.SessionName, "SessionName should still be set after rename")
	assert.Equal(t, "Agent Title", *s.SessionName, "SessionName unchanged after rename")
}

func TestSessionIdentity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "sqlite-identity", "sqlite-identity", func(s *Session) {
		s.Agent = "claude"
		s.AgentLabel = "Claude Triage"
		s.Entrypoint = "sdk-cli"
		s.SessionName = Ptr("Agent Title")
		s.StartedAt = Ptr("2024-06-15T08:00:00Z")
		s.EndedAt = Ptr("2024-06-15T09:00:00Z")
		s.CreatedAt = "2024-06-15T08:00:00Z"
		s.UserMessageCount = 1
	})

	index, err := d.GetSidebarSessionIndex(ctx, SessionFilter{
		Project: "sqlite-identity",
	})
	require.NoError(t, err)
	require.Len(t, index.Sessions, 1)
	assert.Equal(t, "sqlite-identity", index.Sessions[0].ID)
	assert.Equal(t, "Claude Triage", index.Sessions[0].AgentLabel)
	assert.Equal(t, "sdk-cli", index.Sessions[0].Entrypoint)
	require.NotNil(t, index.Sessions[0].DisplayName)
	assert.Equal(t, "Agent Title", *index.Sessions[0].DisplayName)
}

func TestSessionIdentityAbsent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "sqlite-identity-absent", "sqlite-identity-absent", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T08:00:00Z")
		s.CreatedAt = "2024-06-15T08:00:00Z"
		s.UserMessageCount = 1
	})

	session, err := d.GetSession(ctx, "sqlite-identity-absent")
	require.NoError(t, err)
	assert.Equal(t, "", session.AgentLabel)
	assert.Equal(t, "", session.Entrypoint)

	index, err := d.GetSidebarSessionIndex(ctx, SessionFilter{
		Project: "sqlite-identity-absent",
	})
	require.NoError(t, err)
	require.Len(t, index.Sessions, 1)
	assert.Equal(t, "", index.Sessions[0].AgentLabel)
	assert.Equal(t, "", index.Sessions[0].Entrypoint)
}

func TestGetSessionName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertSession(Session{
		ID: "null-name", Project: "p", Machine: "local", Agent: "codex",
	}), "upsert session with null name")
	require.NoError(t, d.UpsertSession(Session{
		ID: "stored-name", Project: "p", Machine: "local", Agent: "codex",
		SessionName: Ptr("Agent Title"),
	}), "upsert session with stored name")

	tests := []struct {
		name      string
		id        string
		wantName  string
		wantFound bool
	}{
		{name: "missing", id: "missing", wantFound: false},
		{name: "null", id: "null-name", wantFound: true},
		{name: "stored", id: "stored-name", wantName: "Agent Title", wantFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, found, err := d.GetSessionName(ctx, tt.id)
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantFound, found)
		})
	}
}

func TestRenameSessionSetsAndClears(t *testing.T) {
	d := testDB(t)
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Agent Name"),
	}), "upsert")

	requireNoError(t, d.RenameSession("s1", Ptr("User Name")), "rename")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.DisplayName, "display_name set after rename")
	assert.Equal(t, "User Name", *got.DisplayName)
	// session_name is unchanged by RenameSession.
	require.NotNil(t, got.SessionName, "session_name not cleared by rename")
	assert.Equal(t, "Agent Name", *got.SessionName)

	requireNoError(t, d.RenameSession("s1", nil), "clear")
	got = getSessionRow(t, d, "s1")
	assert.Nil(t, got.DisplayName, "display_name cleared")
	// session_name persists after clearing the user rename.
	require.NotNil(t, got.SessionName, "session_name persists")
	assert.Equal(t, "Agent Name", *got.SessionName)
}

func TestSessionNameCOALESCEInGetSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session with only session_name — GetSession should return it via COALESCE.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Agent Title"), MessageCount: 1,
	}), "upsert with session_name")
	s, err := d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Agent Title", *s.DisplayName, "COALESCE returns session_name when no user rename")

	// User renames — display_name wins.
	requireNoError(t, d.RenameSession("s1", Ptr("User Title")), "rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "User Title", *s.DisplayName, "display_name wins over session_name")

	// Clear rename — session_name visible again.
	requireNoError(t, d.RenameSession("s1", nil), "clear rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Agent Title", *s.DisplayName, "session_name restored after clearing rename")
}
