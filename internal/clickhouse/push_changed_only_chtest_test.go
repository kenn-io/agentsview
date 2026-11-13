//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// pushVersions reads a session's published and content versions and the
// push versions of its stored message rows.
type pushVersions struct {
	published, content uint64
	messages           []uint64
}

func readPushVersions(t *testing.T, conn *sql.DB, id string) pushVersions {
	t.Helper()
	var v pushVersions
	require.NoError(t, conn.QueryRowContext(t.Context(),
		"SELECT push_version, content_version FROM sessions WHERE id = ?", id,
	).Scan(&v.published, &v.content))
	// The newest version of each message, read before merges so a rewrite
	// with identical content still shows.
	rows, err := conn.QueryContext(t.Context(),
		"SELECT max(push_version) FROM messages WHERE session_id = ? GROUP BY ordinal ORDER BY ordinal SETTINGS final = 0", id)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var version uint64
		require.NoError(t, rows.Scan(&version))
		v.messages = append(v.messages, version)
	}
	require.NoError(t, rows.Err())
	return v
}

// changedOnlyFixture pushes the fixture once and returns a Sync for the
// next push with its mirror connection.
func changedOnlyFixture(t *testing.T) (*db.DB, *Sync, *sql.DB) {
	t.Helper()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(t.Context(), false, nil)
	require.NoError(t, err)
	return local, s, chtest.Open(t, target.URL, target.Database)
}

// A change to session fields alone, such as a rename or the data version a
// resync stamps, rewrites the session row and keeps the stored messages.
// Reads that pair those messages with the session keep finding them.
func TestPushRewritesOnlySessionRowWhenContentUnchanged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	before := readPushVersions(t, conn, fixtureAlphaID)
	var lastTerminalBefore sql.NullTime
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT last_terminal_at FROM terminal_event_snapshots WHERE session_id = ?", fixtureAlphaID,
	).Scan(&lastTerminalBefore))

	title := "Renamed Alpha"
	require.NoError(t, local.RenameSession(ctx, fixtureAlphaID, &title))
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Full)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 1, res.SessionRowsOnly)
	assert.Zero(t, res.MessagesPushed)

	after := readPushVersions(t, conn, fixtureAlphaID)
	assert.Greater(t, after.published, before.published, "the session row was republished")
	assert.Equal(t, before.content, after.content, "the row keeps the version of the kept messages")
	assert.Equal(t, before.messages, after.messages, "no message row was written")
	var name string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT display_name FROM sessions WHERE id = ?", fixtureAlphaID).Scan(&name))
	assert.Equal(t, title, name)
	var snapshotVersion uint64
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT push_version FROM "+usageSessionSnapshotTable+" WHERE id = ?", fixtureAlphaID,
	).Scan(&snapshotVersion))
	assert.Equal(t, after.published, snapshotVersion, "the usage snapshot follows the session row")

	var usageRows int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT count() FROM usage_messages m JOIN sessions s ON s.id = m.session_id WHERE s.id = ? AND "+
			chUsageMessageCurrent, fixtureAlphaID).Scan(&usageRows))
	assert.Equal(t, 2, usageRows, "usage reads still see the kept messages")
	var terminalVersion uint64
	var lastTerminalAfter sql.NullTime
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT push_version, last_terminal_at FROM terminal_event_snapshots WHERE session_id = ?", fixtureAlphaID,
	).Scan(&terminalVersion, &lastTerminalAfter))
	assert.Equal(t, after.published, terminalVersion)
	assert.Equal(t, lastTerminalBefore, lastTerminalAfter)

	store := newTestStore(t, conn, s.target)
	binding, err := store.SessionSourceBinding(ctx, fixtureAlphaID)
	require.NoError(t, err, "the kept messages count as the published content")
	assert.NotEmpty(t, binding)

	again, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, again.SessionsPushed, "the republished row records the current fingerprint")
}

// A message change after a session-row-only push rewrites the content and
// removes the rows of the version the row kept.
func TestPushRewritesContentAfterSessionRowOnlyPush(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	title := "Renamed Beta"
	require.NoError(t, local.RenameSession(ctx, fixtureBetaID, &title))
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	appendMessage(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, res.SessionRowsOnly)
	assert.Equal(t, 2, res.MessagesPushed)
	after := readPushVersions(t, conn, fixtureBetaID)
	assert.Equal(t, after.published, after.content)
	assert.Equal(t, []uint64{after.content, after.content}, after.messages)
}

