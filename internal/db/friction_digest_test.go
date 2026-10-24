package db

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
)

func TestFrictionPendingDatesSeeksActivityWindow(t *testing.T) {
	for _, history := range []int{1, 1024} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			d := testDB(t)
			for i := range history {
				seedFrictionSession(t, d, "old-"+strconv.Itoa(i), "2000-01-01T12:00:00Z", "", nil)
			}
			seedFrictionSession(t, d, "recent", "2026-09-15T12:00:00Z", "", nil)
			if history > 1 {
				// Reopen an archive created before the activity index existed.
				_, err := d.getWriter().ExecContext(t.Context(), "DROP INDEX idx_sessions_friction_activity")
				require.NoError(t, err)
				path := d.Path()
				require.NoError(t, d.Close())
				d, err = OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
			}
			dates, err := d.FrictionPendingDates(t.Context(), "2026-09-15", "2026-09-15", time.UTC)
			require.NoError(t, err)
			assert.Equal(t, []string{"2026-09-15"}, dates)
			rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+frictionPendingDatesSQL,
				friction.SkippedRulesVersion, "2026-09-14T00:00:00Z", "2026-09-17T00:00:00Z")
			require.NoError(t, err)
			defer rows.Close()
			plan := strings.Join(explainQueryPlanDetails(t, rows), "\n")
			assert.Contains(t, plan, "SEARCH s ")
			assert.Contains(t, plan, "(<expr>>? AND <expr><?)", "history outside the window must not be scanned")
		})
	}
}

func seedFrictionSession(
	t *testing.T, d *DB, id, started, ended string, mutate func(*Session),
) {
	t.Helper()
	s := Session{ID: id, Project: "proj", Machine: "machine-a", Agent: "claude", MessageCount: 1}
	if started != "" {
		s.StartedAt = &started
	}
	if ended != "" {
		s.EndedAt = &ended
	}
	if mutate != nil {
		mutate(&s)
	}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.replaceSessionFriction(t.Context(), id, nil, "friction-v1", "h-"+id))
}

func subjectIDs(subjects []FrictionSubject) []string {
	out := make([]string, 0, len(subjects))
	for _, s := range subjects {
		out = append(out, s.SubjectID)
	}
	return out
}

func TestFrictionSubjectsForDate(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	tests := []struct {
		name            string
		loc             *time.Location
		date            string
		seed            func(t *testing.T, d *DB)
		includeDigested bool
		want            []string
	}{
		{
			name: "ended_at decides the date",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "a", "2026-09-14T23:00:00Z", "2026-09-15T00:30:00Z", nil)
				seedFrictionSession(t, d, "b", "2026-09-15T10:00:00Z", "2026-09-16T00:00:00Z", nil)
			},
			want: []string{"a"},
		},
		{
			name: "started_at_only",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "open", "2026-09-15T08:00:00Z", "", nil)
			},
			want: []string{"open"},
		},
		{
			name: "timestamp with offset crosses UTC date",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "offset", "", "2026-09-16T08:30:00+09:00", nil)
			},
			want: []string{"offset"},
		},
		{
			name: "dst_short_day_new_york",
			loc:  ny, date: "2026-03-08",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				// 23:30 EDT on 2026-03-08 is 03:30Z on 2026-03-09.
				seedFrictionSession(t, d, "late", "", "2026-03-09T03:30:00Z", nil)
				// 00:30 EDT on 2026-03-09 is 04:30Z: next local date.
				seedFrictionSession(t, d, "next", "", "2026-03-09T04:30:00Z", nil)
			},
			want: []string{"late"},
		},
		{
			name: "digested subjects excluded",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "done", "", "2026-09-15T01:00:00Z", nil)
				seedFrictionSession(t, d, "fresh", "", "2026-09-15T02:00:00Z", nil)
				require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1),
					[]FrictionDigestSubject{{SubjectID: "done", Date: "2026-09-15", SubjectKind: friction.SubjectSession}}, nil))
			},
			want: []string{"fresh"},
		},
		{
			name: "rebuild keeps membership even after activity moved",
			loc:  time.UTC, date: "2026-09-15", includeDigested: true,
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "moved", "", "2026-09-17T01:00:00Z", nil)
				seedFrictionSession(t, d, "fresh", "", "2026-09-15T02:00:00Z", nil)
				require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1),
					[]FrictionDigestSubject{{SubjectID: "moved", Date: "2026-09-15", SubjectKind: friction.SubjectSession}}, nil))
			},
			want: []string{"fresh", "moved"},
		},
		{
			name: "trashed sessions excluded",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "gone", "", "2026-09-15T01:00:00Z", nil)
				require.NoError(t, d.SoftDeleteSession(t.Context(), "gone"))
			},
			want: []string{},
		},
		{
			name: "review excluded sessions excluded",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				t.Helper()
				seedFrictionSession(t, d, "excluded", "", "2026-09-15T01:00:00Z", nil)
				require.NoError(t, d.replaceSessionFriction(t.Context(), "excluded", nil, friction.SkippedRulesVersion, "h2"))
			},
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			tt.seed(t, d)
			got, err := d.FrictionSubjectsForDate(t.Context(), tt.date, tt.loc, tt.includeDigested)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.want, subjectIDs(got))
		})
	}
}

