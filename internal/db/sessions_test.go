package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	displayName := "My Chat Title"
	err := d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		DisplayName:  &displayName,
		NameSource:   Ptr("agent"),
		MessageCount: 1,
	})
	require.NoError(t, err, "UpsertSession insert")

	// Verify display_name was set.
	s, err := d.GetSession(ctx, "claude-ai:dn-test")
	require.NoError(t, err, "GetSession after insert")
	require.NotNil(t, s, "GetSession returned nil after insert")
	require.NotNil(t, s.DisplayName, "DisplayName is nil after insert")
	assert.Equal(t, "My Chat Title", *s.DisplayName, "DisplayName")

	// Re-upsert with a different agent display_name: should overwrite since
	// name_source is 'agent', not 'user'.
	newName := "Updated Title"
	err = d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		DisplayName:  &newName,
		NameSource:   Ptr("agent"),
		MessageCount: 2,
	})
	require.NoError(t, err, "UpsertSession update")

	// Agent display_name should be updated (not preserved) on re-upsert.
	s, err = d.GetSession(ctx, "claude-ai:dn-test")
	require.NoError(t, err, "GetSession after re-upsert")
	require.NotNil(t, s, "GetSession returned nil after re-upsert")
	require.NotNil(t, s.DisplayName, "DisplayName is nil after re-upsert")
	assert.Equal(t, "Updated Title", *s.DisplayName,
		"agent DisplayName should be updated on re-upsert")
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

// getSessionRow reads display_name and name_source directly from the
// sessions table without going through scanSessionRow.
func getSessionRow(t *testing.T, d *DB, id string) Session {
	t.Helper()
	var s Session
	s.ID = id
	requireNoError(t, d.getWriter().QueryRow(
		"SELECT display_name, name_source FROM sessions WHERE id = ?", id).
		Scan(&s.DisplayName, &s.NameSource), "get session row")
	return s
}

func TestUpsertNameSourceOwnership(t *testing.T) {
	d := testDB(t)

	// Agent name lands on a fresh row.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: Ptr("agent-one"), NameSource: Ptr("agent"),
	}), "insert agent name")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "agent-one", *got.DisplayName)
	assert.Equal(t, "agent", *got.NameSource)

	// A newer agent name overwrites an agent-owned row.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: Ptr("agent-two"), NameSource: Ptr("agent"),
	}), "update agent name")
	got = getSessionRow(t, d, "s1")
	assert.Equal(t, "agent-two", *got.DisplayName)

	// A manual rename pins the row.
	requireNoError(t, d.RenameSession("s1", Ptr("user-name")), "rename")

	// A subsequent agent name must NOT overwrite the user name.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: Ptr("agent-three"), NameSource: Ptr("agent"),
	}), "agent after user")
	got = getSessionRow(t, d, "s1")
	assert.Equal(t, "user-name", *got.DisplayName, "user name must survive")
	assert.Equal(t, "user", *got.NameSource)
}

func TestRenameSessionSetsAndClearsNameSource(t *testing.T) {
	d := testDB(t)
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert")

	requireNoError(t, d.RenameSession("s1", Ptr("Name")), "rename")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.NameSource)
	assert.Equal(t, "user", *got.NameSource)

	requireNoError(t, d.RenameSession("s1", nil), "clear")
	got = getSessionRow(t, d, "s1")
	assert.Nil(t, got.DisplayName, "display_name cleared")
	assert.Nil(t, got.NameSource, "name_source cleared")
}
