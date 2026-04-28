package db

import (
	"context"
	"testing"
)

func TestFindSessionIDsByPartial(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "abcdef-1111-2222", "proj")
	insertSession(t, d, "abcdef-3333-4444", "proj")
	insertSession(t, d, "fedcba-5555", "proj")

	ctx := context.Background()

	got, err := d.FindSessionIDsByPartial(ctx, "abcdef", 5)
	if err != nil {
		t.Fatalf("FindSessionIDsByPartial: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("abcdef matches = %v, want 2", got)
	}

	got, err = d.FindSessionIDsByPartial(ctx, "fedcba", 5)
	if err != nil {
		t.Fatalf("FindSessionIDsByPartial: %v", err)
	}
	if len(got) != 1 || got[0] != "fedcba-5555" {
		t.Fatalf("fedcba matches = %v, want [fedcba-5555]",
			got)
	}

	got, err = d.FindSessionIDsByPartial(ctx, "nope", 5)
	if err != nil {
		t.Fatalf("FindSessionIDsByPartial: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("nope matches = %v, want empty", got)
	}

	got, err = d.FindSessionIDsByPartial(ctx, "", 5)
	if err != nil {
		t.Fatalf("FindSessionIDsByPartial: %v", err)
	}
	if got != nil {
		t.Fatalf("empty input = %v, want nil", got)
	}
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
		if err != nil {
			t.Fatalf("UpdateSessionSignals %s: %v", tc.id, err)
		}
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
		if err != nil {
			t.Fatalf("UpdateSessionSignals %s: %v", tc.id, err)
		}
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
		if err != nil {
			t.Fatalf("UpdateSessionSignals %s: %v", tc.id, err)
		}
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

func TestUpsertSession_DisplayNameInsertOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	displayName := "My Chat Title"
	err := d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		DisplayName:  &displayName,
		MessageCount: 1,
	})
	requireNoError(t, err, "UpsertSession insert")

	// Verify display_name was set.
	s, err := d.GetSession(ctx, "claude-ai:dn-test")
	requireNoError(t, err, "GetSession after insert")
	if s == nil {
		t.Fatal("GetSession returned nil after insert")
	}
	if s.DisplayName == nil {
		t.Fatal("DisplayName is nil after insert, want non-nil")
	}
	if *s.DisplayName != "My Chat Title" {
		t.Errorf("DisplayName = %q, want %q", *s.DisplayName, "My Chat Title")
	}

	// Re-upsert with a different display_name.
	newName := "Updated Title"
	err = d.UpsertSession(Session{
		ID:           "claude-ai:dn-test",
		Project:      "claude.ai",
		Machine:      "local",
		Agent:        "claude-ai",
		DisplayName:  &newName,
		MessageCount: 2,
	})
	requireNoError(t, err, "UpsertSession update")

	// display_name should NOT be overwritten by re-upsert.
	s, err = d.GetSession(ctx, "claude-ai:dn-test")
	requireNoError(t, err, "GetSession after re-upsert")
	if s == nil {
		t.Fatal("GetSession returned nil after re-upsert")
	}
	if s.DisplayName == nil {
		t.Fatal("DisplayName is nil after re-upsert, want non-nil")
	}
	if *s.DisplayName != "My Chat Title" {
		t.Errorf(
			"DisplayName = %q after re-upsert, want %q (should be preserved)",
			*s.DisplayName, "My Chat Title",
		)
	}
	// But other fields should update.
	if s.MessageCount != 2 {
		t.Errorf("MessageCount = %d, want 2", s.MessageCount)
	}
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
	if err := d.UpsertSession(Session{
		ID:           "dv-1",
		Project:      "p",
		Machine:      "m",
		Agent:        "claude",
		MessageCount: 1,
		DataVersion:  CurrentDataVersion(),
	}); err != nil {
		t.Fatalf("UpsertSession (insert): %v", err)
	}
	if got := d.GetSessionDataVersion("dv-1"); got != 0 {
		t.Errorf(
			"after insert, data_version = %d, want 0", got,
		)
	}

	// Stamp a current value to simulate a successful write.
	if err := d.SetSessionDataVersion(
		"dv-1", CurrentDataVersion(),
	); err != nil {
		t.Fatalf("SetSessionDataVersion: %v", err)
	}
	if got := d.GetSessionDataVersion("dv-1"); got !=
		CurrentDataVersion() {
		t.Errorf(
			"after Set, data_version = %d, want %d",
			got, CurrentDataVersion(),
		)
	}

	// Re-upserting (e.g. as part of an incremental sync)
	// must NOT clobber the stamped version with the
	// struct's value (here 0), and must NOT replace it
	// with a future "current" value before the rewrite
	// succeeds.
	if err := d.UpsertSession(Session{
		ID:           "dv-1",
		Project:      "p",
		Machine:      "m",
		Agent:        "claude",
		MessageCount: 5,
		DataVersion:  0,
	}); err != nil {
		t.Fatalf("UpsertSession (update): %v", err)
	}
	if got := d.GetSessionDataVersion("dv-1"); got !=
		CurrentDataVersion() {
		t.Errorf(
			"after re-upsert, data_version = %d, want %d "+
				"(must be preserved across UpsertSession)",
			got, CurrentDataVersion(),
		)
	}
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
			if err := d.UpsertSession(s); err != nil {
				t.Fatalf("upsert: %v", err)
			}

			got, err := d.GetSession(ctx, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got == nil {
				t.Fatal("session not found")
			}

			if (got.TerminationStatus == nil) != (tc.val == nil) {
				t.Fatalf(
					"nil mismatch: got=%v want=%v",
					got.TerminationStatus, tc.val,
				)
			}
			if got.TerminationStatus != nil && *got.TerminationStatus != *tc.val {
				t.Fatalf(
					"value mismatch: got=%q want=%q",
					*got.TerminationStatus, *tc.val,
				)
			}
		})
	}
}
