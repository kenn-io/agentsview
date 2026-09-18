package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteSession_LargeSessionFTSDelete(t *testing.T) {
	require := require.New(t)

	if testing.Short() {
		t.Skip("skipping perf test in -short mode")
	}
	t.Parallel()
	d := openLargeSessionFixtureDB(t, true)
	requireFTS(t, d)

	start := time.Now()
	require.NoError(d.DeleteSession(largeSessionFixtureID), "DeleteSession")
	elapsed := time.Since(start)
	require.LessOrEqual(elapsed, largeSessionPerfCeiling,
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
	require.NoError(err, "neighbor pins count")
	assert.Equal(t, crossSessionNeighborCount, neighborPins,
		"neighbor pins count")
}

func TestFindSessionIDsByPartial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "abcdef-1111-2222", "proj")
	insertSession(t, d, "abcdef-3333-4444", "proj")
	insertSession(t, d, "fedcba-5555", "proj")

	ctx := t.Context()

	got, err := d.FindSessionIDsByPartial(ctx, "abcdef", 5)
	require.NoError(err, "FindSessionIDsByPartial")
	assert.Len(got, 2, "abcdef matches")

	got, err = d.FindSessionIDsByPartial(ctx, "fedcba", 5)
	require.NoError(err, "FindSessionIDsByPartial")
	assert.Equal([]string{"fedcba-5555"}, got, "fedcba matches")

	got, err = d.FindSessionIDsByPartial(ctx, "nope", 5)
	require.NoError(err, "FindSessionIDsByPartial")
	assert.Empty(got, "nope matches")

	got, err = d.FindSessionIDsByPartial(ctx, "", 5)
	require.NoError(err, "FindSessionIDsByPartial")
	assert.Nil(got, "empty input")
}

func TestFindSessionIDsByPartialLiteralCaseSensitive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "abc_def", "proj")
	insertSession(t, d, "abcXdef", "proj")
	insertSession(t, d, "abc%def", "proj")
	insertSession(t, d, "ABCdef", "proj")

	ctx := t.Context()

	got, err := d.FindSessionIDsByPartial(ctx, "c_d", 10)
	require.NoError(err, "underscore lookup")
	assert.Equal([]string{"abc_def"}, got)

	got, err = d.FindSessionIDsByPartial(ctx, "c%d", 10)
	require.NoError(err, "percent lookup")
	assert.Equal([]string{"abc%def"}, got)

	got, err = d.FindSessionIDsByPartial(ctx, "abc", 10)
	require.NoError(err, "case-sensitive lookup")
	assert.ElementsMatch([]string{"abc_def", "abcXdef", "abc%def"}, got)
	assert.NotContains(got, "ABCdef")
}

func TestFindSessionIDsByRawSuffix(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "plain-id", "exact")
	insertSession(t, d, "codex:uuid", "agent")
	insertSession(t, d, "host~uuid", "host")
	insertSession(t, d, "host~uuid-fork", "fork")
	insertSession(t, d, "host~P-E", "entry")
	insertSession(t, d, "host~wild_%_literal", "wild")
	insertSession(t, d, "host~trashed", "trash")
	require.NoError(d.SoftDeleteSession("host~trashed"))

	ctx := t.Context()
	got, err := d.FindSessionIDsByRawSuffix(ctx, "uuid", 2)
	require.NoError(err)
	assert.ElementsMatch([]string{"codex:uuid", "host~uuid"}, got)
	uuidIDs := append([]string(nil), got...)

	got, err = d.FindSessionIDsByRawSuffix(ctx, "plain-id", 2)
	require.NoError(err)
	assert.Equal([]string{"plain-id"}, got)
	exactIDs := append([]string(nil), got...)

	got, err = d.FindSessionIDsByRawSuffix(ctx, "wild_%_literal", 2)
	require.NoError(err)
	assert.Equal([]string{"host~wild_%_literal"}, got)
	wildcardIDs := append([]string(nil), got...)

	got, err = d.FindSessionIDsByRawSuffix(ctx, "trashed", 2)
	require.NoError(err)
	assert.Empty(got)
	trashedIDs := append([]string(nil), got...)

	got, err = d.FindSessionIDsByRawSuffix(ctx, "E", 2)
	require.NoError(err)
	assert.Empty(got)
	t.Logf("head: sqlite_uuid=%v exact=%v wildcard=%v trashed=%v entry=%v", uuidIDs, exactIDs, wildcardIDs, trashedIDs, got)
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
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	sessionName := "My Chat Title"
	err := d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		SessionName:  &sessionName,
		MessageCount: 1,
	})
	require.NoError(err, "UpsertSession insert")

	// Verify session_name is visible via COALESCE in GetSession.
	s, err := d.GetSession(ctx, "claude-ai:dn-test")
	require.NoError(err, "GetSession after insert")
	require.NotNil(s, "GetSession returned nil after insert")
	require.NotNil(s.DisplayName, "DisplayName is nil after insert")
	assert.Equal("My Chat Title", *s.DisplayName, "DisplayName")

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
	require.NoError(err, "UpsertSession update")

	// session_name should be updated on re-upsert.
	s, err = d.GetSession(ctx, "claude-ai:dn-test")
	require.NoError(err, "GetSession after re-upsert")
	require.NotNil(s, "GetSession returned nil after re-upsert")
	require.NotNil(s.DisplayName, "DisplayName is nil after re-upsert")
	assert.Equal("Updated Title", *s.DisplayName,
		"session_name should be updated on re-upsert")
	// Other fields should also update.
	assert.Equal(2, s.MessageCount, "MessageCount")
}