// The archive can change between planning a session-row-only push and
// writing it; the batch then writes the content it loaded.
func TestSessionBatchRewritesContentChangedSincePlan(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	before := readPushVersions(t, conn, fixtureBetaID)
	sess, err := local.GetSessionFull(ctx, fixtureBetaID)
	require.NoError(t, err)
	planned, err := s.loadPayload(ctx, *sess)
	require.NoError(t, err)
	writes := map[string]sessionWrite{fixtureBetaID: {
		row: true, keptContent: planned.fingerprint.content, keptVersion: before.content,
	}}

	appendMessage(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
	sess, err = local.GetSessionFull(ctx, fixtureBetaID)
	require.NoError(t, err)
	version := newPushVersion()
	written, err := s.pushSessionBatch(ctx, []db.Session{*sess}, writes,
		map[string]sessionFingerprint{}, version, map[string]sessionPayload{})
	require.NoError(t, err)
	require.Len(t, written, 1)
	assert.True(t, written[0].write.content)

	after := readPushVersions(t, conn, fixtureBetaID)
	assert.Equal(t, version, after.content)
	assert.Equal(t, []uint64{version, version}, after.messages)
}

// Each automatic full-push reason compares every session with the mirror
// and rewrites only what differs, while still removing mirror sessions
// missing locally.
func TestFullPushRewritesOnlyChangedSessions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
		opts   storage.PushOptions
		force  func(t *testing.T, s *Sync, conn *sql.DB)
	}{
		{
			name:   "rebuilt archive",
			reason: "archive was rebuilt",
			force: func(t *testing.T, s *Sync, conn *sql.DB) {
				require.NoError(t, writeMetadata(t.Context(), conn, map[string]string{
					s.archiveKey(databaseIDKeyBase): "previous-generation",
				}))
			},
		},
		{
			name:   "deletion journal reset",
			reason: "local deletion journal was reset",
			force: func(t *testing.T, s *Sync, conn *sql.DB) {
				require.NoError(t, writeMetadata(t.Context(), conn, map[string]string{
					s.archiveKey(deletionRevisionKeyBase): "1000000",
				}))
			},
		},
		{
			name:   "push scope changed",
			reason: "push scope changed",
			force: func(t *testing.T, s *Sync, conn *sql.DB) {
				require.NoError(t, writeMetadata(t.Context(), conn, map[string]string{
					s.archiveKey(pushScopeKeyBase): "projects=other",
				}))
			},
		},
		{
			// The CLI resyncs a stale archive, then pushes with Full set.
			name: "automatic resync",
			opts: storage.PushOptions{Full: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			local, s, conn := changedOnlyFixture(t)
			alphaBefore := readPushVersions(t, conn, fixtureAlphaID)
			const retired = "ch-sync-retired"
			_, err := conn.ExecContext(ctx, `INSERT INTO sessions SELECT * REPLACE (
					? AS id, push_version + 1 AS push_version)
				FROM (SELECT * FROM sessions WHERE id = ?)`, retired, fixtureAlphaID)
			require.NoError(t, err)
			appendMessage(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
			if tc.force != nil {
				tc.force(t, s, conn)
			}

			res, err := s.PushWithOptions(ctx, tc.opts, nil)
			require.NoError(t, err)
			assert.True(t, res.Full)
			assert.Equal(t, tc.reason, res.FullReason)
			assert.Equal(t, 1, res.SessionsPushed, "only the changed session is rewritten")
			assert.Equal(t, 2, res.MessagesPushed)
			assert.Equal(t, 2, res.SkippedUnchanged)
			assert.Equal(t, 1, res.DeletedStale, "the full push still removes sessions missing locally")
			assert.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", retired))
			assert.Equal(t, alphaBefore, readPushVersions(t, conn, fixtureAlphaID))
		})
	}
}

// Sessions whose mirror rows predate the current dashboard facts get their
// session row republished with the facts; their messages stay.
func TestStoredCountRepairRewritesOnlyStaleSessionRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, s, conn := changedOnlyFixture(t)
	before := readPushVersions(t, conn, fixtureAlphaID)
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions SELECT * REPLACE (
			0 AS dashboard_facts_version, push_version + 1 AS push_version)
		FROM (SELECT * FROM sessions WHERE id = ?)`, fixtureAlphaID)
	require.NoError(t, err)

	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "publishing stored session counts", res.FullReason)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 1, res.SessionRowsOnly)
	assert.Equal(t, 2, res.SkippedUnchanged)
	var facts int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT dashboard_facts_version FROM sessions WHERE id = ?", fixtureAlphaID).Scan(&facts))
	assert.Equal(t, dashboardFactsVersion, facts)
	after := readPushVersions(t, conn, fixtureAlphaID)
	assert.Equal(t, before.content, after.content)
	assert.Equal(t, before.messages, after.messages)
}

// A full push takes over sessions whose rows name another archive by
// republishing their session rows, not their content.
func TestFullPushTakesOverSessionRowsOfAnotherArchive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, s, conn := changedOnlyFixture(t)
	before := readPushVersions(t, conn, fixtureAlphaID)
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions SELECT * REPLACE (
			'other-archive' AS source_archive_id, push_version + 1 AS push_version)
		FROM (SELECT * FROM sessions WHERE id = ?)`, fixtureAlphaID)
	require.NoError(t, err)

	incremental, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, incremental.SessionsPushed, "an incremental push leaves other archives' rows alone")

	res, err := s.PushWithOptions(ctx, storage.PushOptions{Full: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionRowsOnly)
	var archive string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT source_archive_id FROM sessions WHERE id = ?", fixtureAlphaID).Scan(&archive))
	assert.Equal(t, s.archiveID, archive)
	after := readPushVersions(t, conn, fixtureAlphaID)
	assert.Equal(t, before.content, after.content)
	assert.Equal(t, before.messages, after.messages)
}