func TestFrictionSubjectsCarryPathAndRole(t *testing.T) {
	d := testDB(t)
	parent := "root"
	seedFrictionSession(t, d, "root", "", "2026-09-15T01:00:00Z", nil)
	seedFrictionSession(t, d, "child", "", "2026-09-15T02:00:00Z", func(s *Session) {
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
		fp := "archive/profiles/seat-02/projects/x.jsonl"
		s.FilePath = &fp
	})
	require.NoError(t, d.replaceSessionFriction(t.Context(), "child", nil, "friction-v1", "h2"))
	got, err := d.FrictionSubjectsForDate(t.Context(), "2026-09-15", time.UTC, false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	byID := map[string]FrictionSubject{got[0].SubjectID: got[0], got[1].SubjectID: got[1]}
	child := byID["child"]
	assert.True(t, child.IsSubAgent)
	assert.Equal(t, "archive/profiles/seat-02/projects/x.jsonl", child.FilePath)
	assert.Equal(t, friction.SubjectSession, child.SubjectKind)
	assert.Equal(t, "friction-v1", child.RulesVersion)
	assert.Equal(t, "machine-a", child.Machine)
	assert.Equal(t, time.Date(2026, 9, 15, 2, 0, 0, 0, time.UTC), child.LastActivity)
	assert.False(t, byID["root"].IsSubAgent)
}

func TestFrictionSubjectsIdentifyPriorMembership(t *testing.T) {
	d := testDB(t)
	seedFrictionSession(t, d, "old", "", "2026-09-15T01:00:00Z", nil)
	seedFrictionSession(t, d, "new", "", "2026-09-15T02:00:00Z", nil)
	require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1),
		[]FrictionDigestSubject{{SubjectID: "old", Date: "2026-09-15", SubjectKind: friction.SubjectSession}}, nil))
	got, err := d.FrictionSubjectsForDate(t.Context(), "2026-09-15", time.UTC, true)
	require.NoError(t, err)
	require.Len(t, got, 2)
	byID := map[string]FrictionSubject{got[0].SubjectID: got[0], got[1].SubjectID: got[1]}
	assert.True(t, byID["old"].AlreadyDigested)
	assert.False(t, byID["new"].AlreadyDigested)
}

func testDigest(date string, revision int) FrictionDigest {
	md := []byte("# Friction Log — " + date + "\n")
	sum := sha256.Sum256(md)
	return FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC), Revision: revision,
		SnapshotJSON: []byte(`{"date":"` + date + `"}`), SummaryJSON: []byte("{}\n"),
		Markdown: md, MarkdownSHA256: hex.EncodeToString(sum[:]), RunID: "run-" + date,
	}
}