// TestUpsertSessionDoesNotAdvanceDataVersion guards the
// invariant that data_version is never touched by
// UpsertSession -- it must only advance via
// SetSessionDataVersion after a successful message rewrite,
// so a transient write failure cannot leave a session row
// stamped at the current parser version with stale
// messages.
func TestUpsertSessionDoesNotAdvanceDataVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	// New session: data_version stays 0 even when the
	// caller passes a non-zero value on the struct.
	require.NoError(d.UpsertSession(Session{
		ID:           "dv-1",
		Project:      "p",
		Machine:      "m",
		Agent:        "claude",
		MessageCount: 1,
		DataVersion:  CurrentDataVersion(),
	}), "UpsertSession (insert)")
	assert.Equal(0, d.GetSessionDataVersion("dv-1"),
		"after insert, data_version")

	// Stamp a current value to simulate a successful write.
	require.NoError(d.SetSessionDataVersion(
		"dv-1", CurrentDataVersion(),
	), "SetSessionDataVersion")
	assert.Equal(CurrentDataVersion(), d.GetSessionDataVersion("dv-1"),
		"after Set, data_version")

	// Re-upserting (e.g. as part of an incremental sync)
	// must NOT clobber the stamped version with the
	// struct's value (here 0), and must NOT replace it
	// with a future "current" value before the rewrite
	// succeeds.
	require.NoError(d.UpsertSession(Session{
		ID:           "dv-1",
		Project:      "p",
		Machine:      "m",
		Agent:        "claude",
		MessageCount: 5,
		DataVersion:  0,
	}), "UpsertSession (update)")
	assert.Equal(CurrentDataVersion(), d.GetSessionDataVersion("dv-1"),
		"after re-upsert, data_version (must be preserved across UpsertSession)")
}

