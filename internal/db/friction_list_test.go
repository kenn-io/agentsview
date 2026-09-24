package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedFrictionListFixture(t *testing.T, d *DB) {
	t.Helper()
	ctx := t.Context()
	for _, id := range []string{"claude:s1", "claude:s2", "claude:gone"} {
		insertSession(t, d, id, "proj")
	}
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	finding := func(sid, kind, detector, fp string, seq int, ord *int) FrictionFinding {
		return FrictionFinding{
			SessionID: sid, Kind: kind, Detector: detector,
			MessageOrdinal: ord, Title: "[friction/" + kind + "] " + sid,
			Fingerprint: fp, OccurredAt: &at, Seq: seq,
			RulesVersion: "friction-v1",
		}
	}
	require.NoError(t, d.ReplaceSessionFriction(ctx, "claude:s1", []FrictionFinding{
		finding("claude:s1", "correction", "correction.coding", "fl1:aaa", 0, new(3)),
		finding("claude:s1", "error", "error", "fl1:bbb", 1, new(5)),
	}, nil, "friction-v1", "hash-s1"))
	require.NoError(t, d.ReplaceSessionFriction(ctx, "claude:s2", []FrictionFinding{
		finding("claude:s2", "pattern", "pattern.retry_loop", "fl1:ccc", 0, nil),
	}, nil, "friction-v1", "hash-s2"))
	require.NoError(t, d.ReplaceSessionFriction(ctx, "claude:gone", []FrictionFinding{
		finding("claude:gone", "error", "error", "fl1:bbb", 0, new(1)),
	}, nil, "friction-v1", "hash-gone"))
	require.NoError(t, d.SoftDeleteSession(ctx, "claude:gone"))

	built := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
	save := func(date, subject string, patterns []FrictionPatternUpdate) {
		require.NoError(t, d.SaveFrictionDigest(ctx, FrictionDigest{
			Date: date, Timezone: "UTC", RulesVersion: "friction-v1",
			BuiltAt: built, Revision: 1, SessionsScanned: 1,
			SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}\n"),
			Markdown:       []byte("# Friction Log — " + date + "\n"),
			MarkdownSHA256: "sha-" + date, RunID: "run-" + date,
		}, []FrictionDigestSubject{{SubjectID: subject, Date: date, SubjectKind: "session"}}, patterns))
	}
	save("2026-09-13", "claude:s2", []FrictionPatternUpdate{
		{Fingerprint: "fl1:ccc", Kind: "pattern", Title: "ccc", Date: "2026-09-13", SubjectID: "claude:s2", Occurrences: 1},
	})
	save("2026-09-14", "claude:s1", []FrictionPatternUpdate{
		{Fingerprint: "fl1:aaa", Kind: "correction", Title: "aaa", Date: "2026-09-14", SubjectID: "claude:s1", Ordinal: new(3), Occurrences: 1},
		{Fingerprint: "fl1:bbb", Kind: "error", Title: "bbb", Date: "2026-09-14", SubjectID: "claude:s1", Ordinal: new(5), Occurrences: 2},
	})
}

type findingKey struct {
	SessionID string
	Seq       int
}

func findingKeys(rows []FrictionFinding) []findingKey {
	out := make([]findingKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, findingKey{r.SessionID, r.Seq})
	}
	return out
}

func TestListFrictionFindings(t *testing.T) {
	d := testDB(t)
	seedFrictionListFixture(t, d)
	tests := []struct {
		name       string
		filter     FrictionFindingFilter
		want       []findingKey
		wantCursor string
	}{
		{
			"all_hides_soft_deleted",
			FrictionFindingFilter{},
			[]findingKey{{"claude:s1", 0}, {"claude:s1", 1}, {"claude:s2", 0}},
			"",
		},
		{"kind", FrictionFindingFilter{Kind: "error"}, []findingKey{{"claude:s1", 1}}, ""},
		{"session", FrictionFindingFilter{SessionID: "claude:s2"}, []findingKey{{"claude:s2", 0}}, ""},
		{"fingerprint", FrictionFindingFilter{Fingerprint: "fl1:bbb"}, []findingKey{{"claude:s1", 1}}, ""},
		{
			"digest_date",
			FrictionFindingFilter{Date: "2026-09-14"},
			[]findingKey{{"claude:s1", 0}, {"claude:s1", 1}},
			"",
		},
		{"digest_date_other", FrictionFindingFilter{Date: "2026-09-13"}, []findingKey{{"claude:s2", 0}}, ""},
		{
			"page_one",
			FrictionFindingFilter{Limit: 2},
			[]findingKey{{"claude:s1", 0}, {"claude:s1", 1}},
			"2",
		},
		{"page_two", FrictionFindingFilter{Limit: 2, Cursor: "2"}, []findingKey{{"claude:s2", 0}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, next, err := d.ListFrictionFindings(t.Context(), tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.want, findingKeys(rows))
			assert.Equal(t, tt.wantCursor, next)
		})
	}
	t.Run("fields_round_trip", func(t *testing.T) {
		rows, _, err := d.ListFrictionFindings(t.Context(), FrictionFindingFilter{SessionID: "claude:s1", Kind: "correction"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "correction.coding", rows[0].Detector)
		assert.Equal(t, "fl1:aaa", rows[0].Fingerprint)
		require.NotNil(t, rows[0].MessageOrdinal)
		assert.Equal(t, 3, *rows[0].MessageOrdinal)
		require.NotNil(t, rows[0].OccurredAt)
		assert.True(t, rows[0].OccurredAt.Equal(time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)))
	})
	for _, bad := range []string{"x", "-1", "1.5"} {
		t.Run("bad_cursor_"+bad, func(t *testing.T) {
			_, _, err := d.ListFrictionFindings(t.Context(), FrictionFindingFilter{Cursor: bad})
			require.ErrorIs(t, err, ErrInvalidCursor)
		})
	}
}