func TestSaveFrictionDigest(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, d *DB)
	}{
		{name: "round_trip", run: func(t *testing.T, d *DB) {
			t.Helper()
			want := testDigest("2026-09-15", 1)
			want.SessionsScanned = 3
			require.NoError(t, d.SaveFrictionDigest(t.Context(), want, nil, nil))
			got, err := d.GetFrictionDigest(t.Context(), "2026-09-15")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, want, *got)
		}},
		{name: "missing_date_is_nil", run: func(t *testing.T, d *DB) {
			t.Helper()
			got, err := d.GetFrictionDigest(t.Context(), "2026-01-01")
			require.NoError(t, err)
			assert.Nil(t, got)
		}},
		{name: "second_insert_conflicts", run: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil,
				[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-15", SubjectID: "s1", Occurrences: 2}}))
			err := d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil,
				[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-15", SubjectID: "s2", Occurrences: 5}})
			require.ErrorIs(t, err, ErrFrictionDigestConflict)
			assert.Equal(t, 2, frictionPatternCount(t, d, "fl1:a"), "rolled-back save must not count")
		}},
		{name: "revision_update_requires_previous", run: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil, nil))
			require.ErrorIs(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 3), nil, nil), ErrFrictionDigestConflict)
			second := testDigest("2026-09-15", 2)
			second.SessionsScanned = 9
			require.NoError(t, d.SaveFrictionDigest(t.Context(), second, nil, nil))
			got, err := d.GetFrictionDigest(t.Context(), "2026-09-15")
			require.NoError(t, err)
			assert.Equal(t, 2, got.Revision)
			assert.Equal(t, 9, got.SessionsScanned)
		}},
		{name: "subjects_are_recorded_once", run: func(t *testing.T, d *DB) {
			t.Helper()
			subj := []FrictionDigestSubject{{SubjectID: "s1", Date: "2026-09-15", SubjectKind: friction.SubjectSession}}
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), subj, nil))
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 2), subj, nil))
			assert.Equal(t, 1, countRows(t, d, "friction_digest_sessions"))
		}},
		{name: "patterns_accumulate_across_dates", run: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil, []FrictionPatternUpdate{
				{Fingerprint: "fl1:a", Kind: "correction", Title: "t", Date: "2026-09-15", SubjectID: "s1", Ordinal: Ptr(4), Occurrences: 2},
				{Fingerprint: "fl1:a", Kind: "correction", Title: "t", Date: "2026-09-15", SubjectID: "s2", Ordinal: Ptr(7), Occurrences: 1},
			}))
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-14", 1), nil, []FrictionPatternUpdate{
				{Fingerprint: "fl1:a", Kind: "correction", Title: "t", Date: "2026-09-14", SubjectID: "s0", Ordinal: Ptr(1), Occurrences: 1},
			}))
			row := readFrictionPattern(t, d, "fl1:a")
			assert.Equal(t, frictionPatternRow{
				FirstSeen: "2026-09-14", LastSeen: "2026-09-15", Occurrences: 4,
				Sessions: 3, LastSubject: "s2", LastOrdinal: Ptr(7),
			}, row)
		}},
		{name: "latest_date", run: func(t *testing.T, d *DB) {
			t.Helper()
			latest, err := d.LatestFrictionDigestDate(t.Context())
			require.NoError(t, err)
			assert.Empty(t, latest)
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-14", 1), nil, nil))
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-16", 1), nil, nil))
			latest, err = d.LatestFrictionDigestDate(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "2026-09-16", latest)
		}},
		{name: "update_render_bumps_revision", run: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil, nil))
			require.NoError(t, d.UpdateFrictionDigestRender(t.Context(), "2026-09-15", []byte("new md\n"), []byte("{\"x\":1}\n"), 2))
			got, err := d.GetFrictionDigest(t.Context(), "2026-09-15")
			require.NoError(t, err)
			sum := sha256.Sum256([]byte("new md\n"))
			assert.Equal(t, 2, got.Revision)
			assert.Equal(t, "new md\n", string(got.Markdown))
			assert.Equal(t, hex.EncodeToString(sum[:]), got.MarkdownSHA256)
			require.ErrorIs(t, d.UpdateFrictionDigestRender(t.Context(), "2026-09-15", []byte("x"), []byte("{}"), 2), ErrFrictionDigestConflict)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, testDB(t)) })
	}
}

func TestEarliestSessionDate(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	d := testDB(t)
	got, err := d.EarliestSessionDate(t.Context(), time.UTC)
	require.NoError(t, err)
	assert.Empty(t, got)
	seedFrictionSession(t, d, "a", "2026-09-10T20:00:00Z", "2026-09-10T20:30:00Z", nil)
	seedFrictionSession(t, d, "b", "2026-09-12T01:00:00Z", "", nil)
	got, err = d.EarliestSessionDate(t.Context(), time.UTC)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-10", got)
	got, err = d.EarliestSessionDate(t.Context(), tokyo)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-11", got, "20:30Z is 05:30 next day in Tokyo")
}