func TestSetSessionDataVersionsIsAtomic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	for _, id := range []string{"source-main", "source-fork"} {
		require.NoError(d.UpsertSession(Session{
			ID: id, Project: "p", Machine: "m", Agent: "claude",
		}))
		require.NoError(d.SetSessionDataVersion(id, 1))
	}

	raw, err := sql.Open("sqlite3", d.Path())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `
		CREATE TRIGGER fail_fork_promotion
		BEFORE UPDATE OF data_version ON sessions
		WHEN NEW.id = 'source-fork' AND NEW.data_version > OLD.data_version
		BEGIN
			SELECT RAISE(FAIL, 'injected fork promotion failure');
		END;
	`)
	require.NoError(err)

	err = d.SetSessionDataVersions(
		[]string{"source-main", "source-fork"}, CurrentDataVersion(),
	)
	require.ErrorContains(err, "injected fork promotion failure")
	assert.Equal(1, d.GetSessionDataVersion("source-main"),
		"a later member failure must roll back an earlier promotion")
	assert.Equal(1, d.GetSessionDataVersion("source-fork"))

	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_fork_promotion`)
	require.NoError(err)
	require.NoError(d.SetSessionDataVersions(
		[]string{"source-main", "source-fork"}, CurrentDataVersion(),
	))
	assert.Equal(CurrentDataVersion(),
		d.GetSessionDataVersion("source-main"))
	assert.Equal(CurrentDataVersion(),
		d.GetSessionDataVersion("source-fork"))
}

func TestSessionTranscriptFidelityRoundTrips(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	s := Session{
		ID:                 "antigravity-cli:fidelity-rt",
		Agent:              "antigravity-cli",
		TranscriptFidelity: "summary",
	}
	require.NoError(d.UpsertSession(s))

	got, err := d.GetSession(ctx, s.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(t, "summary", got.TranscriptFidelity)
}

func TestUpsertSessionTerminationStatus(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(d.UpsertSession(s), "upsert")

			got, err := d.GetSession(ctx, id)
			require.NoError(err, "get")
			require.NotNil(got, "session not found")

			if tc.val == nil {
				assert.Nil(got.TerminationStatus, "nil mismatch")
			} else {
				require.NotNil(got.TerminationStatus, "nil mismatch")
				assert.Equal(*tc.val, *got.TerminationStatus, "value mismatch")
			}
		})
	}
}

func TestListSessionsTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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
	assert := assert.New(t)

	d := testDB(t)

	// Agent name lands on a fresh row via session_name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("agent-one"),
	}), "insert agent name")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.SessionName)
	assert.Equal("agent-one", *got.SessionName)
	assert.Nil(got.DisplayName, "display_name not set by upsert")

	// A newer agent name overwrites session_name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("agent-two"),
	}), "update agent name")
	got = getSessionRow(t, d, "s1")
	assert.Equal("agent-two", *got.SessionName, "session_name updated on re-upsert")
	assert.Nil(got.DisplayName, "display_name still not set by upsert")

	// A manual rename sets display_name.
	requireNoError(t, d.RenameSession("s1", Ptr("user-name")), "rename")

	// A subsequent agent name must NOT overwrite the user's display_name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("agent-three"),
	}), "agent after user")
	got = getSessionRow(t, d, "s1")
	assert.Equal("user-name", *got.DisplayName, "user display_name must survive re-parse")
	assert.Equal("agent-three", *got.SessionName, "session_name always updated")
}

// TestGetSessionFullPopulatesSessionName verifies that GetSessionFull
// keeps the raw session_name on the Go struct while DisplayName carries
// the visible name (user rename, else session_name), matching the PG and
// DuckDB GetSessionFull implementations. session_name is a backend-only
// field: it is NOT serialised in JSON responses (json:"-" on
// Session.SessionName). Push paths that need display_name unmerged read
// via ListSessionsModifiedBetween, not GetSessionFull.
func TestGetSessionFullPopulatesSessionName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

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
	require.NoError(err, "GetSessionFull")
	require.NotNil(s, "session not found")
	require.NotNil(s.SessionName, "SessionName is nil after GetSessionFull")
	assert.Equal("Agent Title", *s.SessionName, "SessionName round-trips")
	// No user rename yet: DisplayName falls back to session_name.
	require.NotNil(s.DisplayName, "DisplayName coalesces to session_name")
	assert.Equal("Agent Title", *s.DisplayName, "visible name before rename")

	// After a manual rename, display_name is set; session_name is unchanged.
	requireNoError(t, d.RenameSession("s-ns", Ptr("User Title")), "rename")
	s, err = d.GetSessionFull(ctx, "s-ns")
	require.NoError(err, "GetSessionFull after rename")
	require.NotNil(s, "session not found after rename")
	require.NotNil(s.DisplayName, "DisplayName should be set after rename")
	assert.Equal("User Title", *s.DisplayName, "display_name after rename")
	require.NotNil(s.SessionName, "SessionName should still be set after rename")
	assert.Equal("Agent Title", *s.SessionName, "SessionName unchanged after rename")
}

func TestSessionIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "sqlite-identity", "sqlite-identity", func(s *Session) {
		s.Agent = "claude"
		s.AgentLabel = "Claude Triage"
		s.Entrypoint = "sdk-cli"
		s.SessionKind = "bg"
		s.SessionName = Ptr("Agent Title")
		s.StartedAt = Ptr("2024-06-15T08:00:00Z")
		s.EndedAt = Ptr("2024-06-15T09:00:00Z")
		s.CreatedAt = "2024-06-15T08:00:00Z"
		s.UserMessageCount = 1
	})

	index, err := d.GetSidebarSessionIndex(ctx, SessionFilter{
		Project: "sqlite-identity",
	})
	require.NoError(err)
	require.Len(index.Sessions, 1)
	assert.Equal("sqlite-identity", index.Sessions[0].ID)
	assert.Equal("Claude Triage", index.Sessions[0].AgentLabel)
	assert.Equal("sdk-cli", index.Sessions[0].Entrypoint)
	assert.Equal("bg", index.Sessions[0].SessionKind)
	require.NotNil(index.Sessions[0].DisplayName)
	assert.Equal("Agent Title", *index.Sessions[0].DisplayName)
}

func TestSessionIdentityAbsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "sqlite-identity-absent", "sqlite-identity-absent", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T08:00:00Z")
		s.CreatedAt = "2024-06-15T08:00:00Z"
		s.UserMessageCount = 1
	})

	session, err := d.GetSession(ctx, "sqlite-identity-absent")
	require.NoError(err)
	assert.Equal("", session.AgentLabel)
	assert.Equal("", session.Entrypoint)

	index, err := d.GetSidebarSessionIndex(ctx, SessionFilter{
		Project: "sqlite-identity-absent",
	})
	require.NoError(err)
	require.Len(index.Sessions, 1)
	assert.Equal("", index.Sessions[0].AgentLabel)
	assert.Equal("", index.Sessions[0].Entrypoint)
	assert.Equal("", index.Sessions[0].SessionKind)
}

func TestSessionKindAndPromptSourcePersist(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "sk-persist", "sk-persist", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.SessionKind = "bg"
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	insertMessages(t, d,
		Message{
			SessionID: "sk-persist", Ordinal: 0, Role: "user",
			Content: "first", PromptSource: "typed",
		},
		Message{
			SessionID: "sk-persist", Ordinal: 1, Role: "user",
			Content: "second", PromptSource: "queued",
		},
	)

	session, err := d.GetSession(ctx, "sk-persist")
	require.NoError(err)
	assert.Equal("bg", session.SessionKind)
	assert.Equal("sdk-cli", session.Entrypoint)

	msgs, err := d.GetMessages(ctx, "sk-persist", 0, 10, true)
	require.NoError(err)
	require.Len(msgs, 2)
	assert.Equal("typed", msgs[0].PromptSource)
	assert.Equal("queued", msgs[1].PromptSource)
}

func TestSessionKindAndPromptSourceDefaultEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	// A session/message written without the new fields reads back empty,
	// so historical rows and other agents are unaffected.
	insertSession(t, d, "sk-default", "sk-default", func(s *Session) {
		s.Agent = "codex"
	})
	insertMessages(t, d, Message{
		SessionID: "sk-default", Ordinal: 0, Role: "user", Content: "x",
	})

	session, err := d.GetSession(ctx, "sk-default")
	require.NoError(err)
	assert.Equal("", session.SessionKind)

	msgs, err := d.GetMessages(ctx, "sk-default", 0, 10, true)
	require.NoError(err)
	require.Len(msgs, 1)
	assert.Equal("", msgs[0].PromptSource)
}

func TestGetSessionName(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Agent Name"),
	}), "upsert")

	requireNoError(t, d.RenameSession("s1", Ptr("User Name")), "rename")
	got := getSessionRow(t, d, "s1")
	require.NotNil(got.DisplayName, "display_name set after rename")
	assert.Equal("User Name", *got.DisplayName)
	// session_name is unchanged by RenameSession.
	require.NotNil(got.SessionName, "session_name not cleared by rename")
	assert.Equal("Agent Name", *got.SessionName)

	requireNoError(t, d.RenameSession("s1", nil), "clear")
	got = getSessionRow(t, d, "s1")
	assert.Nil(got.DisplayName, "display_name cleared")
	// session_name persists after clearing the user rename.
	require.NotNil(got.SessionName, "session_name persists")
	assert.Equal("Agent Name", *got.SessionName)
}

func TestSessionNameCOALESCEInGetSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	// Session with only session_name — GetSession should return it via COALESCE.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Agent Title"), MessageCount: 1,
	}), "upsert with session_name")
	s, err := d.GetSession(ctx, "s1")
	require.NoError(err)
	require.NotNil(s.DisplayName)
	assert.Equal("Agent Title", *s.DisplayName, "COALESCE returns session_name when no user rename")

	// User renames — display_name wins.
	requireNoError(t, d.RenameSession("s1", Ptr("User Title")), "rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(err)
	require.NotNil(s.DisplayName)
	assert.Equal("User Title", *s.DisplayName, "display_name wins over session_name")

	// Clear rename — session_name visible again.
	requireNoError(t, d.RenameSession("s1", nil), "clear rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(err)
	require.NotNil(s.DisplayName)
	assert.Equal("Agent Title", *s.DisplayName, "session_name restored after clearing rename")
}
