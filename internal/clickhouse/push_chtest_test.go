//go:build chtest

package clickhouse

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestEnsureSchemaCreatesMissingDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := chtest.ServerURL(t)
	database := "agentsview_boot_" + strings.ReplaceAll(t.Name(), "/", "_")
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	parsed.Path = "/" + database
	missingDSN := parsed.String()
	admin := chtest.Open(t, dsn, "default")
	status, err := ReadStatus(ctx, Target{URL: missingDSN}, fixtureMachine, "", nil, nil)
	require.NoError(t, err)
	assert.True(t, status.SchemaMissing)
	require.NoError(t, EnsureSchema(ctx, Target{URL: missingDSN}))
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+database+" SYNC")
	})
	conn := chtest.Open(t, missingDSN, database)
	assert.Equal(t, 0, chtest.Count(t, conn, "sessions", ""))
}

func TestPushMirrorsEveryTableAndSkipsUnchanged(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})

	first, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, first.SessionsPushed)
	assert.Equal(t, 4, first.MessagesPushed)
	assert.Zero(t, first.Errors)
	assert.True(t, first.Full, "the first push from an archive is full")
	assert.Equal(t, "first push from this archive", first.FullReason)

	conn := chtest.Open(t, target.URL, target.Database)
	assert.Equal(t, 3, chtest.Count(t, conn, "sessions", ""))
	assert.Equal(t, 4, chtest.Count(t, conn, "messages", ""))
	assert.Equal(t, 2, chtest.Count(t, conn, "tool_calls", ""))
	assert.Equal(t, 1, chtest.Count(t, conn, "tool_result_events", ""))
	assert.Equal(t, 1, chtest.Count(t, conn, "usage_events", ""))
	assert.Equal(t, 1, chtest.Count(t, conn, "secret_findings", ""))
	assert.Equal(t, 1, chtest.Count(t, conn, "starred_sessions", ""))
	assert.Equal(t, 1, chtest.Count(t, conn, "pinned_messages", ""))

	var machine, archive, fp string
	var lastMessageAt *string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT machine, source_archive_id, agentsview_push_fingerprint,
		        toString(last_message_at)
		 FROM sessions WHERE id = ?`, fixtureAlphaID,
	).Scan(&machine, &archive, &fp, &lastMessageAt))
	assert.Equal(t, fixtureMachine, machine, "local sentinel machine takes the push machine")
	assert.Equal(t, s.archiveID, archive)
	assert.Len(t, fp, 64)
	require.NotNil(t, lastMessageAt)
	assert.Contains(t, *lastMessageAt, "2026-01-10 00:01:00")

	var summary string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT result_content FROM tool_calls WHERE session_id = ?`, fixtureAlphaID).Scan(&summary))
	assert.Empty(t, summary, "a summary equal to its single event is not stored")

	second, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, second.SessionsPushed)
	assert.False(t, second.Full)
	// The cutoff moved past every seeded sync_marker, so the window is empty
	// and nothing is even fingerprinted.
	assert.Zero(t, second.SkippedUnchanged)

	status, err := s.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, status.Sessions)
	assert.Equal(t, 4, status.Messages)
	assert.Equal(t, fixtureMachine, status.LastPushMachine)
	assert.Equal(t, SchemaVersion, status.SchemaVersion)
	assert.NotEmpty(t, status.LastPushAt)
}

func TestPushReplacesChangedSessionsWithoutLeavingOldRows(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	appendMessage(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "only the changed session is rewritten")
	assert.Equal(t, 2, res.MessagesPushed)

	conn := chtest.Open(t, target.URL, target.Database)
	assert.Equal(t, 2, chtest.Count(t, conn, "messages", "session_id = ?", fixtureBetaID))
	var minVersion, maxVersion uint64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT min(push_version), max(push_version) FROM messages WHERE session_id = ?`,
		fixtureBetaID).Scan(&minVersion, &maxVersion))
	assert.Equal(t, maxVersion, minVersion, "older message rows were deleted")
	var sessionVersion uint64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT push_version FROM sessions WHERE id = ?`, fixtureBetaID).Scan(&sessionVersion))
	assert.Equal(t, maxVersion, sessionVersion)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT message_count FROM sessions WHERE id = ?`, fixtureBetaID).Scan(&count))
	assert.Equal(t, 2, count)

	// A full push rewrites everything and leaves exactly one row per key.
	full, err := s.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, full.SessionsPushed)
	assert.Equal(t, "requested", full.FullReason)
	assert.Equal(t, 5, chtest.Count(t, conn, "messages", ""))
	assert.Equal(t, 3, chtest.Count(t, conn, "sessions", ""))
}

func TestPushRemovesUsageForReplacedMessages(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	first, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, first.Errors)
	conn := chtest.Open(t, target.URL, target.Database)
	require.Equal(t, 2, chtest.Count(t, conn, "usage_messages", "session_id = ?", fixtureAlphaID))

	session, err := local.GetSessionFull(ctx, fixtureAlphaID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	for _, remaining := range []int{1, 0} {
		session.MessageCount = remaining
		_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: *session, Messages: messages[:remaining],
			DataVersion: 1, ReplaceMessages: true,
		}})
		require.NoError(t, err)
		beforePush, err := local.GetAllMessages(ctx, fixtureAlphaID)
		require.NoError(t, err)
		result, err := s.Push(ctx, true, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
		assert.Equal(t, remaining, chtest.Count(t, conn, "usage_messages", "session_id = ?", fixtureAlphaID))
		assert.Equal(t, remaining, chtest.Count(t, conn, "messages", "session_id = ?", fixtureAlphaID))
		current, err := local.GetAllMessages(ctx, fixtureAlphaID)
		require.NoError(t, err)
		assert.Equal(t, beforePush, current, "pushing preserves the source messages")
		assert.Equal(t, 2, chtest.Count(t, conn, "usage_messages", "session_id != ?", fixtureAlphaID))
		var oldRows int
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT count() FROM usage_messages m JOIN sessions s ON s.id = m.session_id
			 WHERE m.session_id = ? AND m.push_version < s.push_version`,
			fixtureAlphaID).Scan(&oldRows))
		assert.Zero(t, oldRows)
	}
}

func TestPushRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	conn := chtest.Open(t, target.URL, target.Database)
	// Views fill these on insert but never see a delete.
	derived := []string{"usage_messages", "terminal_event_snapshots"}
	for _, table := range derived {
		require.Equal(t, 1, chtest.Count(t, conn, table, "session_id = ?", fixtureBetaID), table)
	}

	require.NoError(t, local.SoftDeleteSession(t.Context(), fixtureBetaID))
	deleted, err := local.DeleteSessionIfTrashed(t.Context(), fixtureBetaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeletedStale)

	assert.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID))
	assert.Zero(t, chtest.Count(t, conn, "messages", "session_id = ?", fixtureBetaID))
	for _, table := range derived {
		assert.Zero(t, chtest.Count(t, conn, table, "session_id = ?", fixtureBetaID), table)
	}
	assert.Equal(t, 2, chtest.Count(t, conn, "sessions", ""))
}

func TestPushRecoversFromFailureBeforeSessionRows(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	appendMessage(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:30:00.000Z")
	before, err := s.Status(ctx)
	require.NoError(t, err)

	boom := errors.New("simulated crash before session rows")
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error { return boom }}
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err, "a per-session failure is counted, not fatal")
	assert.Equal(t, 1, res.Errors)
	assert.Zero(t, res.SessionsPushed)

	conn := chtest.Open(t, target.URL, target.Database)
	assert.Equal(t, 3, chtest.Count(t, conn, "messages", "session_id = ?", fixtureAlphaID),
		"dependents landed before the failure")
	var count int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT message_count FROM sessions WHERE id = ?`, fixtureAlphaID).Scan(&count))
	assert.Equal(t, 2, count, "the session row keeps the old fingerprint and count")
	after, err := s.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, before.LastPushAt, after.LastPushAt, "the cursor does not advance past a failure")

	s.hooks = nil
	repaired, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, repaired.SessionsPushed)
	assert.Zero(t, repaired.Errors)
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT message_count FROM sessions WHERE id = ?`, fixtureAlphaID).Scan(&count))
	assert.Equal(t, 3, count)
	assert.Equal(t, 3, chtest.Count(t, conn, "messages", "session_id = ?", fixtureAlphaID))
}

func TestPushHonorsProjectScopeAndRemovesMovedSessions(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{Projects: []string{"alpha"}})
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, res.SessionsPushed, "alpha and its child")

	conn := chtest.Open(t, target.URL, target.Database)
	assert.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID))
	assert.Equal(t, 2, chtest.Count(t, conn, "sessions", ""))

	// Move the child out of scope locally and push again: the scope filter
	// would never list it, but the unfiltered window does, so its rows go.
	child, err := local.GetSessionFull(ctx, fixtureChildID)
	require.NoError(t, err)
	child.Project = "elsewhere"
	moved := "2026-01-12T00:00:00.000Z"
	child.LocalModifiedAt = &moved
	msgs, err := local.GetAllMessages(ctx, fixtureChildID)
	require.NoError(t, err)
	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: *child, Messages: msgs, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)

	res, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeletedStale)
	assert.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", fixtureChildID))
	assert.Zero(t, chtest.Count(t, conn, "messages", "session_id = ?", fixtureChildID))

	// Changing the scope forces a full push.
	wider := newTestSync(t, local, target, storage.PusherOptions{})
	res, err = wider.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, "push scope changed", res.FullReason)
	assert.Equal(t, 3, chtest.Count(t, conn, "sessions", ""))
}

func TestPushRefreshesCurationWithoutContentChange(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	conn := chtest.Open(t, target.URL, target.Database)
	assert.Equal(t, 1, chtest.Count(t, conn, "starred_sessions", ""))

	require.NoError(t, local.UnstarSession(t.Context(), fixtureAlphaID))
	ok, err := local.StarSession(t.Context(), fixtureBetaID)
	require.NoError(t, err)
	require.True(t, ok)

	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, res.SessionsPushed, "stars do not change session content")
	assert.Equal(t, 1, chtest.Count(t, conn, "starred_sessions", ""))
	assert.Equal(t, 1, chtest.Count(t, conn, "starred_sessions", "session_id = ?", fixtureBetaID))
}

func TestEnsureSchemaUpgradesOlderMirrorInPlace(t *testing.T) {
	ctx := context.Background()
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	require.NoError(t, EnsureSchema(ctx, target))

	conn := chtest.Open(t, dsn, database)
	_, err := conn.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN last_message_at")
	require.NoError(t, err)
	require.Error(t, CheckSchemaCompat(ctx, conn))

	require.NoError(t, EnsureSchema(ctx, target))
	assert.NoError(t, CheckSchemaCompat(ctx, conn))
	assert.NoError(t, CheckDataVersionCompat(ctx, conn))
}