func TestEarliestSessionDateUsesInstantAcrossOffsets(t *testing.T) {
	d := testDB(t)
	seedFrictionSession(t, d, "earlier", "", "2026-09-16T08:30:00+09:00", nil)
	seedFrictionSession(t, d, "later", "", "2026-09-16T00:00:00Z", nil)
	got, err := d.EarliestSessionDate(t.Context(), time.UTC)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-15", got)
}

func TestFrictionActivityTimesIgnoreInvalidRows(t *testing.T) {
	for _, invalid := range []string{"", "not-a-time", "2000-02-30T12:00:00Z", "2026-09-15invalid"} {
		t.Run(invalid, func(t *testing.T) {
			d := testDB(t)
			seedFrictionSession(t, d, "invalid", "", "", func(s *Session) {
				s.StartedAt, s.EndedAt = new(invalid), new(invalid)
			})
			got, err := d.EarliestSessionDate(t.Context(), time.UTC)
			require.NoError(t, err)
			assert.Empty(t, got, "an archive without usable activity times has no first date")
			seedFrictionSession(t, d, "fallback", "2026-09-16T08:30:00+09:00", "", func(s *Session) { s.EndedAt = new("") })
			seedFrictionSession(t, d, "ended", "2026-09-01T00:00:00Z", "2026-09-16T12:00:00Z", nil)
			got, err = d.EarliestSessionDate(t.Context(), time.UTC)
			require.NoError(t, err)
			assert.Equal(t, "2026-09-15", got)
			dates, err := d.FrictionPendingDates(t.Context(), "2026-09-01", "2026-09-21", time.UTC)
			require.NoError(t, err)
			assert.Equal(t, []string{"2026-09-15", "2026-09-16"}, dates)
			for day, id := range map[string]string{"2026-09-15": "fallback", "2026-09-16": "ended"} {
				subjects, err := d.FrictionSubjectsForDate(t.Context(), day, time.UTC, false)
				require.NoError(t, err)
				assert.Equal(t, []string{id}, subjectIDs(subjects))
			}
		})
	}
}

func TestFrictionMembershipConflictRollsBack(t *testing.T) {
	for _, revision := range []int{1, 2} {
		t.Run(strconv.Itoa(revision), func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1),
				[]FrictionDigestSubject{{SubjectID: "claimed", Date: "2026-09-15", SubjectKind: friction.SubjectSession}},
				[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-15", SubjectID: "claimed", Occurrences: 2}}))
			if revision == 2 {
				require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-16", 1), nil, nil))
			}
			before, err := d.GetFrictionDigest(t.Context(), "2026-09-16")
			require.NoError(t, err)
			err = d.SaveFrictionDigest(t.Context(), testDigest("2026-09-16", revision),
				[]FrictionDigestSubject{
					{SubjectID: "fresh", Date: "2026-09-16", SubjectKind: friction.SubjectSession},
					{SubjectID: "claimed", Date: "2026-09-16", SubjectKind: friction.SubjectSession},
				}, []FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-16", SubjectID: "claimed", Occurrences: 5}})
			require.ErrorIs(t, err, ErrFrictionMembershipConflict)
			after, err := d.GetFrictionDigest(t.Context(), "2026-09-16")
			require.NoError(t, err)
			assert.Equal(t, before, after, "a losing save must leave the digest unchanged")
			assert.Equal(t, 1, countRows(t, d, "friction_digest_sessions"), "the earlier fresh membership must roll back too")
			assert.Equal(t, 2, frictionPatternCount(t, d, "fl1:a"))
		})
	}
}

