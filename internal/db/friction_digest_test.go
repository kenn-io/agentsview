package db

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
)

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
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), id, nil, nil, "friction-v1", "h-"+id))
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
				seedFrictionSession(t, d, "a", "2026-09-14T23:00:00Z", "2026-09-15T00:30:00Z", nil)
				seedFrictionSession(t, d, "b", "2026-09-15T10:00:00Z", "2026-09-16T00:00:00Z", nil)
			},
			want: []string{"a"},
		},
		{
			name: "started_at_only",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				seedFrictionSession(t, d, "open", "2026-09-15T08:00:00Z", "", nil)
			},
			want: []string{"open"},
		},
		{
			name: "timestamp with offset crosses UTC date",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				seedFrictionSession(t, d, "offset", "", "2026-09-16T08:30:00+09:00", nil)
			},
			want: []string{"offset"},
		},
		{
			name: "dst_short_day_new_york",
			loc:  ny, date: "2026-03-08",
			seed: func(t *testing.T, d *DB) {
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
				seedFrictionSession(t, d, "gone", "", "2026-09-15T01:00:00Z", nil)
				require.NoError(t, d.SoftDeleteSession(t.Context(), "gone"))
			},
			want: []string{},
		},
		{
			name: "review excluded sessions excluded",
			loc:  time.UTC, date: "2026-09-15",
			seed: func(t *testing.T, d *DB) {
				seedFrictionSession(t, d, "excluded", "", "2026-09-15T01:00:00Z", nil)
				require.NoError(t, d.ReplaceSessionFriction(t.Context(), "excluded", nil,
					&FrictionSessionDims{SessionID: "excluded", ReviewExcluded: true}, "friction-v1", "h2"))
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

func TestFrictionSubjectsCarryDimsAndRole(t *testing.T) {
	d := testDB(t)
	parent := "root"
	seedFrictionSession(t, d, "root", "", "2026-09-15T01:00:00Z", nil)
	seedFrictionSession(t, d, "child", "", "2026-09-15T02:00:00Z", func(s *Session) {
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
		fp := "archive/profiles/seat-02/projects/x.jsonl"
		s.FilePath = &fp
	})
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), "child", nil,
		&FrictionSessionDims{SessionID: "child", Seat: "seat-02", Persona: "helper", Channel: "general", DimsSource: "nanoclaw+seat_pattern"},
		"friction-v1", "h2"))
	got, err := d.FrictionSubjectsForDate(t.Context(), "2026-09-15", time.UTC, false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	byID := map[string]FrictionSubject{got[0].SubjectID: got[0], got[1].SubjectID: got[1]}
	child := byID["child"]
	assert.True(t, child.IsSubAgent)
	assert.Equal(t, friction.SubjectSession, child.SubjectKind)
	assert.Equal(t, "seat-02", child.Dims.Seat)
	assert.Equal(t, "helper", child.Dims.Persona)
	assert.Equal(t, "general", child.Dims.Channel)
	assert.Equal(t, "friction-v1", child.RulesVersion)
	assert.Equal(t, "machine-a", child.Machine)
	assert.Equal(t, time.Date(2026, 9, 15, 2, 0, 0, 0, time.UTC), child.LastActivity)
	assert.False(t, byID["root"].IsSubAgent)
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
			want := testDigest("2026-09-15", 1)
			want.SessionsScanned = 3
			require.NoError(t, d.SaveFrictionDigest(t.Context(), want, nil, nil))
			got, err := d.GetFrictionDigest(t.Context(), "2026-09-15")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, want, *got)
		}},
		{name: "missing_date_is_nil", run: func(t *testing.T, d *DB) {
			got, err := d.GetFrictionDigest(t.Context(), "2026-01-01")
			require.NoError(t, err)
			assert.Nil(t, got)
		}},
		{name: "second_insert_conflicts", run: func(t *testing.T, d *DB) {
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil,
				[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-15", SubjectID: "s1", Occurrences: 2}}))
			err := d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), nil,
				[]FrictionPatternUpdate{{Fingerprint: "fl1:a", Kind: "error", Title: "t", Date: "2026-09-15", SubjectID: "s2", Occurrences: 5}})
			require.ErrorIs(t, err, ErrFrictionDigestConflict)
			assert.Equal(t, 2, frictionPatternCount(t, d, "fl1:a"), "rolled-back save must not count")
		}},
		{name: "revision_update_requires_previous", run: func(t *testing.T, d *DB) {
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
			subj := []FrictionDigestSubject{{SubjectID: "s1", Date: "2026-09-15", SubjectKind: friction.SubjectSession}}
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 1), subj, nil))
			require.NoError(t, d.SaveFrictionDigest(t.Context(), testDigest("2026-09-15", 2), subj, nil))
			assert.Equal(t, 1, countRows(t, d, "friction_digest_sessions"))
		}},
		{name: "patterns_accumulate_across_dates", run: func(t *testing.T, d *DB) {
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
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), "s1", []FrictionFinding{mk("s1", 1), mk("s1", 0)}, nil, "friction-v1", "h1"))
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), "s2", []FrictionFinding{mk("s2", 0)}, nil, "friction-v1", "h2"))
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