// --full, and markers whose rows the fingerprints cannot see, rewrite
// every session.
func TestFullPushRewritesEverySessionWhenRequiredByReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
		opts   storage.PushOptions
		marker string
	}{
		{name: "requested", opts: storage.PushOptions{Full: true, RewriteUnchanged: true}},
		{name: "message dialogue", reason: "preparing message dialogue", marker: messageDialogueReadyKeyBase},
		{name: "usage snapshots", reason: "preparing complete usage snapshots", marker: usageSnapshotReadyKeyBase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			_, s, conn := changedOnlyFixture(t)
			before := readPushVersions(t, conn, fixtureAlphaID)
			if tc.marker != "" {
				require.NoError(t, writeMetadata(ctx, conn, map[string]string{s.archiveKey(tc.marker): ""}))
			}
			res, err := s.PushWithOptions(ctx, tc.opts, nil)
			require.NoError(t, err)
			assert.True(t, res.Full)
			assert.Equal(t, tc.reason, res.FullReason)
			assert.Equal(t, 3, res.SessionsPushed)
			assert.Equal(t, 4, res.MessagesPushed)
			assert.Zero(t, res.SessionRowsOnly)
			after := readPushVersions(t, conn, fixtureAlphaID)
			assert.Greater(t, after.content, before.content)
			assert.Equal(t, []uint64{after.content, after.content}, after.messages)
		})
	}
}

// A missing commit links marker rewrites only commit links: the session
// rows and messages stay.
func TestCommitLinksMarkerRewritesOnlyCommitLinks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, s, conn := changedOnlyFixture(t)
	before := readPushVersions(t, conn, fixtureAlphaID)
	// A link this archive no longer has, as an older format left it.
	_, err := conn.ExecContext(ctx, `INSERT INTO commit_links (session_id, sha, branch, subject,
			repository, message_ordinal, call_index, tool_use_id, committed_at, turn_ordinal,
			turn_text, project, retained, push_version)
		VALUES (?, 'stale', '', '', '', 1, 0, '', '', NULL, 'damaged', '', true, 1)`, fixtureAlphaID)
	require.NoError(t, err)
	require.NoError(t, writeMetadata(ctx, conn, map[string]string{s.archiveKey(commitLinksReadyKeyBase): ""}))

	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "backfilling commit links", res.FullReason)
	assert.Zero(t, res.SessionsPushed)
	assert.Equal(t, 3, res.CommitLinksOnly)
	assert.Zero(t, chtest.Count(t, conn, "commit_links", "session_id = ?", fixtureAlphaID))
	assert.Equal(t, before, readPushVersions(t, conn, fixtureAlphaID))

	next, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, next.Full, "the push recorded the marker")
}

// A full push that rewrites only changed sessions resumes after a restart:
// the sessions the interrupted process wrote match the mirror and are
// skipped like any unchanged session.
func TestChangedOnlyFullPushResumes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	appendMessage(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
	appendMessage(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:10:00.000Z")
	require.NoError(t, writeMetadata(ctx, conn, map[string]string{
		s.archiveKey(databaseIDKeyBase): "previous-generation",
	}))
	killSecondSessionBatch(t, s, false)

	res, err := newTestSync(t, local, s.target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "resuming an unfinished full push, started because: archive was rebuilt", res.FullReason)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 2, res.SkippedUnchanged)
	assert.Zero(t, res.SkippedResumed)

	next, err := newTestSync(t, local, s.target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, next.Full)
	assert.Zero(t, next.SessionsPushed)
}

// A resumed run that rewrites every session must not be continued as one
// that skips unchanged sessions, nor the reverse.
func TestRewriteAllRunIsNotResumedAsChangedOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, _ := changedOnlyFixture(t)
	killCtx, kill := context.WithCancel(ctx)
	batches := 0
	s.sessionBatchSize = 1
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
		batches++
		if batches == 2 {
			kill()
			return killCtx.Err()
		}
		return nil
	}}
	_, err := s.PushWithOptions(killCtx, storage.PushOptions{Full: true, RewriteUnchanged: true}, nil)
	kill()
	require.ErrorIs(t, err, context.Canceled)

	res, err := newTestSync(t, local, s.target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "resuming an unfinished full push, started because: requested", res.FullReason)
	assert.Equal(t, 2, res.SessionsPushed, "the resumed run rewrites the sessions it had not reached")
	assert.Equal(t, 1, res.SkippedResumed)
}