func TestFrictionFindingsForSubjects(t *testing.T) {
	d := testDB(t)
	seedFrictionSession(t, d, "s1", "", "2026-09-15T01:00:00Z", nil)
	seedFrictionSession(t, d, "s2", "", "2026-09-15T02:00:00Z", nil)
	occurred := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
	mk := func(sid string, seq int) FrictionFinding {
		return FrictionFinding{
			SessionID: sid, Kind: "workaround", Detector: "workaround", MessageOrdinal: Ptr(seq),
			Label: "for now", Text: "for now", Title: "[friction/workaround] for now: for now",
			Fingerprint: "fl1:x", OccurredAt: &occurred, Seq: seq, RulesVersion: "friction-v1",
		}
	}
	require.NoError(t, d.replaceSessionFriction(t.Context(), "s1", []FrictionFinding{mk("s1", 1), mk("s1", 0)}, "friction-v1", "h1"))
	require.NoError(t, d.replaceSessionFriction(t.Context(), "s2", []FrictionFinding{mk("s2", 0)}, "friction-v1", "h2"))
	got, err := d.FrictionFindingsForSubjects(t.Context(), []string{"s2", "s1", "missing"})
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []int{0, 1}, []int{got[0].Seq, got[1].Seq}, "s1 rows ordered by seq")
	assert.Equal(t, "s1", got[0].SessionID)
	assert.Equal(t, occurred, *got[0].OccurredAt)
	empty, err := d.FrictionFindingsForSubjects(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

type frictionPatternRow struct {
	FirstSeen, LastSeen string
	Occurrences         int
	Sessions            int
	LastSubject         string
	LastOrdinal         *int
}

func readFrictionPattern(t *testing.T, d *DB, fp string) frictionPatternRow {
	t.Helper()
	var r frictionPatternRow
	var ord *int64
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `
		SELECT first_seen_date, last_seen_date, occurrence_count, session_count,
		       last_subject_id, last_ordinal
		FROM friction_patterns WHERE fingerprint = ?`, fp,
	).Scan(&r.FirstSeen, &r.LastSeen, &r.Occurrences, &r.Sessions, &r.LastSubject, &ord))
	if ord != nil {
		r.LastOrdinal = Ptr(int(*ord))
	}
	return r
}

func frictionPatternCount(t *testing.T, d *DB, fp string) int {
	t.Helper()
	return readFrictionPattern(t, d, fp).Occurrences
}

func countRows(t *testing.T, d *DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n))
	return n
}

func TestCopyFrictionStateFrom(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src := testDBAtPath(t, srcPath, "src")
	require.NoError(t, src.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1),
		[]FrictionDigestSubject{{SubjectID: "s1", Date: "2026-09-15", SubjectKind: friction.SubjectSession}},
		[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-15", SubjectID: "s1", Occurrences: 3}}))
	src.Close()

	dst := testDBAtPath(t, filepath.Join(dir, "dst.db"), "dst")
	defer dst.Close()
	require.NoError(t, dst.CopyFrictionStateFrom(srcPath))
	require.NoError(t, dst.CopyFrictionStateFrom(srcPath), "copy is idempotent")

	got, err := dst.GetFrictionDigest(t.Context(), "2026-09-15")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, testDigest("2026-09-15", 1), *got)
	assert.Equal(t, 1, countRows(t, dst, "friction_digest_sessions"))
	assert.Equal(t, 3, frictionPatternCount(t, dst, "fl1:a"))
}

func TestCopyFrictionStateFromOldArchive(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "old")
	_, err := src.getWriter().Exec(t.Context(), `
		DROP TABLE friction_digests;
		DROP TABLE friction_digest_sessions;
		DROP TABLE friction_patterns;`)
	require.NoError(t, err)
	src.Close()
	dst := testDBAtPath(t, filepath.Join(dir, "dst.db"), "dst")
	defer dst.Close()
	require.NoError(t, dst.CopyFrictionStateFrom(srcPath))
	assert.Equal(t, 0, countRows(t, dst, "friction_digests"))
}

func TestCopyFrictionStateFromRejectsUsageOnly(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src := testDBAtPath(t, srcPath, "src")
	require.NoError(t, src.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil, nil))
	require.NoError(t, src.Close())

	dst := testDBAtPath(t, filepath.Join(dir, "dst.db"), "dst")
	t.Cleanup(func() { require.NoError(t, dst.Close()) })
	dst.SetArchiveContent(config.ArchiveContentUsage)
	require.ErrorIs(t, dst.CopyFrictionStateFrom(srcPath), ErrArchiveContentExcluded)
	assert.Equal(t, 0, countRows(t, dst, "friction_digests"))
}