func TestListFrictionDigests(t *testing.T) {
	d := testDB(t)
	seedFrictionListFixture(t, d)
	tests := []struct {
		name     string
		from, to string
		want     []string
	}{
		{"all_newest_first", "", "", []string{"2026-09-14", "2026-09-13"}},
		{"from", "2026-09-14", "", []string{"2026-09-14"}},
		{"to", "", "2026-09-13", []string{"2026-09-13"}},
		{"empty_range", "2026-10-01", "2026-10-02", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := d.ListFrictionDigests(t.Context(), tt.from, tt.to)
			require.NoError(t, err)
			dates := make([]string, 0, len(rows))
			for _, r := range rows {
				dates = append(dates, r.Date)
				assert.Nil(t, r.SnapshotJSON)
				assert.Nil(t, r.SummaryJSON)
				assert.Nil(t, r.Markdown)
				assert.Equal(t, "UTC", r.Timezone)
				assert.Equal(t, 1, r.Revision)
			}
			assert.Equal(t, tt.want, dates)
		})
	}
}

func TestListFrictionPatterns(t *testing.T) {
	d := testDB(t)
	seedFrictionListFixture(t, d)
	tests := []struct {
		name       string
		filter     FrictionPatternFilter
		want       []string
		wantCursor string
	}{
		{"ranked", FrictionPatternFilter{}, []string{"fl1:bbb", "fl1:aaa", "fl1:ccc"}, ""},
		{"kind", FrictionPatternFilter{Kind: "pattern"}, []string{"fl1:ccc"}, ""},
		{"since", FrictionPatternFilter{Since: "2026-09-14"}, []string{"fl1:bbb", "fl1:aaa"}, ""},
		{"linked_is_empty_before_links_exist", FrictionPatternFilter{LinkState: FrictionLinkStateLinked}, []string{}, ""},
		{"unlinked_is_everything", FrictionPatternFilter{LinkState: FrictionLinkStateUnlinked}, []string{"fl1:bbb", "fl1:aaa", "fl1:ccc"}, ""},
		{"page", FrictionPatternFilter{Limit: 1}, []string{"fl1:bbb"}, "1"},
		{"page_two", FrictionPatternFilter{Limit: 2, Cursor: "1"}, []string{"fl1:aaa", "fl1:ccc"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, next, err := d.ListFrictionPatterns(t.Context(), tt.filter)
			require.NoError(t, err)
			got := make([]string, 0, len(rows))
			for _, r := range rows {
				got = append(got, r.Fingerprint)
			}
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantCursor, next)
		})
	}
	t.Run("fields", func(t *testing.T) {
		rows, _, err := d.ListFrictionPatterns(t.Context(), FrictionPatternFilter{Kind: "error"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "2026-09-14", rows[0].FirstSeenDate)
		assert.Equal(t, "2026-09-14", rows[0].LastSeenDate)
		assert.Equal(t, 2, rows[0].OccurrenceCount)
		assert.Equal(t, "claude:s1", rows[0].LastSubjectID)
		require.NotNil(t, rows[0].LastOrdinal)
		assert.Equal(t, 5, *rows[0].LastOrdinal)
	})
}

func TestFrictionListHelpers(t *testing.T) {
	limits := []struct{ in, want int }{{0, 100}, {-3, 100}, {50, 50}, {1000, 1000}, {1001, 100}}
	for _, tt := range limits {
		assert.Equal(t, tt.want, NormalizeFrictionLimit(tt.in), "limit %d", tt.in)
	}
	cursors := []struct {
		in   string
		want int
		ok   bool
	}{{"", 0, true}, {"0", 0, true}, {"25", 25, true}, {"-1", 0, false}, {"abc", 0, false}}
	for _, tt := range cursors {
		got, err := DecodeFrictionCursor(tt.in)
		if tt.ok {
			require.NoError(t, err, tt.in)
			assert.Equal(t, tt.want, got)
			continue
		}
		require.ErrorIs(t, err, ErrInvalidCursor, tt.in)
	}
	assert.Empty(t, EncodeFrictionCursor(0))
	assert.Equal(t, "40", EncodeFrictionCursor(40))
	assert.False(t, FrictionLinkFilterExcludesAll(""))
	assert.False(t, FrictionLinkFilterExcludesAll(FrictionLinkStateUnlinked))
	assert.True(t, FrictionLinkFilterExcludesAll(FrictionLinkStateLinked))
	assert.True(t, FrictionLinkFilterExcludesAll("pending"))
}