func TestFrictionDigestWritesRespectArchiveContent(t *testing.T) {
	d := testDB(t)
	original := testDigest("2026-09-15", 1)
	require.NoError(t, d.SaveFrictionDigest(t.Context(), original, nil, nil))
	d.SetArchiveContent(config.ArchiveContentUsage)

	err := d.SaveFrictionDigest(t.Context(), testDigest("2026-09-16", 1),
		[]FrictionDigestSubject{{SubjectID: "s", Date: "2026-09-16", SubjectKind: friction.SubjectSession}},
		[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "transcript text", Date: "2026-09-16", SubjectID: "s", Occurrences: 1}})
	require.ErrorIs(t, err, ErrArchiveContentExcluded)
	assert.Equal(t, 1, countRows(t, d, "friction_digests"))
	assert.Zero(t, countRows(t, d, "friction_digest_sessions"))
	assert.Zero(t, countRows(t, d, "friction_patterns"))

	require.ErrorIs(t, d.UpdateFrictionDigestRender(t.Context(), original.Date,
		[]byte("new transcript text"), []byte("{}"), 2), ErrArchiveContentExcluded)
	got, err := d.GetFrictionDigest(t.Context(), original.Date)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, original, *got, "rejected writes leave the stored digest unchanged")
}

func TestFrictionDayBounds(t *testing.T) {
	for _, tt := range []struct{ zone, date, start, end string }{
		{"America/Santiago", "2026-09-05", "2026-09-05T04:00:00Z", "2026-09-06T04:00:00Z"},
		{"America/Santiago", "2026-09-06", "2026-09-06T04:00:00Z", "2026-09-07T03:00:00Z"},
		{"America/Havana", "2026-11-01", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z"},
		{"Pacific/Apia", "2011-12-30", "2011-12-30T10:00:00Z", "2011-12-30T10:00:00Z"},
	} {
		t.Run(tt.zone+"/"+tt.date, func(t *testing.T) {
			loc, err := time.LoadLocation(tt.zone)
			require.NoError(t, err)
			start, end, err := FrictionDayBounds(tt.date, loc)
			require.NoError(t, err)
			assert.Equal(t, tt.start, start.UTC().Format(time.RFC3339))
			assert.Equal(t, tt.end, end.UTC().Format(time.RFC3339))
		})
	}
}

func TestFrictionSubjectsAcrossMissingMidnight(t *testing.T) {
	d := testDB(t)
	loc, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	for _, s := range []struct{ id, ended string }{
		{"before", "2026-09-06T03:59:59Z"},
		{"first", "2026-09-06T04:00:00Z"},
		{"late", "2026-09-07T02:59:59Z"},
		{"after", "2026-09-07T03:00:00Z"},
	} {
		seedFrictionSession(t, d, s.id, "", s.ended, nil)
	}
	got, err := d.FrictionSubjectsForDate(t.Context(), "2026-09-06", loc, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"first", "late"}, subjectIDs(got))
}

func TestFrictionSubjectsSeekActivityAndMembership(t *testing.T) {
	for _, history := range []int{1, 1024} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			d := testDB(t)
			for i := range history {
				seedFrictionSession(t, d, "old-"+strconv.Itoa(i), "2000-01-01T12:00:00Z", "", nil)
			}
			seedFrictionSession(t, d, "late", "2026-09-15T12:00:00Z", "", nil)
			seedFrictionSession(t, d, "member", "2026-09-21T12:00:00Z", "", nil)
			_, err := d.getWriter().ExecContext(t.Context(), `INSERT INTO friction_digest_sessions(subject_id, subject_kind, date)
    VALUES ('member', 'session', '2026-09-15')`)
			require.NoError(t, err)
			for _, rebuild := range []bool{false, true} {
				subjects, err := d.FrictionSubjectsForDate(t.Context(), "2026-09-15", time.UTC, rebuild)
				require.NoError(t, err)
				want := []string{"late"}
				if rebuild {
					want = append(want, "member")
				}
				assert.ElementsMatch(t, want, subjectIDs(subjects))
				rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+frictionSubjectsSQL,
					friction.SkippedRulesVersion, "2026-09-14T00:00:00Z", "2026-09-17T00:00:00Z", boolInt(rebuild), "2026-09-15")
				require.NoError(t, err)
				defer rows.Close()
				plan := strings.Join(explainQueryPlanDetails(t, rows), "\n")
				assert.NotContains(t, plan, "SCAN s", "subject selection must not scan unchanged archive history")
				assert.Contains(t, plan, "(<expr>>? AND <expr><?)")
				assert.Contains(t, plan, "idx_friction_digest_sessions_date (date=?)")
			}
		})
	}
}
